//go:build integration

package httpapi

// T9 端到端集成验收：真实 PostgreSQL（主库 + 日志库）+ Redis + 真实计费引擎，
// 驱动 RelayHandler 的完整接缝（鉴权 → 归一化 → 路由 → PreConsume →
// 上游失败 → Refund → usage_logs）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationRelayBilling -v
//
// 覆盖（对应 R-84/R-85/R-86）：
//   - 按次模型全部尝试失败 → 余额不变、hold/release 平衡、usage_logs=refunded
//     且没有 success 行；
//   - 余额不足 → 402 insufficient_quota（不触达上游、不写日志）；
//   - 停用模型（status=2）→ 404 model_not_found（不进入上游/计费）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// relayBillingTestDBs 打开主库与日志库（日志库名固定为 newtg_logs，与
// billing 包的集成测试同一约定），并返回 GORM 句柄。
func relayBillingTestDBs(t *testing.T) (*sql.DB, *sql.DB, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过中继计费集成测试")
	}
	mainDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	if err := mainDB.Ping(); err != nil {
		_ = mainDB.Close()
		t.Fatalf("ping main db: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })

	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("parse NEWTG_TEST_DB_DSN: %v", err)
	}
	u.Path = "/newtg_logs"
	logsDB, err := sql.Open("pgx", u.String())
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

func relayBillingTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过中继计费集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// relayBillingStaticConfig 是计费引擎的最小配置：禁用信任旁路（保证每次都
// 真实预扣），预扣比例 1.0。
type relayBillingStaticConfig map[string]json.RawMessage

func (m relayBillingStaticConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

func relayBillingTestConfig() billing.ConfigReader {
	return relayBillingStaticConfig{
		billing.ConfigPreConsumeRatio: json.RawMessage("1.0"),
		billing.ConfigTrustQuotaUSD:   json.RawMessage("0"),
	}
}

var relayBillingSeq atomic.Int64

// relayBillingTestUser 创建独立用户与 API 令牌（remain_quota NULL = 不限额度）。
func relayBillingTestUser(t *testing.T, db *sql.DB, balance int64) (int64, int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	username := fmt.Sprintf("relaybil%d%d", time.Now().UnixNano()%1_000_000, relayBillingSeq.Add(1))
	var userID int64
	if err := db.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', $2, 'default') RETURNING id`, username, balance).Scan(&userID); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	var tokenID int64
	if err := db.QueryRow(
		`INSERT INTO api_tokens (user_id, name, key_hash, "group", remain_quota)
		 VALUES ($1, 'relay-billing-test', $2, 'default', NULL) RETURNING id`,
		userID, fmt.Sprintf("hash-%d-%d", time.Now().UnixNano(), relayBillingSeq.Add(1))).Scan(&tokenID); err != nil {
		t.Fatalf("create test token: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM api_tokens WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, tokenID
}

// relayBillingTestModel 插入一条 per_request 定价（积分/次），并清理。
func relayBillingTestModel(t *testing.T, db *sql.DB, model string, price int64, status int16) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		 VALUES ($1, 1, $2, 't9-test', $3)`, model, price, status); err != nil {
		t.Fatalf("insert model_pricing(%s): %v", model, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model = $1`, model)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})
}

// relayBillingTestEnv 组装真实引擎 + 内存渠道/令牌 fake + httptest 上游。
type relayBillingTestEnv struct {
	handler *RelayHandler
	mainDB  *sql.DB
	logsDB  *sql.DB
	userID  int64
}

func newRelayBillingTestEnv(t *testing.T, model string, balance int64,
	mainDB, logsDB *sql.DB, gormDB *gorm.DB, rdb *redis.Client,
	upstream *httptest.Server) *relayBillingTestEnv {

	t.Helper()
	userID, tokenID := relayBillingTestUser(t, mainDB, balance)

	// 定价源必须在插入定价行之后构造（DBSource 有 30s 缓存；先构造会缓存
	// 「无行 → 默认价」，把测试变成与缓存时序相关）。
	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	engine := billing.NewEngine(gormDB, logsDB, rdb, relayBillingTestConfig(), source,
		billing.NewRegistry(rdb), billing.NewDBLogger(logsDB))

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	channels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{ChannelID: int(relayTestChannelID), Priority: 10, Weight: 1}},
		channels: map[int64]*store.Channel{
			relayTestChannelID: {
				ID:              relayTestChannelID,
				Name:            "t9-upstream",
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
	tokens := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID: tokenID, UserID: userID, Status: "enabled", UnlimitedQuota: true,
	})
	access := &fakeRelayAccess{role: "admin", status: "enabled"}

	handler := NewRelayHandler(RelayHandlerOptions{
		Channels:    channels,
		Tokens:      tokens,
		Access:      access,
		Sealer:      sealer,
		Client:      upstream.Client(),
		ModelStatus: store.NewPricingRepo(mainDB),
		Billing:     engine.NewSession,
	})
	return &relayBillingTestEnv{
		handler: handler, mainDB: mainDB, logsDB: logsDB, userID: userID,
	}
}

func (e *relayBillingTestEnv) do(t *testing.T, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	w := httptest.NewRecorder()
	e.handler.Handler().ServeHTTP(w, req)
	return w
}

func (e *relayBillingTestEnv) balance(t *testing.T) int64 {
	t.Helper()
	var balance int64
	if err := e.mainDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, e.userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return balance
}

// walletSummary 汇总该用户的钱包流水（按 request_id 聚合）。
type walletSummary struct {
	hold, release, consume int64
	holdStatus             string
}

func (e *relayBillingTestEnv) wallet(t *testing.T) walletSummary {
	t.Helper()
	rows, err := e.mainDB.Query(`
		SELECT type, amount, status FROM wallet_transactions WHERE user_id = $1
		 ORDER BY id`, e.userID)
	if err != nil {
		t.Fatalf("read wallet rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var sum walletSummary
	for rows.Next() {
		var typ, status string
		var amount int64
		if err := rows.Scan(&typ, &amount, &status); err != nil {
			t.Fatalf("scan wallet row: %v", err)
		}
		switch typ {
		case "consume_pre":
			sum.hold += amount // 负数
			sum.holdStatus = status
		case "release":
			sum.release += amount
		case "consume":
			sum.consume += amount
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate wallet rows: %v", err)
	}
	return sum
}

// usageLogSummary 是该用户 usage_logs 的断言投影。
type usageLogSummary struct {
	status                      int16
	cost, refunded, preDeducted int64
	other                       []byte
}

func (e *relayBillingTestEnv) usageLogs(t *testing.T) []usageLogSummary {
	t.Helper()
	rows, err := e.logsDB.Query(`
		SELECT status, cost, refunded, pre_deducted, other::text
		  FROM usage_logs WHERE user_id = $1 ORDER BY id`, e.userID)
	if err != nil {
		t.Fatalf("read usage logs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []usageLogSummary
	for rows.Next() {
		var s usageLogSummary
		if err := rows.Scan(&s.status, &s.cost, &s.refunded, &s.preDeducted, &s.other); err != nil {
			t.Fatalf("scan usage log: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usage logs: %v", err)
	}
	return out
}

func (e *relayBillingTestEnv) cleanupLogs(t *testing.T) {
	t.Helper()
	_, _ = e.logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, e.userID)
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestIntegrationRelayBillingFailureRefunds：按次模型（150 积分/次）+ 上游
// 全部尝试失败 → HTTP 502、余额不变、hold 已 released 且 release 回补、
// usage_logs 恰好一行 status=refunded（cost=0），绝不出现 success 行。
func TestIntegrationRelayBillingFailureRefunds(t *testing.T) {
	mainDB, logsDB, gormDB := relayBillingTestDBs(t)
	rdb := relayBillingTestRedis(t)
	model := fmt.Sprintf("t9-percall-fail-%d", time.Now().UnixNano())
	relayBillingTestModel(t, mainDB, model, 150, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := newRelayBillingTestEnv(t, model, 1000, mainDB, logsDB, gormDB, rdb, upstream)
	env.cleanupLogs(t)

	w := env.do(t, model)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502（重试耗尽）; body=%s", w.Code, w.Body.String())
	}

	if got := env.balance(t); got != 1000 {
		t.Fatalf("balance = %d, want 1000（失败请求不得扣费）", got)
	}
	wallet := env.wallet(t)
	if wallet.hold != -150 {
		t.Fatalf("hold amount = %d, want -150", wallet.hold)
	}
	if wallet.release != 150 {
		t.Fatalf("release amount = %d, want 150（全额退回 hold）", wallet.release)
	}
	if wallet.consume != 0 {
		t.Fatalf("consume = %d, want 0（失败请求不得产生实耗）", wallet.consume)
	}
	if wallet.holdStatus != "released" {
		t.Fatalf("hold status = %q, want released", wallet.holdStatus)
	}

	logs := env.usageLogs(t)
	if len(logs) != 1 {
		t.Fatalf("usage_logs rows = %d, want exactly 1", len(logs))
	}
	entry := logs[0]
	if entry.status != billing.UsageLogStatusRefunded {
		t.Fatalf("usage_logs.status = %d, want %d(refunded)", entry.status, billing.UsageLogStatusRefunded)
	}
	if entry.cost != 0 || entry.refunded != 150 || entry.preDeducted != 150 {
		t.Fatalf("usage_logs amounts = cost:%d refunded:%d pre_deducted:%d, want 0/150/150",
			entry.cost, entry.refunded, entry.preDeducted)
	}
	for _, row := range logs {
		if row.status == billing.UsageLogStatusSuccess {
			t.Fatal("failed request must not write a success usage log")
		}
	}
}

// TestIntegrationRelayBillingInsufficientQuota402：余额不足以覆盖按次预扣 →
// 402 insufficient_quota，上游不被调用，不产生 hold / 日志。
func TestIntegrationRelayBillingInsufficientQuota402(t *testing.T) {
	mainDB, logsDB, gormDB := relayBillingTestDBs(t)
	rdb := relayBillingTestRedis(t)
	model := fmt.Sprintf("t9-percall-402-%d", time.Now().UnixNano())
	relayBillingTestModel(t, mainDB, model, 150, 1)

	var upstreamCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := newRelayBillingTestEnv(t, model, 100, mainDB, logsDB, gormDB, rdb, upstream)
	env.cleanupLogs(t)

	w := env.do(t, model)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"insufficient_quota"`) {
		t.Fatalf("body = %s, want insufficient_quota", w.Body.String())
	}
	if got := atomic.LoadInt64(&upstreamCalls); got != 0 {
		t.Fatalf("upstream calls = %d, want 0（402 必须在派发前）", got)
	}
	if got := env.balance(t); got != 100 {
		t.Fatalf("balance = %d, want 100（预扣失败不得改余额）", got)
	}
	if rows := env.usageLogs(t); len(rows) != 0 {
		t.Fatalf("usage_logs rows = %d, want 0（会话未开启）", len(rows))
	}
	if sum := env.wallet(t); sum.hold != 0 || sum.release != 0 || sum.consume != 0 {
		t.Fatalf("wallet rows = %+v, want empty（预扣失败不落流水）", sum)
	}
}

// TestIntegrationRelayBillingPriceError400：定价配置错误（billing_expr 为空，
// R-37 fail-closed）→ 400 model_price_error；不得落 502，也不得触达上游。
func TestIntegrationRelayBillingPriceError400(t *testing.T) {
	mainDB, logsDB, gormDB := relayBillingTestDBs(t)
	rdb := relayBillingTestRedis(t)
	model := fmt.Sprintf("t9-price-error-%d", time.Now().UnixNano())
	emptyExpr := ""
	if _, err := mainDB.Exec(
		`INSERT INTO model_pricing (model, mode, billing_expr, author, status)
		 VALUES ($1, 0, $2, 't9-test', 1)`, model, emptyExpr); err != nil {
		t.Fatalf("insert invalid model_pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	var upstreamCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := newRelayBillingTestEnv(t, model, 1000, mainDB, logsDB, gormDB, rdb, upstream)
	env.cleanupLogs(t)

	w := env.do(t, model)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"model_price_error"`) {
		t.Fatalf("body = %s, want model_price_error", w.Body.String())
	}
	if got := atomic.LoadInt64(&upstreamCalls); got != 0 {
		t.Fatalf("upstream calls = %d, want 0（定价错误必须在派发前失败）", got)
	}
	if got := env.balance(t); got != 1000 {
		t.Fatalf("balance = %d, want 1000（预扣失败不得改余额）", got)
	}
	if rows := env.usageLogs(t); len(rows) != 0 {
		t.Fatalf("usage_logs rows = %d, want 0（会话未开启）", len(rows))
	}
}

// TestIntegrationRelayBillingDisabledModel404：model_pricing.status=2 →
// 404 model_not_found；上游不被调用，也不产生计费副作用。
func TestIntegrationRelayBillingDisabledModel404(t *testing.T) {
	mainDB, logsDB, gormDB := relayBillingTestDBs(t)
	rdb := relayBillingTestRedis(t)
	model := fmt.Sprintf("t9-disabled-%d", time.Now().UnixNano())
	relayBillingTestModel(t, mainDB, model, 150, 2)

	var upstreamCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := newRelayBillingTestEnv(t, model, 1000, mainDB, logsDB, gormDB, rdb, upstream)
	env.cleanupLogs(t)

	w := env.do(t, model)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"model_not_found"`) {
		t.Fatalf("body = %s, want model_not_found", w.Body.String())
	}
	if got := atomic.LoadInt64(&upstreamCalls); got != 0 {
		t.Fatalf("upstream calls = %d, want 0（停用模型不得进入上游）", got)
	}
	if rows := env.usageLogs(t); len(rows) != 0 {
		t.Fatalf("usage_logs rows = %d, want 0", len(rows))
	}
	if sum := env.wallet(t); sum.hold != 0 || sum.release != 0 || sum.consume != 0 {
		t.Fatalf("wallet rows = %+v, want empty", sum)
	}
}

// TestIntegrationRelayBillingUsageSemanticAbsentForOpenAI 验证 OpenAI 请求的
// usage_logs.other 不写 usage_semantic（Claude 语义只由协议路径或 Claude
// 专有用量键触发；OpenAI 的 PromptIncludesCache=true 不满足任一条件）。
//
// Claude 语义的正向写入由 billing 包的
// TestIntegrationSessionUsageSemanticClaude / ...FromFact 覆盖。
func TestIntegrationRelayBillingUsageSemanticAbsentForOpenAI(t *testing.T) {
	mainDB, logsDB, gormDB := relayBillingTestDBs(t)
	rdb := relayBillingTestRedis(t)
	model := fmt.Sprintf("t9-semantic-%d", time.Now().UnixNano())
	relayBillingTestModel(t, mainDB, model, 100, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()

	env := newRelayBillingTestEnv(t, model, 1000, mainDB, logsDB, gormDB, rdb, upstream)
	env.cleanupLogs(t)

	w := env.do(t, model)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	logs := env.usageLogs(t)
	if len(logs) != 1 {
		t.Fatalf("usage_logs rows = %d, want 1", len(logs))
	}
	var other map[string]any
	if err := json.Unmarshal(logs[0].other, &other); err != nil {
		t.Fatalf("decode other: %v", err)
	}
	if _, ok := other["usage_semantic"]; ok {
		t.Fatalf("OpenAI 请求不得写 usage_semantic=anthropic: %v", other)
	}
}

// TestIntegrationRelayBillingHeaderUsageNotClaude 是 F1 的端到端回归：
// 上游只在 `x-model-response-usage` 头里报告用量（OpenAI 语义）。该解析器在
// 缺 `prompt_includes_cache` 时把关系布尔默认成 false；中继绝不能因此把
// usage_logs.other.usage_semantic 写成 anthropic。
func TestIntegrationRelayBillingHeaderUsageNotClaude(t *testing.T) {
	mainDB, logsDB, gormDB := relayBillingTestDBs(t)
	rdb := relayBillingTestRedis(t)
	model := fmt.Sprintf("t9-header-openai-%d", time.Now().UnixNano())
	relayBillingTestModel(t, mainDB, model, 100, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 响应体没有任何 usage 对象；用量只在非标准头里。
		w.Header().Set("x-model-response-usage",
			`{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}`)
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	env := newRelayBillingTestEnv(t, model, 1000, mainDB, logsDB, gormDB, rdb, upstream)
	env.cleanupLogs(t)

	w := env.do(t, model)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	logs := env.usageLogs(t)
	if len(logs) != 1 {
		t.Fatalf("usage_logs rows = %d, want 1", len(logs))
	}
	var other map[string]any
	if err := json.Unmarshal(logs[0].other, &other); err != nil {
		t.Fatalf("decode other: %v", err)
	}
	if got, ok := other["usage_semantic"]; ok {
		t.Fatalf("usage_semantic = %v, want absent（header 派生 OpenAI 不得标 anthropic）", got)
	}
}
