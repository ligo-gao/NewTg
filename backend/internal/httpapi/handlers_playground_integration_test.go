//go:build integration

package httpapi

// M6c Task 13 的 /pg 端到端集成验收（真实 PostgreSQL 主库 + 日志库 + Redis +
// 真实计费引擎 + 真实 UserGroupsRepo）。
//
// 运行（隔离库，勿在共享 newtg_test 上执行写操作）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_t13?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationPlayground -v
//
// 断言（relay-01 §1.5 / modules-02 §2.15）：
//   - 仅会话（无 Authorization）→ 200；usage_logs 一行且 token_id IS NULL；
//   - 令牌 remain_quota 不变（跳过令牌额度）；
//   - 钱包照常扣费（余额下降、wallet_transactions 有 consume）；
//   - 余额不足 → 402，无 usage_logs、无资金变动；
//   - 模型不在当前分组 → 403，无 usage_logs、无资金变动。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/store"
)

// openPlaygroundIntegrationDBs 打开**隔离**的主库与日志库。
//
// 不能复用 relayBillingTestDBs：它把日志库名固定为 /newtg_logs（共享库），
// 而隔离主库的 users.id 从 1 开始，会与共享日志库中其它测试残留的
// user_id=1 行串在一起，让「恰好一行」断言失真。两个 DSN 都必须由调用方
// 显式指向本任务的隔离库。
func openPlaygroundIntegrationDBs(t *testing.T) (*sql.DB, *sql.DB, *gorm.DB) {
	t.Helper()
	open := func(env, label string) *sql.DB {
		dsn := os.Getenv(env)
		if dsn == "" {
			t.Skipf("%s 未设置，跳过 /pg 集成测试", env)
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", label, err)
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			t.Fatalf("ping %s: %v", label, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	mainDB := open("NEWTG_TEST_DB_DSN", "main db")
	logsDB := open("NEWTG_TEST_LOGS_DSN", "logs db")
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: mainDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	return mainDB, logsDB, gormDB
}

type playgroundIntegrationEnv struct {
	mux       *http.ServeMux
	sessions  *auth.SessionManager
	rdb       *redis.Client
	mainDB    *sql.DB
	logsDB    *sql.DB
	userID    int64
	tokenID   int64
	channelID int64
	sid       string
	csrf      string
}

// setupPlaygroundIntegration 创建用户/令牌/渠道/定价并装配真实依赖。
func setupPlaygroundIntegration(t *testing.T, balance, remainQuota int64) *playgroundIntegrationEnv {
	t.Helper()
	return setupPlaygroundIntegrationWithConfig(t, balance, remainQuota, nil, false)
}

// setupPlaygroundIntegrationWithConfig 允许注入计费配置与准入限流器，
// 供演示模式（site.demo.* + NEWTG_DEMO_ENABLED 覆盖）的 /pg 计费用例使用。
// cfg 为 nil 时沿用 relayBillingTestConfig（非演示、无限流）。
func setupPlaygroundIntegrationWithConfig(t *testing.T, balance, remainQuota int64,
	cfg m2BillingConfig, withLimiter bool,
) *playgroundIntegrationEnv {
	t.Helper()
	mainDB, logsDB, gormDB := openPlaygroundIntegrationDBs(t)
	rdb := relayBillingTestRedis(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	model := "pg-it-" + suffix

	if _, err := mainDB.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	username := "pgit" + suffix[len(suffix)-10:]
	var userID int64
	if err := mainDB.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', $2, 'default') RETURNING id`, username, balance).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM api_tokens WHERE user_id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})

	var tokenID int64
	if err := mainDB.QueryRow(
		`INSERT INTO api_tokens (user_id, name, key_hash, "group", remain_quota)
		 VALUES ($1, 'pg-it', $2, 'default', $3) RETURNING id`,
		userID, "pgit-"+suffix, remainQuota).Scan(&tokenID); err != nil {
		t.Fatalf("create token: %v", err)
	}

	// 真实 UserGroupsRepo 只从 DB 读「可用分组 × 渠道 supported_models ×
	// model_pricing」；路由用的渠道内容仍由内存 fake 提供（与其它中继集成
	// 测试一致），两者必须指向同一个模型名。
	var channelID int64
	if err := mainDB.QueryRow(
		`INSERT INTO channels (name, type, credentials, supported_models, group_tags, model_mapping, status)
		 VALUES ($1, 'openai', '\x00'::bytea, jsonb_build_array($2::text), NULL, '{}'::jsonb, 1)
		 RETURNING id`, "pg-it-channel-"+suffix, model).Scan(&channelID); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := mainDB.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		 VALUES ($1, 1, 100, 'pg-it', 1)`, model); err != nil {
		t.Fatalf("create model pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
		_, _ = mainDB.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"`+model+
			`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing source: %v", err)
	}
	var engineCfg billing.ConfigReader = relayBillingTestConfig()
	if cfg != nil {
		engineCfg = cfg
	}
	engine := billing.NewEngine(gormDB, logsDB, rdb, engineCfg, source,
		billing.NewRegistry(rdb), billing.NewDBLogger(logsDB))

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	channels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{ChannelID: int(channelID), Priority: 10, Weight: 1}},
		channels: map[int64]*store.Channel{
			channelID: {
				ID:              channelID,
				Name:            "pg-it",
				Type:            string(contract.ChannelTypeOpenAI),
				BaseURL:         &baseURL,
				Credentials:     sealedTestCredentials(t, sealer),
				SupportedModels: []byte(`["` + model + `"]`),
				ModelMapping:    []byte(`{}`),
				Status:          1,
				CreatedAt:       time.Now().UTC(),
			},
		},
		models: []string{model},
	}

	// 只有显式注入配置时才覆盖 handler 的读取面：非演示用例保持 Config=nil
	// 的原装配形状（cfg 是 map 类型，nil map 赋值给接口会得到非 nil 接口）。
	var handlerConfig ConfigReader
	if cfg != nil {
		handlerConfig = cfg
	}
	handler := NewRelayHandler(RelayHandlerOptions{
		Channels:       channels,
		Tokens:         store.NewTokenRepo(mainDB),
		Access:         store.NewTokenRepo(mainDB),
		Sealer:         sealer,
		Client:         upstream.Client(),
		Billing:        engine.NewSession,
		ModelStatus:    store.NewPricingRepo(mainDB),
		UserGroups:     store.NewUserGroupsRepo(mainDB),
		Config:         handlerConfig,
		RateLimiter:    limiterFor(withLimiter, rdb),
		TrustedProxies: "",
	})
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterPlayground(mux, sessions, "")

	sid, err := sessions.Create(ctx, userID, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := sessions.IssueCSRF(ctx, sid, csrf); err != nil {
		t.Fatalf("issue csrf: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Revoke(context.Background(), sid) })

	return &playgroundIntegrationEnv{
		mux: mux, sessions: sessions, rdb: rdb, mainDB: mainDB, logsDB: logsDB,
		userID: userID, tokenID: tokenID, channelID: channelID, sid: sid, csrf: csrf,
	}
}

// limiterFor 按需构造真实 Redis 限流器：/pg 演示额度用例需要它，
// 其它既有用例保持「无 limiter」的装配形状。
func limiterFor(with bool, rdb *redis.Client) RelayRateLimiter {
	if !with {
		return nil
	}
	return ratelimit.New(rdb)
}

func (e *playgroundIntegrationEnv) post(t *testing.T, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/pg/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set(auth.CSRFHeaderName, e.csrf)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: e.sid})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: e.csrf})
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func (e *playgroundIntegrationEnv) balance(t *testing.T) int64 {
	t.Helper()
	var balance int64
	if err := e.mainDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, e.userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return balance
}

func (e *playgroundIntegrationEnv) remainQuota(t *testing.T) int64 {
	t.Helper()
	var remain int64
	if err := e.mainDB.QueryRow(`SELECT remain_quota FROM api_tokens WHERE id = $1`, e.tokenID).Scan(&remain); err != nil {
		t.Fatalf("read remain_quota: %v", err)
	}
	return remain
}

type pgUsageRow struct {
	tokenID sql.NullInt64
	status  int16
	cost    int64
}

func (e *playgroundIntegrationEnv) usageLogs(t *testing.T) []pgUsageRow {
	t.Helper()
	rows, err := e.logsDB.Query(`SELECT token_id, status, cost FROM usage_logs WHERE user_id = $1 ORDER BY id`, e.userID)
	if err != nil {
		t.Fatalf("read usage_logs: %v", err)
	}
	defer rows.Close()
	var out []pgUsageRow
	for rows.Next() {
		var row pgUsageRow
		if err := rows.Scan(&row.tokenID, &row.status, &row.cost); err != nil {
			t.Fatalf("scan usage_log: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usage_logs: %v", err)
	}
	return out
}

// TestIntegrationPlaygroundSessionBillsWalletNotToken：TC-A-25/27 + TC-F-02-19-01/02。
func TestIntegrationPlaygroundSessionBillsWalletNotToken(t *testing.T) {
	env := setupPlaygroundIntegration(t, 1000, 500)
	model := modelNameForUser(t, env.mainDB, env.userID)

	w := env.post(t, model)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", w.Code, w.Body.String())
	}
	if env.balance(t) != 900 {
		t.Fatalf("balance = %d, want 900（按次 100 照常扣钱包）", env.balance(t))
	}
	if env.remainQuota(t) != 500 {
		t.Fatalf("token remain_quota = %d, want 500（不得扣令牌额度）", env.remainQuota(t))
	}
	logs := env.usageLogs(t)
	if len(logs) != 1 {
		t.Fatalf("usage_logs rows = %d, want 1", len(logs))
	}
	if logs[0].tokenID.Valid {
		t.Fatalf("usage_logs.token_id = %d, want NULL", logs[0].tokenID.Int64)
	}
	if logs[0].status != billing.UsageLogStatusSuccess {
		t.Fatalf("usage_logs.status = %d, want success", logs[0].status)
	}
	if logs[0].cost != 100 {
		t.Fatalf("usage_logs.cost = %d, want 100", logs[0].cost)
	}
}

// TestIntegrationPlaygroundInsufficientQuota402：TC-F-02-19-03 / TC-A-27 的
// 「预扣前拒绝、无资金变动」：余额 50 < 按次 100 → 402，无 usage_logs、无流水。
func TestIntegrationPlaygroundInsufficientQuota402(t *testing.T) {
	env := setupPlaygroundIntegration(t, 50, 500)
	model := modelNameForUser(t, env.mainDB, env.userID)

	w := env.post(t, model)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402（body=%s）", w.Code, w.Body.String())
	}
	if env.balance(t) != 50 {
		t.Fatalf("balance = %d, want 50（402 不得扣费）", env.balance(t))
	}
	if got := env.usageLogs(t); len(got) != 0 {
		t.Fatalf("usage_logs rows = %d, want 0（refused 不落计费行）", len(got))
	}
	var txns int
	if err := env.mainDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, env.userID).Scan(&txns); err != nil {
		t.Fatalf("count wallet txns: %v", err)
	}
	if txns != 0 {
		t.Fatalf("wallet_transactions = %d, want 0（预扣前拒绝）", txns)
	}
}

// TestIntegrationPlaygroundModelUnavailable403：TC-A-26 相邻的模型准入：
// 模型不在用户当前分组的可用清单 → 403，且不落计费行。
func TestIntegrationPlaygroundModelUnavailable403(t *testing.T) {
	env := setupPlaygroundIntegration(t, 1000, 500)

	w := env.post(t, "model-not-available")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403（body=%s）", w.Code, w.Body.String())
	}
	if env.balance(t) != 1000 {
		t.Fatalf("balance = %d, want 1000（403 不计费）", env.balance(t))
	}
	if got := env.usageLogs(t); len(got) != 0 {
		t.Fatalf("usage_logs rows = %d, want 0", len(got))
	}
}

// TestIntegrationPlaygroundDemoSessionIsExemptButQuotaStillApplies 固定 W13
// 控制端裁决的 /pg 计费口径：演示模式下，会话身份（无令牌）与 /v1/* 一样
// 享受「只记日志、不动钱」的豁免；但 §3.17.2 的两层日额度仍照常计数
// （第 21 次 429），成本兜底不因免单而消失。
func TestIntegrationPlaygroundDemoSessionIsExemptButQuotaStillApplies(t *testing.T) {
	cfg := m2BillingConfig{
		keyDemoEnabled:                json.RawMessage("true"),
		keyDemoRelayPerIPDaily:        json.RawMessage("20"),
		keyDemoRelayGlobalDaily:       json.RawMessage("2000"),
		billing.ConfigPreConsumeRatio: json.RawMessage("1.0"),
		billing.ConfigTrustQuotaUSD:   json.RawMessage("0"),
		// 只验证演示额度：关闭 B10/B11/B12，避免先到者拒掩盖 21 次的边界。
		ratelimit.KeyUserModelRPM:  json.RawMessage("0"),
		ratelimit.KeyTokenRPM:      json.RawMessage("0"),
		ratelimit.KeyUserGlobalRPM: json.RawMessage("0"),
	}
	env := setupPlaygroundIntegrationWithConfig(t, 1000, 500, cfg, true)
	model := modelNameForUser(t, env.mainDB, env.userID)

	// 演示额度键按站点日期分段，同一 IP 在同一天跨用例/重跑会累加；
	// 先清空本进程可见的键，保证 20/21 边界可复现。
	clearDemoKeys := func() {
		ctx := context.Background()
		keys, _ := env.rdb.Keys(ctx, "demo:relay:*").Result()
		if len(keys) > 0 {
			_ = env.rdb.Del(ctx, keys...).Err()
		}
	}
	clearDemoKeys()
	t.Cleanup(clearDemoKeys)

	for i := 0; i < 20; i++ {
		w := env.post(t, model)
		if w.Code != http.StatusOK {
			t.Fatalf("演示 /pg 第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	logs := env.usageLogs(t)
	if len(logs) != 20 {
		t.Fatalf("usage_logs = %d，期望 20（豁免只免资金，日志照写）", len(logs))
	}
	for i, row := range logs {
		if row.tokenID.Valid {
			t.Fatalf("第 %d 行 token_id = %d，期望 NULL（会话身份）", i+1, row.tokenID.Int64)
		}
		if row.cost <= 0 {
			t.Fatalf("第 %d 行 cost = %d，期望 > 0（真实成本落库）", i+1, row.cost)
		}
	}
	if got := env.balance(t); got != 1000 {
		t.Fatalf("演示 /pg 余额 = %d，期望不变 1000（会话身份也要免单）", got)
	}
	var walletRows int
	if err := env.mainDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`,
		env.userID).Scan(&walletRows); err != nil {
		t.Fatalf("count wallet rows: %v", err)
	}
	if walletRows != 0 {
		t.Fatalf("演示 /pg 钱包流水 = %d 行，期望 0（不得预扣/冻结）", walletRows)
	}

	// 第 21 次：额度耗尽 → 429，且不新增日志/资金动作。豁免不改变额度口径。
	w := env.post(t, model)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("演示 /pg 第 21 次 = %d，期望 429（额度仍然生效）；body=%s", w.Code, w.Body.String())
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
		t.Fatalf("第 21 次 code = %q，期望 %s", code, demoRelayQuotaExceededCode)
	}
	if got := len(env.usageLogs(t)); got != 20 {
		t.Fatalf("被拒请求写入 usage_logs：行数 = %d，期望 20", got)
	}
	if got := env.balance(t); got != 1000 {
		t.Fatalf("被拒请求改变余额：%d，期望 1000", got)
	}
}

// modelNameForUser 从该用户所属分组的可用模型清单中取唯一模型（避免在
// 测试里重复业务查询口径，直接复用生产 SQL）。
func modelNameForUser(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	groups, err := store.NewUserGroupsRepo(db).UsableGroupsForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("usable groups: %v", err)
	}
	for _, group := range groups {
		if len(group.Models) > 0 {
			return group.Models[0]
		}
	}
	t.Fatal("测试用户没有任何可用模型")
	return ""
}
