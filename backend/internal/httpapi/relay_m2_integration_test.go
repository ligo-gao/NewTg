//go:build integration

package httpapi

// W7/M2 集成验收（真实 PostgreSQL + Redis + 真实计费引擎）：
//
//	1. 生产接缝 store.TokenRepo.Resolve 的 auto 投影 + RelayAuth 白名单（R-67）；
//	2. 历史空列表 auto 令牌请求期 403 auto_groups_not_enabled（R-61/I-20）；
//	3. 跨组重试的计费按**最终分组**倍率结算（R-65 / TC-F-02-14-03）；
//	4. 切换目标组倍率缺失 → 403 group_ratio_missing、零上游、预扣全额退回（R-64）；
//	5. 首组空候选时预扣估算跟随切组后的分组（R-62/R-64 的资损面）。
//
// 运行（隔离库，绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_m2 \
//	NEWTG_TEST_LOGS_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_m2_logs \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationM2 -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 装置
// ---------------------------------------------------------------------------

func m2TestDBs(t *testing.T) (*sql.DB, *sql.DB, *gorm.DB) {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if mainDSN == "" || logsDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN 未设置，跳过 M2 集成测试")
	}
	mainDB, err := sql.Open("pgx", mainDSN)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	if err := mainDB.Ping(); err != nil {
		_ = mainDB.Close()
		t.Fatalf("ping main db: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })

	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := logsDB.Ping(); err != nil {
		_ = logsDB.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: mainDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	return mainDB, logsDB, gormDB
}

func m2TestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:56379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func m2Suffix() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func m2SeedGroup(t *testing.T, db *sql.DB, name string, ratio int, status int16) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, status)
		VALUES ($1, $2, true, $3)`, name, ratio, status); err != nil {
		t.Fatalf("seed group %s: %v", name, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM user_groups WHERE name = $1`, name) })
}

func m2SeedUser(t *testing.T, db *sql.DB, group string, balance int64) int64 {
	t.Helper()
	var userID int64
	if err := db.QueryRow(`INSERT INTO users (username, "group", balance)
		VALUES ($1, $2, $3) RETURNING id`, "m2_user_"+m2Suffix(), group, balance).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM api_tokens WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})
	return userID
}

// m2SeedToken 直接插行（绕过 tokenstore 的写入期校验），以便构造历史
// 「group=auto + 空 auto_groups」等 M1 迁移遗留状态。
func m2SeedToken(t *testing.T, db *sql.DB, userID int64, group, autoGroupsJSON string,
	retry bool, modelLimits, allowIPs *string,
) (int64, string) {
	t.Helper()
	plain := fmt.Sprintf("nkt-%048d", time.Now().UnixNano())
	var tokenID int64
	if err := db.QueryRow(`INSERT INTO api_tokens
		(user_id, name, key_hash, "group", remain_quota, model_limits, allow_ips, auto_groups, cross_group_retry)
		VALUES ($1, 'm2-token', $2, $3, NULL, $4::jsonb, $5::jsonb, $6, $7) RETURNING id`,
		userID, auth.HashTokenKey(plain), group, modelLimits, allowIPs, autoGroupsJSON, retry).Scan(&tokenID); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return tokenID, plain
}

func m2SeedChannel(t *testing.T, db *sql.DB, sealer *SecretSealer, name, group, upstreamURL, model string) int64 {
	t.Helper()
	sealed, err := sealer.Seal([]byte(`{"api_key":"m2-upstream-key"}`))
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO channels
		(name, type, base_url, credentials, supported_models, group_tags, model_mapping, weight, priority, status)
		VALUES ($1, 'openai', $2, $3, $4::jsonb, $5::jsonb, '{}'::jsonb, 1, 10, 1) RETURNING id`,
		name, upstreamURL, sealed,
		`["`+model+`"]`, `["`+group+`"]`).Scan(&id); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

func m2SeedModel(t *testing.T, db *sql.DB, model string, price int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		VALUES ($1, 1, $2, 'm2', 1)`, model, price); err != nil {
		t.Fatalf("seed model_pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model = $1`, model)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})
}

// m2BillingConfig 是真实计费引擎的最小配置：禁用信任旁路，预扣比例 1.0。
type m2BillingConfig map[string]json.RawMessage

func (m m2BillingConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	return value, ok
}

func m2NewEngine(t *testing.T, gormDB *gorm.DB, logsDB *sql.DB, rdb *redis.Client) *billing.Engine {
	t.Helper()
	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	return billing.NewEngine(gormDB, logsDB, rdb, m2BillingConfig{
		billing.ConfigPreConsumeRatio: json.RawMessage("1.0"),
		billing.ConfigTrustQuotaUSD:   json.RawMessage("0"),
	}, source, billing.NewRegistry(rdb), billing.NewDBLogger(logsDB))
}

// m2NewHandler 走生产接缝：store.TokenRepo（令牌 + 用户状态）与
// store.ChannelRepo（候选/渠道）都是真实实现。
func m2NewHandler(t *testing.T, mainDB *sql.DB, engine *billing.Engine) *RelayHandler {
	t.Helper()
	tokenRepo := store.NewTokenRepo(mainDB)
	pricingRepo := store.NewPricingRepo(mainDB)
	opts := RelayHandlerOptions{
		Channels:        store.NewChannelRepo(mainDB),
		Tokens:          tokenRepo,
		Access:          tokenRepo,
		Sealer:          NewSecretSealer(relaySealerMaster),
		ModelStatus:     pricingRepo,
		GroupConfigured: pricingRepo.GroupConfigured,
		Client:          &http.Client{Timeout: 10 * time.Second},
	}
	if engine != nil {
		opts.Billing = engine.NewSession
	}
	return NewRelayHandler(opts)
}

func m2Request(t *testing.T, handler *RelayHandler, plain, model, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	if remoteAddr == "" {
		remoteAddr = "203.0.113.9:1234"
	}
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	handler.Handler().ServeHTTP(w, req)
	return w
}

func m2Balance(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var balance int64
	if err := db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return balance
}

type m2UsageRow struct {
	status      int16
	cost        int64
	refunded    int64
	preDeducted int64
	group       sql.NullString
}

func m2UsageRows(t *testing.T, logsDB *sql.DB, userID int64) []m2UsageRow {
	t.Helper()
	rows, err := logsDB.Query(`SELECT status, cost, refunded, pre_deducted, "group"
		FROM usage_logs WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("read usage logs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []m2UsageRow
	for rows.Next() {
		var row m2UsageRow
		if err := rows.Scan(&row.status, &row.cost, &row.refunded, &row.preDeducted, &row.group); err != nil {
			t.Fatalf("scan usage log: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usage logs: %v", err)
	}
	return out
}

// m2CleanupUsageLogs 登记 usage_logs 清理。日志库被同包其它集成用例
// （TestIntegrationUsageLogsScoping 统计 24h 全量行）共享，漏清理会污染断言。
func m2CleanupUsageLogs(t *testing.T, logsDB *sql.DB, userID int64) {
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID) })
}

func m2WalletRows(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count wallet rows: %v", err)
	}
	return count
}

type m2CounterUpstream struct {
	server *httptest.Server
	calls  atomic.Int64
}

func m2Counter(t *testing.T, status int, body string) *m2CounterUpstream {
	t.Helper()
	counter := &m2CounterUpstream{}
	counter.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(counter.server.Close)
	return counter
}

func (c *m2CounterUpstream) count() int { return int(c.calls.Load()) }

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestIntegrationM2TokenRepoProjection 覆盖 R-67 与用例 1/4：
// 生产中继接缝 store.TokenRepo.Resolve 投影 auto 字段与白名单，RelayAuth
// 按真实行执行模型/IP 校验。
func TestIntegrationM2TokenRepoProjection(t *testing.T) {
	mainDB, _, gormDB := m2TestDBs(t)
	suffix := m2Suffix()
	g1, g2 := "m2g1_"+suffix, "m2g2_"+suffix
	m2SeedGroup(t, mainDB, g1, 100, 1)
	m2SeedGroup(t, mainDB, g2, 300, 1)
	userID := m2SeedUser(t, mainDB, g1, 1000)
	modelLimits := `["m2-allowed"]`
	allowIPs := `["10.0.0.0/8"]`
	tokenID, plain := m2SeedToken(t, mainDB, userID, "auto", `["`+g1+`","`+g2+`"]`, true,
		&modelLimits, &allowIPs)
	upstream := m2Counter(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	m2SeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "m2-proj-"+suffix, g1, upstream.server.URL, "m2-allowed")

	// (a) 生产接缝直接投影。
	repo := store.NewTokenRepo(mainDB)
	meta, err := repo.Resolve(context.Background(), auth.HashTokenKey(plain))
	if err != nil {
		t.Fatalf("TokenRepo.Resolve: %v", err)
	}
	if meta.Group != "auto" || !meta.CrossGroupRetry || len(meta.AutoGroups) != 2 ||
		meta.AutoGroups[0] != g1 || meta.AutoGroups[1] != g2 {
		t.Fatalf("auto 投影 = %#v，期望 group=auto / [%s %s] / retry=true", meta, g1, g2)
	}
	if len(meta.ModelLimits) != 1 || meta.ModelLimits[0] != "m2-allowed" ||
		len(meta.AllowIPs) != 1 || meta.AllowIPs[0] != "10.0.0.0/8" {
		t.Fatalf("白名单投影 = %v / %v", meta.ModelLimits, meta.AllowIPs)
	}

	// (b) BuildRelayContext 的 R-61 解析：UsingGroup = AutoGroups[0]，
	// UserGroup 保持字面 "auto"。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m2-allowed"}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	tid := tokenID
	rc, err := relay.BuildRelayContext(context.Background(),
		&auth.Principal{UserID: userID, TokenID: &tid, Role: "user"}, req, repo)
	if err != nil {
		t.Fatalf("BuildRelayContext: %v", err)
	}
	if rc.UsingGroup != g1 || rc.UserGroup != "auto" || !rc.CrossGroupRetry ||
		len(rc.AutoGroups) != 2 || rc.AutoGroups[1] != g2 {
		t.Fatalf("RelayContext auto 字段 = UsingGroup:%q UserGroup:%q Auto:%v Retry:%v",
			rc.UsingGroup, rc.UserGroup, rc.AutoGroups, rc.CrossGroupRetry)
	}

	handler := m2NewHandler(t, mainDB, nil)
	// (c) allow_ips 不含请求来源 → 403 access_denied（真实 RelayAuth）。
	w := m2Request(t, handler, plain, "m2-allowed", "192.0.2.44:1234")
	if w.Code != http.StatusForbidden {
		t.Fatalf("非白名单 IP status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "access_denied" {
		t.Fatalf("code = %q，期望 access_denied", got)
	}
	// (d) 模型白名单：合规 IP 但越界模型 → 403 distributor_token_model_forbidden。
	w = m2Request(t, handler, plain, "m2-other", "10.1.2.3:5555")
	if w.Code != http.StatusForbidden {
		t.Fatalf("越界模型 status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "distributor_token_model_forbidden" {
		t.Fatalf("code = %q，期望 distributor_token_model_forbidden", got)
	}
	// (e) 合规请求 → 200，且上游只被调用一次。
	w = m2Request(t, handler, plain, "m2-allowed", "10.1.2.3:5555")
	if w.Code != http.StatusOK {
		t.Fatalf("合规请求 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if upstream.count() != 1 {
		t.Fatalf("上游调用 = %d，期望 1", upstream.count())
	}
	_ = gormDB
}

// TestIntegrationM2AutoGroupsNotEnabled 覆盖用例 2 / R-61：
// 历史空列表 auto 令牌请求期 403，零预扣、零上游、零流水。
func TestIntegrationM2AutoGroupsNotEnabled(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	g1 := "m2g1_" + suffix
	m2SeedGroup(t, mainDB, g1, 100, 1)
	model := "m2-empty-" + suffix
	m2SeedModel(t, mainDB, model, 100)
	userID := m2SeedUser(t, mainDB, g1, 1000)
	m2CleanupUsageLogs(t, logsDB, userID)
	tokenID, plain := m2SeedToken(t, mainDB, userID, "auto", `[]`, true, nil, nil)
	_ = tokenID
	upstream := m2Counter(t, http.StatusOK, `{"id":"ok"}`)
	m2SeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "m2-empty-"+suffix, g1, upstream.server.URL, model)

	handler := m2NewHandler(t, mainDB, m2NewEngine(t, gormDB, logsDB, rdb))
	w := m2Request(t, handler, plain, model, "203.0.113.9:1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "auto_groups_not_enabled" {
		t.Fatalf("code = %q，期望 auto_groups_not_enabled", got)
	}
	if upstream.count() != 0 {
		t.Fatalf("上游调用 = %d，期望 0", upstream.count())
	}
	if rows := m2WalletRows(t, mainDB, userID); rows != 0 {
		t.Fatalf("wallet_transactions = %d，期望 0（零 PreConsume）", rows)
	}
	if rows := m2UsageRows(t, logsDB, userID); len(rows) != 0 {
		t.Fatalf("usage_logs = %d，期望 0", len(rows))
	}
}

// TestIntegrationM2CrossGroupSettlementFinalGroup 覆盖 R-65 与
// TC-F-02-14-03：g1 倍率 1.0、g2 倍率 3.0，g1 全部 500 → g2 成功，
// 实际扣费按 ×3（读 balance + usage_logs，不只断 200）。
func TestIntegrationM2CrossGroupSettlementFinalGroup(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	g1, g2 := "m2g1_"+suffix, "m2g2_"+suffix
	m2SeedGroup(t, mainDB, g1, 100, 1)
	m2SeedGroup(t, mainDB, g2, 300, 1)
	model := "m2-settle-" + suffix
	m2SeedModel(t, mainDB, model, 100) // 按次 100 积分
	userID := m2SeedUser(t, mainDB, g1, 10_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	tokenID, plain := m2SeedToken(t, mainDB, userID, "auto", `["`+g1+`","`+g2+`"]`, true, nil, nil)
	_ = tokenID

	sealer := NewSecretSealer(relaySealerMaster)
	failing := m2Counter(t, http.StatusInternalServerError, `{"error":"boom"}`)
	working := m2Counter(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	m2SeedChannel(t, mainDB, sealer, "m2-g1-"+suffix, g1, failing.server.URL, model)
	m2SeedChannel(t, mainDB, sealer, "m2-g2-"+suffix, g2, working.server.URL, model)

	handler := m2NewHandler(t, mainDB, m2NewEngine(t, gormDB, logsDB, rdb))
	w := m2Request(t, handler, plain, model, "203.0.113.9:1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	// 组内预算 = relayMaxRetries(3)+1 = 4；g2 一次成功。
	if got := failing.count(); got != 4 {
		t.Fatalf("g1 上游调用 = %d，期望 4（组内预算）", got)
	}
	if got := working.count(); got != 1 {
		t.Fatalf("g2 上游调用 = %d，期望 1", got)
	}
	if balance := m2Balance(t, mainDB, userID); balance != 9_700 {
		t.Fatalf("余额 = %d，期望 9700（按 g2 倍率 3 扣 300，而非 g1 的 100）", balance)
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 1 {
		t.Fatalf("usage_logs = %d，期望 1", len(rows))
	}
	row := rows[0]
	if row.status != billing.UsageLogStatusSuccess || row.cost != 300 {
		t.Fatalf("usage_log = status %d / cost %d，期望 success/300（最终组倍率）", row.status, row.cost)
	}
	if !row.group.Valid || row.group.String != g2 {
		t.Fatalf("usage_logs.group = %v，期望 %s（日志也按最终组）", row.group, g2)
	}
}

// TestIntegrationM2GroupRatioMissing 覆盖用例 12 / R-64 / I-25：
// g2 已停用 → 切组时 403 group_ratio_missing，g2 零上游，预扣全额退回。
func TestIntegrationM2GroupRatioMissing(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	g1, g2 := "m2g1_"+suffix, "m2g2_"+suffix
	m2SeedGroup(t, mainDB, g1, 100, 1)
	m2SeedGroup(t, mainDB, g2, 300, 2) // status=2：已停用
	model := "m2-ratio-" + suffix
	m2SeedModel(t, mainDB, model, 100)
	userID := m2SeedUser(t, mainDB, g1, 1_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	tokenID, plain := m2SeedToken(t, mainDB, userID, "auto", `["`+g1+`","`+g2+`"]`, true, nil, nil)
	_ = tokenID

	sealer := NewSecretSealer(relaySealerMaster)
	failing := m2Counter(t, http.StatusInternalServerError, `{"error":"boom"}`)
	disabled := m2Counter(t, http.StatusOK, `{"id":"ok"}`)
	m2SeedChannel(t, mainDB, sealer, "m2-g1-"+suffix, g1, failing.server.URL, model)
	m2SeedChannel(t, mainDB, sealer, "m2-g2-"+suffix, g2, disabled.server.URL, model)

	handler := m2NewHandler(t, mainDB, m2NewEngine(t, gormDB, logsDB, rdb))
	w := m2Request(t, handler, plain, model, "203.0.113.9:1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "group_ratio_missing" {
		t.Fatalf("code = %q，期望 group_ratio_missing（不得落 502）", got)
	}
	if disabled.count() != 0 {
		t.Fatalf("g2 上游调用 = %d，期望 0", disabled.count())
	}
	if balance := m2Balance(t, mainDB, userID); balance != 1_000 {
		t.Fatalf("余额 = %d，期望 1000（预扣全额退回）", balance)
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 1 || rows[0].status != billing.UsageLogStatusRefunded || rows[0].cost != 0 {
		t.Fatalf("usage_logs = %#v，期望恰好一行 refunded/cost=0", rows)
	}
}

// TestIntegrationM2EmptyFirstGroupEstimateFollowsSwitch 覆盖 R-62/R-64：
// 首组无候选、切到 g2 后才 PreConsume 时，预扣估算必须使用 g2 的倍率
// （pre_deducted=300，而不是 g1 的 100），否则结算会出现「按 100 预扣、
// 按 300 补扣」的余额窗口。
func TestIntegrationM2EmptyFirstGroupEstimateFollowsSwitch(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	g1, g2 := "m2g1_"+suffix, "m2g2_"+suffix
	m2SeedGroup(t, mainDB, g1, 100, 1)
	m2SeedGroup(t, mainDB, g2, 300, 1)
	model := "m2-empty1-" + suffix
	m2SeedModel(t, mainDB, model, 100)
	userID := m2SeedUser(t, mainDB, g1, 1_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	tokenID, plain := m2SeedToken(t, mainDB, userID, "auto", `["`+g1+`","`+g2+`"]`, true, nil, nil)
	_ = tokenID

	working := m2Counter(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	m2SeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "m2-g2-"+suffix, g2, working.server.URL, model)

	handler := m2NewHandler(t, mainDB, m2NewEngine(t, gormDB, logsDB, rdb))
	w := m2Request(t, handler, plain, model, "203.0.113.9:1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if working.count() != 1 {
		t.Fatalf("g2 上游调用 = %d，期望 1", working.count())
	}
	if balance := m2Balance(t, mainDB, userID); balance != 700 {
		t.Fatalf("余额 = %d，期望 700（g2 倍率 3 × 100）", balance)
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 1 {
		t.Fatalf("usage_logs = %d，期望 1", len(rows))
	}
	if rows[0].cost != 300 || rows[0].preDeducted != 300 {
		t.Fatalf("usage_log cost=%d pre_deducted=%d，期望 300/300（估算跟随切组后的分组）",
			rows[0].cost, rows[0].preDeducted)
	}
}
