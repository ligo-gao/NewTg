package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
)

// 本文件是 M5 Task 3 的 HTTP 端点验收（P-9/P-10/P-12）：
//   - GET /api/topup/config 只对已认证用户开放并下发公开配置；
//   - POST /api/orders 的 Idempotency-Key 幂等（201 → 200）与跨用户 409；
//   - webhook 每 provider 独立 60 次/分钟限流（第 61 次 429，另一渠道不受影响）；
//   - Stripe webhook 验签失败 400 且不入账、合法回调恰入账一次。
//
// 需要真实 PostgreSQL（未设置 NEWTG_TEST_DB_DSN 时跳过）与 miniredis：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test ./internal/httpapi/ -run 'TestTopup|TestStripe|TestAlipay|TestOrderCreate' -v

type topupConfigMap map[string]json.RawMessage

func (m topupConfigMap) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

type topupTestEnv struct {
	t      *testing.T
	db     *sql.DB
	gorm   *gorm.DB
	mr     *miniredis.Miniredis
	rdb    *redis.Client
	router http.Handler
}

func topupHTTPTestConfig(stripeBaseURL string) topupConfigMap {
	_ = stripeBaseURL
	return topupConfigMap{
		billing.ConfigOnlineEnabled:        json.RawMessage("true"),
		billing.ConfigStripeEnabled:        json.RawMessage("true"),
		billing.ConfigStripeSecretKey:      json.RawMessage(`"sk_test_123"`),
		billing.ConfigStripeWebhookSecret:  json.RawMessage(`"whsec_test_123"`),
		billing.ConfigStripePublishableKey: json.RawMessage(`"pk_test_123"`),
		billing.ConfigQuotaPerUnit:         json.RawMessage("100"),
	}
}

func newTopupHTTPTestEnv(t *testing.T, stripeBaseURL string) *topupTestEnv {
	t.Helper()
	return newTopupHTTPTestEnvWithConfig(t, stripeBaseURL, nil)
}

// newTopupHTTPTestEnvWithConfig 允许用例在构造服务前改写站点配置
// （渠道最低价、分组倍率等只读配置的快照）。
func newTopupHTTPTestEnvWithConfig(t *testing.T, stripeBaseURL string,
	mutate func(topupConfigMap)) *topupTestEnv {

	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过充值端点集成测试")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := topupHTTPTestConfig(stripeBaseURL)
	if mutate != nil {
		mutate(cfg)
	}
	h, err := NewTopupHandlers(TopupHandlerConfig{
		DB:            gormDB,
		Redis:         rdb,
		Config:        cfg,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		StripeBaseURL: stripeBaseURL,
	})
	if err != nil {
		t.Fatalf("NewTopupHandlers: %v", err)
	}
	// 复刻 NewRouter 的挂载关系：用户端点走「受保护」子 mux，webhook 用
	// 精确模式绕过它（测试直接注入 principal，不构造完整会话链）。
	rt := &Router{mux: http.NewServeMux(), protected: http.NewServeMux()}
	h.RegisterTopupRoutes(rt.protected, nil)
	h.RegisterWebhookRoutes(rt)
	rt.mux.Handle("/api/", rt.protected)

	return &topupTestEnv{t: t, db: sqlDB, gorm: gormDB, mr: mr, rdb: rdb, router: rt.mux}
}

func (e *topupTestEnv) createUser(balance int64) int64 {
	e.t.Helper()
	if _, err := e.db.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		e.t.Fatalf("ensure default group: %v", err)
	}
	username := fmt.Sprintf("topup%d", time.Now().UnixNano())
	var id int64
	if err := e.db.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', $2, 'default') RETURNING id`, username, balance).Scan(&id); err != nil {
		e.t.Fatalf("create test user: %v", err)
	}
	e.t.Cleanup(func() {
		_, _ = e.db.Exec(`DELETE FROM orders WHERE user_id = $1`, id)
		_, _ = e.db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, id)
		_, _ = e.db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func (e *topupTestEnv) request(userID int64, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	if userID <= 0 {
		return e.requestAs(nil, method, path, body, headers)
	}
	return e.requestAs(&auth.Principal{UserID: userID, Role: "user"}, method, path, body, headers)
}

// requestAs 允许测试注入管理员角色与权限集合（production 由 authz 中间件解析）。
func (e *topupTestEnv) requestAs(p *auth.Principal, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

// insertOrder 直接落一笔订单，用于构造各种状态（HTTP 创建路径只产生 pending）。
func (e *topupTestEnv) insertOrder(userID int64, status string, amount int64, providerTxnID string) int64 {
	e.t.Helper()
	key := fmt.Sprintf("seed-%d-%d", time.Now().UnixNano(), userID)
	var txn *string
	if providerTxnID != "" {
		txn = &providerTxnID
	}
	var id int64
	if err := e.db.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status, idempotency_key, provider_txn_id)
		VALUES ($1, 'stripe', $2, 'CNY', $3, $4, $5) RETURNING id`,
		userID, amount, status, key, txn).Scan(&id); err != nil {
		e.t.Fatalf("insert order: %v", err)
	}
	return id
}

// newOrderStripeStub 模拟 PaymentIntent retrieve/cancel（Task 4 主动核实）。
// state 可在测试中途修改，以覆盖「未支付 → 已支付」的状态变化。
type orderStripeStubState struct {
	mu             sync.Mutex
	status         string
	amount         int64
	amountReceived int64
	currency       string
	httpStatus     int // 非 0 时直接返回该 HTTP 错误（模拟渠道故障）
}

func (s *orderStripeStubState) set(fn func(*orderStripeStubState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func newOrderStripeStub(t *testing.T) (*httptest.Server, *orderStripeStubState) {
	t.Helper()
	state := &orderStripeStubState{
		status: "requires_payment_method", amount: 1000,
		amountReceived: 0, currency: "cny",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/payment_intents/") {
			if strings.HasSuffix(r.URL.Path, "/cancel") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"pi_order","status":"canceled"}`)
				return
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.httpStatus != 0 {
				w.WriteHeader(state.httpStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"provider down"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w,
				`{"id":%q,"amount":%d,"amount_received":%d,"currency":%q,"status":%q}`,
				strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/"),
				state.amount, state.amountReceived, state.currency, state.status)
			return
		}
		// 创建路径沿用 Task 3 的桩语义。
		_ = r.ParseForm()
		amount, _ := strconv.ParseInt(r.PostForm.Get("amount"), 10, 64)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"pi_order_%d","client_secret":"pi_order_%d_secret"}`, amount, amount)
	}))
	t.Cleanup(server.Close)
	return server, state
}

// insertOrderRawCallback 写入原始回调，验证「用户不可见、管理员可见」。
func (e *topupTestEnv) insertOrderRawCallback(orderID int64, raw string) {
	e.t.Helper()
	if _, err := e.db.Exec(
		`UPDATE orders SET raw_callback = $1::jsonb WHERE id = $2`, raw, orderID); err != nil {
		e.t.Fatalf("set raw_callback: %v", err)
	}
}

// postWebhook 发送原始 body（webhook 验签必须针对原始字节）。
func (e *topupTestEnv) postWebhook(path, contentType string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return serve(e.router, req)
}

// newTopupStripeStub 返回按金额生成 pi id 的 Stripe 桩。
func newTopupStripeStub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		amount := r.PostForm.Get("amount")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"pi_http_%s","client_secret":"pi_http_%s_secret"}`, amount, amount)
	}))
	t.Cleanup(server.Close)
	return server
}

// stripeHTTPEvent 构造并签名一个 payment_intent.succeeded 事件。
func stripeHTTPEvent(t *testing.T, intentID string, amount int64, currency, secret string) ([]byte, string) {
	t.Helper()
	payload := []byte(fmt.Sprintf(
		`{"id":"evt_http_1","type":"payment_intent.succeeded","data":{"object":{"id":%q,"amount":%d,"amount_received":%d,"currency":%q,"status":"succeeded"}}}`,
		intentID, amount, amount, currency))
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return payload, fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// ---------------------------------------------------------------------------
// GET /api/topup/config
// ---------------------------------------------------------------------------

func TestTopupConfigEndpoint(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	userID := env.createUser(0)

	rr := env.request(userID, http.MethodGet, "/api/topup/config", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		AmountOptions    []int64  `json:"amount_options"`
		MinTopup         int64    `json:"min_topup"`
		MaxTopup         int64    `json:"max_topup"`
		QuotaPerUnit     int64    `json:"quota_per_unit"`
		CreditsRatio     float64  `json:"credits_ratio"`
		EnabledProviders []string `json:"enabled_providers"`
		Providers        map[string]struct {
			PublishableKey *string `json:"publishable_key"`
			MinTopup       int64   `json:"min_topup"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if resp.MinTopup != billing.DefaultMinTopupCents || resp.MaxTopup != billing.MaxTopupCents {
		t.Fatalf("min/max = %d/%d", resp.MinTopup, resp.MaxTopup)
	}
	if resp.QuotaPerUnit != 100 {
		t.Fatalf("quota_per_unit = %d", resp.QuotaPerUnit)
	}
	// 默认无分组倍率配置：回退 1.0，预览与服务端口径一致。
	if resp.CreditsRatio != 1.0 {
		t.Fatalf("credits_ratio = %v, want 1.0 fallback", resp.CreditsRatio)
	}
	if len(resp.EnabledProviders) != 1 || resp.EnabledProviders[0] != billing.ProviderStripe {
		t.Fatalf("enabled providers = %v", resp.EnabledProviders)
	}
	provider, ok := resp.Providers[billing.ProviderStripe]
	if !ok {
		t.Fatalf("providers missing stripe: %+v", resp.Providers)
	}
	if provider.PublishableKey == nil || *provider.PublishableKey != "pk_test_123" {
		t.Fatalf("publishable_key = %v", provider.PublishableKey)
	}
	// 未配置渠道最低价 → 冻结默认下限（F-04-01）。
	if provider.MinTopup != billing.DefaultProviderMinTopupCents {
		t.Fatalf("stripe min_topup = %d, want %d", provider.MinTopup, billing.DefaultProviderMinTopupCents)
	}
	// 响应体绝不能出现任何密钥。
	if strings.Contains(rr.Body.String(), "sk_test_123") || strings.Contains(rr.Body.String(), "whsec_test_123") {
		t.Fatal("topup config response leaked a secret")
	}

	// 未认证：401。
	anon := env.request(0, http.MethodGet, "/api/topup/config", nil, nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anon.Code)
	}
}

// TestTopupConfigEndpointProviderMinAndCreditsRatio 钉死 UI 需要的两处
// 服务端口径（modules-04 F-04-01）：渠道级最低价随 providers 下发；
// credits_ratio 是当前用户分组的有效倍率（不是恒 1）。
func TestTopupConfigEndpointProviderMinAndCreditsRatio(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnvWithConfig(t, stripe.URL, func(cfg topupConfigMap) {
		cfg[billing.ConfigStripeMinTopupCents] = json.RawMessage("20") // 20 元
		cfg[billing.ConfigTopupGroupRatio] = json.RawMessage(`{"default":0.8}`)
	})
	userID := env.createUser(0)

	rr := env.request(userID, http.MethodGet, "/api/topup/config", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		CreditsRatio float64 `json:"credits_ratio"`
		Providers    map[string]struct {
			MinTopup int64 `json:"min_topup"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if resp.CreditsRatio != 0.8 {
		t.Fatalf("credits_ratio = %v, want 0.8 (group default)", resp.CreditsRatio)
	}
	if got := resp.Providers[billing.ProviderStripe].MinTopup; got != 2_000 {
		t.Fatalf("stripe min_topup = %d, want 2000", got)
	}
}

// ---------------------------------------------------------------------------
// POST /api/orders
// ---------------------------------------------------------------------------

func TestOrderCreateEndpointIdempotent(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	userID := env.createUser(0)
	key := fmt.Sprintf("http-idem-%d", time.Now().UnixNano())
	body := map[string]any{"amount": 1000, "currency": "CNY", "provider": "stripe"}

	first := env.request(userID, http.MethodPost, "/api/orders", body,
		map[string]string{"Idempotency-Key": key})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstResp struct {
		Order struct {
			ID              int64  `json:"id"`
			Status          string `json:"status"`
			CreditedCredits int64  `json:"credited_credits"`
		} `json:"order"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if firstResp.Order.ID == 0 || firstResp.Order.Status != "pending" {
		t.Fatalf("order = %+v", firstResp.Order)
	}
	if firstResp.ClientSecret == "" {
		t.Fatal("client_secret missing on first create")
	}

	second := env.request(userID, http.MethodPost, "/api/orders", body,
		map[string]string{"Idempotency-Key": key})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	var secondResp struct {
		Order struct {
			ID int64 `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if secondResp.Order.ID != firstResp.Order.ID {
		t.Fatalf("second order id = %d, want %d", secondResp.Order.ID, firstResp.Order.ID)
	}
	var count int64
	if err := env.db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != 1 {
		t.Fatalf("orders = %d, want 1", count)
	}
}

func TestOrderCreateEndpointConflict(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	other := env.createUser(0)
	key := fmt.Sprintf("http-conflict-%d", time.Now().UnixNano())
	body := map[string]any{"amount": 1000, "currency": "CNY", "provider": "stripe"}

	if rr := env.request(owner, http.MethodPost, "/api/orders", body,
		map[string]string{"Idempotency-Key": key}); rr.Code != http.StatusCreated {
		t.Fatalf("owner status = %d, body = %s", rr.Code, rr.Body.String())
	}
	rr := env.request(other, http.MethodPost, "/api/orders", body,
		map[string]string{"Idempotency-Key": key})
	if rr.Code != http.StatusConflict {
		t.Fatalf("other status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != problemTypeOrderConflict {
		t.Fatalf("problem type = %q, want %q", problem.Type, problemTypeOrderConflict)
	}
}

func TestOrderCreateEndpointValidation(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	userID := env.createUser(0)
	valid := map[string]any{"amount": 1000, "currency": "CNY", "provider": "stripe"}

	t.Run("missing idempotency key", func(t *testing.T) {
		rr := env.request(userID, http.MethodPost, "/api/orders", valid, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("amount below minimum", func(t *testing.T) {
		rr := env.request(userID, http.MethodPost, "/api/orders",
			map[string]any{"amount": 999, "currency": "CNY", "provider": "stripe"},
			map[string]string{"Idempotency-Key": fmt.Sprintf("low-%d", time.Now().UnixNano())})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("unknown provider", func(t *testing.T) {
		rr := env.request(userID, http.MethodPost, "/api/orders",
			map[string]any{"amount": 1000, "currency": "CNY", "provider": "paypal"},
			map[string]string{"Idempotency-Key": fmt.Sprintf("prov-%d", time.Now().UnixNano())})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("wallet provider rejected", func(t *testing.T) {
		// wallet 只是订阅余额购买的响应侧取值：充值端点不按余额扣款，
		// 必须 400，不能建出一个没有渠道回调可结算的订单。
		rr := env.request(userID, http.MethodPost, "/api/orders",
			map[string]any{"amount": 1000, "currency": "CNY", "provider": "wallet"},
			map[string]string{"Idempotency-Key": fmt.Sprintf("wallet-%d", time.Now().UnixNano())})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("non-CNY currency", func(t *testing.T) {
		// M5 无汇率：USD/JPY/TRY 一律 400，且不得留下订单。
		for _, currency := range []string{"USD", "JPY", "TRY"} {
			rr := env.request(userID, http.MethodPost, "/api/orders",
				map[string]any{"amount": 1000, "currency": currency, "provider": "stripe"},
				map[string]string{"Idempotency-Key": fmt.Sprintf("fx-%s-%d", currency, time.Now().UnixNano())})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("currency %s status = %d, body = %s", currency, rr.Code, rr.Body.String())
			}
		}
	})
	t.Run("disabled provider", func(t *testing.T) {
		// 新环境只开 Stripe；支付宝请求 403（P-13 明确拒绝）。
		rr := env.request(userID, http.MethodPost, "/api/orders",
			map[string]any{"amount": 1000, "currency": "CNY", "provider": "alipay"},
			map[string]string{"Idempotency-Key": fmt.Sprintf("off-%d", time.Now().UnixNano())})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Webhook 限流与入账
// ---------------------------------------------------------------------------

func TestStripeWebhookRateLimit(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	payload := []byte(`{"id":"evt_rl","type":"payment_intent.succeeded"}`)

	// 前 60 次进入验签路径（签名无效 → 400），第 61 次被限流。
	for i := 0; i < webhookRateLimit; i++ {
		rr := env.postWebhook("/api/webhooks/stripe", "application/json", payload,
			map[string]string{"Stripe-Signature": "t=1,v1=bad"})
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate limited", i+1)
		}
	}
	rr := env.postWebhook("/api/webhooks/stripe", "application/json", payload,
		map[string]string{"Stripe-Signature": "t=1,v1=bad"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("61st status = %d, want 429", rr.Code)
	}

	// 独立桶：支付宝端点不受 Stripe 计数影响（不是 429）。
	alipay := env.postWebhook("/api/webhooks/alipay", "application/x-www-form-urlencoded",
		[]byte("trade_status=WAIT_BUYER_PAY"), nil)
	if alipay.Code == http.StatusTooManyRequests {
		t.Fatal("alipay bucket must be independent from stripe")
	}
}

func TestStripeWebhookEndpointCreditsOnce(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	userID := env.createUser(0)
	key := fmt.Sprintf("hook-%d", time.Now().UnixNano())

	create := env.request(userID, http.MethodPost, "/api/orders",
		map[string]any{"amount": 1000, "currency": "CNY", "provider": "stripe"},
		map[string]string{"Idempotency-Key": key})
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", create.Code, create.Body.String())
	}
	var created struct {
		Order struct {
			ID int64 `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	payload, sig := stripeHTTPEvent(t, "pi_http_1000", 1000, "cny", "whsec_test_123")

	// 伪造签名 → 400，订单仍 pending、无流水。
	bad := env.postWebhook("/api/webhooks/stripe", "application/json", payload,
		map[string]string{"Stripe-Signature": "t=" + strconv.FormatInt(time.Now().Unix(), 10) + ",v1=" + strings.Repeat("0", 64)})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad signature status = %d, body = %s", bad.Code, bad.Body.String())
	}
	var status string
	if err := env.db.QueryRow(`SELECT status FROM orders WHERE id = $1`, created.Order.ID).Scan(&status); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status after invalid signature = %s", status)
	}

	// 合法签名：200 且恰入账一次（重复投递幂等）。
	for i := 0; i < 2; i++ {
		rr := env.postWebhook("/api/webhooks/stripe", "application/json", payload,
			map[string]string{"Stripe-Signature": sig})
		if rr.Code != http.StatusOK {
			t.Fatalf("callback %d status = %d, body = %s", i, rr.Code, rr.Body.String())
		}
	}
	var credited int64
	if err := env.db.QueryRow(`SELECT status, credited_credits FROM orders WHERE id = $1`, created.Order.ID).
		Scan(&status, &credited); err != nil {
		t.Fatalf("read settled order: %v", err)
	}
	if status != "succeeded" || credited != 1000 {
		t.Fatalf("status/credits = %s/%d", status, credited)
	}
	var walletRows int64
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'topup' AND reference_type = 'order'`, userID).Scan(&walletRows); err != nil {
		t.Fatalf("count wallet: %v", err)
	}
	if walletRows != 1 {
		t.Fatalf("wallet rows = %d, want 1", walletRows)
	}
}

func TestAlipayWebhookRejectsForgedSignature(t *testing.T) {
	stripe := newTopupStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	// 支付宝未配置：合法形状的请求也必须 fail closed（failure，不入账）。
	rr := env.postWebhook("/api/webhooks/alipay", "application/x-www-form-urlencoded",
		[]byte("out_trade_no=TG1&trade_status=TRADE_SUCCESS&total_amount=10.00&sign=ZmFrZQ%3D%3D&sign_type=RSA2"), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "failure" {
		t.Fatalf("body = %q, want failure", got)
	}
}

// ---------------------------------------------------------------------------
// M5 Task 4：订单五状态状态机与订单端点（P-1/P-9/P-18）
// ---------------------------------------------------------------------------

func orderProblemType(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (body=%s)", err, rr.Body.String())
	}
	return problem.Type
}

type orderListPayload struct {
	Items []struct {
		ID              int64           `json:"id"`
		UserID          int64           `json:"user_id"`
		Status          string          `json:"status"`
		CreditedCredits int64           `json:"credited_credits"`
		RawCallback     json.RawMessage `json:"raw_callback"`
		ExpiresAt       *time.Time      `json:"expires_at"`
	} `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

type orderDetailPayload struct {
	ID              int64           `json:"id"`
	UserID          int64           `json:"user_id"`
	Provider        string          `json:"provider"`
	Amount          int64           `json:"amount"`
	Status          string          `json:"status"`
	CreditedCredits int64           `json:"credited_credits"`
	ManualCompleted bool            `json:"manual_completed"`
	ManualReason    *string         `json:"manual_reason"`
	RefundedAmount  int64           `json:"refunded_amount"`
	RefundedAt      *time.Time      `json:"refunded_at"`
	RefundReason    *string         `json:"refund_reason"`
	PaidAt          *time.Time      `json:"paid_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	ExpiresAt       *time.Time      `json:"expires_at"`
	RawCallback     json.RawMessage `json:"raw_callback"`
}

func decodeOrderDetail(t *testing.T, rr *httptest.ResponseRecorder) orderDetailPayload {
	t.Helper()
	var view orderDetailPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode order view: %v (body=%s)", err, rr.Body.String())
	}
	return view
}

func TestOrdersListPaginationFilterAndScope(t *testing.T) {
	stripe, _ := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	other := env.createUser(0)

	aPending := env.insertOrder(owner, "pending", 1000, "")
	aSucceeded := env.insertOrder(owner, "succeeded", 2000, "")
	aCancelled := env.insertOrder(owner, "cancelled", 3000, "")
	bPending := env.insertOrder(other, "pending", 4000, "")
	bSucceeded := env.insertOrder(other, "succeeded", 5000, "")

	first := env.request(owner, http.MethodGet, "/api/orders?p=1&page_size=2", nil, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", first.Code, first.Body.String())
	}
	var page1 orderListPayload
	if err := json.Unmarshal(first.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if page1.Total != 3 || page1.Page != 1 || page1.PageSize != 2 {
		t.Fatalf("envelope = total %d page %d size %d", page1.Total, page1.Page, page1.PageSize)
	}
	if len(page1.Items) != 2 || page1.Items[0].ID != aCancelled || page1.Items[1].ID != aSucceeded {
		t.Fatalf("page1 items = %+v, want newest first [%d %d]", page1.Items, aCancelled, aSucceeded)
	}
	for _, item := range page1.Items {
		if item.UserID != owner || item.ID == bPending || item.ID == bSucceeded {
			t.Fatalf("user A list leaked another user's order: %+v", item)
		}
	}

	page2 := env.request(owner, http.MethodGet, "/api/orders?p=2&page_size=2", nil, nil)
	var page2Payload orderListPayload
	if err := json.Unmarshal(page2.Body.Bytes(), &page2Payload); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(page2Payload.Items) != 1 || page2Payload.Items[0].ID != aPending {
		t.Fatalf("page2 items = %+v", page2Payload.Items)
	}

	filtered := env.request(owner, http.MethodGet, "/api/orders?status=succeeded", nil, nil)
	var filteredPayload orderListPayload
	if err := json.Unmarshal(filtered.Body.Bytes(), &filteredPayload); err != nil {
		t.Fatalf("decode filtered: %v", err)
	}
	if filteredPayload.Total != 1 || len(filteredPayload.Items) != 1 ||
		filteredPayload.Items[0].ID != aSucceeded {
		t.Fatalf("status filter = %+v", filteredPayload)
	}

	searched := env.request(owner, http.MethodGet,
		"/api/orders?q="+strconv.FormatInt(aSucceeded, 10), nil, nil)
	var searchedPayload orderListPayload
	if err := json.Unmarshal(searched.Body.Bytes(), &searchedPayload); err != nil {
		t.Fatalf("decode searched: %v", err)
	}
	if searchedPayload.Total != 1 || searchedPayload.Items[0].ID != aSucceeded {
		t.Fatalf("q search = %+v", searchedPayload)
	}

	invalidStatus := env.request(owner, http.MethodGet, "/api/orders?status=paid", nil, nil)
	if invalidStatus.Code != http.StatusBadRequest {
		t.Fatalf("invalid status code = %d, want 400", invalidStatus.Code)
	}
	tooBig := env.request(owner, http.MethodGet, "/api/orders?page_size=101", nil, nil)
	if tooBig.Code != http.StatusBadRequest {
		t.Fatalf("page_size=101 code = %d, want 400", tooBig.Code)
	}

	// 用户显式询问他人 user_id → 403，不得退化成「静默过滤掉」。
	crossUser := env.request(owner, http.MethodGet,
		fmt.Sprintf("/api/orders?user_id=%d", other), nil, nil)
	if crossUser.Code != http.StatusForbidden {
		t.Fatalf("cross user filter code = %d, want 403", crossUser.Code)
	}

	admin := &auth.Principal{UserID: owner, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersRead}}
	adminList := env.requestAs(admin, http.MethodGet,
		fmt.Sprintf("/api/orders?user_id=%d", other), nil, nil)
	if adminList.Code != http.StatusOK {
		t.Fatalf("admin list status = %d, body = %s", adminList.Code, adminList.Body.String())
	}
	var adminPayload orderListPayload
	if err := json.Unmarshal(adminList.Body.Bytes(), &adminPayload); err != nil {
		t.Fatalf("decode admin list: %v", err)
	}
	if adminPayload.Total != 2 {
		t.Fatalf("admin total = %d, want 2", adminPayload.Total)
	}

	// 无 orders:read 的 admin 不能跨用户查询。
	noRead := &auth.Principal{UserID: owner, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersManage}}
	if rr := env.requestAs(noRead, http.MethodGet,
		fmt.Sprintf("/api/orders?user_id=%d", other), nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin without orders:read code = %d, want 403", rr.Code)
	}
}

func TestOrderDetailOwnershipAndRawCallbackVisibility(t *testing.T) {
	stripe, _ := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	other := env.createUser(0)
	orderID := env.insertOrder(other, "pending", 1000, "")
	env.insertOrderRawCallback(orderID, `{"provider":"stripe","event_id":"evt_secret"}`)

	own := env.request(other, http.MethodGet, fmt.Sprintf("/api/orders/%d", orderID), nil, nil)
	if own.Code != http.StatusOK {
		t.Fatalf("owner detail status = %d, body = %s", own.Code, own.Body.String())
	}
	view := decodeOrderDetail(t, own)
	if view.ID != orderID || view.Status != "pending" || view.ExpiresAt == nil {
		t.Fatalf("owner detail = %+v", view)
	}
	if len(view.RawCallback) != 0 || strings.Contains(own.Body.String(), "raw_callback") {
		t.Fatalf("user detail leaked raw_callback: %s", own.Body.String())
	}

	// 他人订单：404（不泄漏存在性）。
	if rr := env.request(owner, http.MethodGet,
		fmt.Sprintf("/api/orders/%d", orderID), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("cross user detail code = %d, want 404", rr.Code)
	}

	admin := &auth.Principal{UserID: owner, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersRead}}
	adminView := env.requestAs(admin, http.MethodGet, fmt.Sprintf("/api/orders/%d", orderID), nil, nil)
	if adminView.Code != http.StatusOK {
		t.Fatalf("admin detail status = %d, body = %s", adminView.Code, adminView.Body.String())
	}
	var raw struct {
		RawCallback map[string]any `json:"raw_callback"`
	}
	if err := json.Unmarshal(adminView.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode admin detail: %v", err)
	}
	if raw.RawCallback["event_id"] != "evt_secret" {
		t.Fatalf("admin raw_callback = %+v", raw.RawCallback)
	}

	// admin 没有 orders:read：不获得全局范围，他人订单同样 404。
	noRead := &auth.Principal{UserID: owner, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersManage}}
	if rr := env.requestAs(noRead, http.MethodGet,
		fmt.Sprintf("/api/orders/%d", orderID), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("admin without orders:read code = %d, want 404", rr.Code)
	}
}

func TestOrderCancelStateMachine(t *testing.T) {
	stripe, _ := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	other := env.createUser(0)

	pending := env.insertOrder(owner, "pending", 1000, "")
	cancelled := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/cancel", pending), nil, nil)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancelled.Code, cancelled.Body.String())
	}
	if view := decodeOrderDetail(t, cancelled); view.Status != "cancelled" {
		t.Fatalf("cancelled order = %+v", view)
	}
	// 响应必须反映取消那一刻持久化的 updated_at（内存快照是插入时的旧值）。
	var dbUpdated time.Time
	if err := env.db.QueryRow(`SELECT updated_at FROM orders WHERE id = $1`, pending).Scan(&dbUpdated); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	cancelView := decodeOrderDetail(t, cancelled)
	if !cancelView.UpdatedAt.Equal(dbUpdated) {
		t.Fatalf("cancel response updated_at = %s, db = %s", cancelView.UpdatedAt, dbUpdated)
	}
	var dbStatus string
	if err := env.db.QueryRow(`SELECT status FROM orders WHERE id = $1`, pending).Scan(&dbStatus); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if dbStatus != "cancelled" {
		t.Fatalf("db status = %s, want cancelled", dbStatus)
	}

	// 终态不可再迁移：重复取消与过期订单均为 409。
	again := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/cancel", pending), nil, nil)
	if again.Code != http.StatusConflict || orderProblemType(t, again) != problemTypeOrderConflict {
		t.Fatalf("repeat cancel = %d %s", again.Code, again.Body.String())
	}
	expired := env.insertOrder(owner, "expired", 1000, "")
	expiredCancel := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/cancel", expired), nil, nil)
	if expiredCancel.Code != http.StatusConflict {
		t.Fatalf("expired cancel = %d, want 409", expiredCancel.Code)
	}

	// 他人订单 → 404，不泄漏存在性。
	foreign := env.insertOrder(other, "pending", 1000, "")
	if rr := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/cancel", foreign), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("cross user cancel = %d, want 404", rr.Code)
	}

	// admin orders:manage 可取消任意订单；只有 read 不够。
	admin := &auth.Principal{UserID: owner, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersManage}}
	if rr := env.requestAs(admin, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/cancel", foreign), nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("admin cancel = %d, body = %s", rr.Code, rr.Body.String())
	}
	readOnlyForeign := env.insertOrder(other, "pending", 1000, "")
	readOnly := &auth.Principal{UserID: owner, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersRead}}
	if rr := env.requestAs(readOnly, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/cancel", readOnlyForeign), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("read-only admin cancel = %d, want 404", rr.Code)
	}
}

func TestOrderVerifyCreditsOnce(t *testing.T) {
	stripe, state := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	other := env.createUser(0)
	orderID := env.insertOrder(owner, "pending", 1000, "pi_order")
	state.set(func(s *orderStripeStubState) {
		s.status = "succeeded"
		s.amount = 1000
		s.amountReceived = 1000
	})

	first := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("verify status = %d, body = %s", first.Code, first.Body.String())
	}
	var resp struct {
		Paid  bool               `json:"paid"`
		Order orderDetailPayload `json:"order"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if !resp.Paid || resp.Order.Status != "succeeded" || resp.Order.CreditedCredits != 1000 {
		t.Fatalf("verify response = %+v", resp)
	}
	// 响应视图必须来自入账后的数据库行，而不是入账前的内存快照。
	if resp.Order.PaidAt == nil {
		t.Fatal("verify response paid_at = null for a succeeded order (stale in-memory view)")
	}
	var walletRows int64
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'topup' AND reference_id = $2`,
		owner, strconv.FormatInt(orderID, 10)).Scan(&walletRows); err != nil {
		t.Fatalf("count wallet: %v", err)
	}
	if walletRows != 1 {
		t.Fatalf("wallet rows = %d, want 1", walletRows)
	}

	// 已成功的订单再次 verify：409，且绝不重复入账。
	repeat := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
	if repeat.Code != http.StatusConflict {
		t.Fatalf("repeat verify = %d, want 409", repeat.Code)
	}
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'topup' AND reference_id = $2`,
		owner, strconv.FormatInt(orderID, 10)).Scan(&walletRows); err != nil {
		t.Fatalf("count wallet after repeat: %v", err)
	}
	if walletRows != 1 {
		t.Fatalf("wallet rows after repeat = %d, want 1", walletRows)
	}

	// 他人订单 → 404；终态订单 → 409。
	foreign := env.insertOrder(other, "pending", 1000, "pi_order_foreign")
	if rr := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", foreign), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("cross user verify = %d, want 404", rr.Code)
	}
	expired := env.insertOrder(owner, "expired", 1000, "pi_order_expired")
	if rr := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", expired), nil, nil); rr.Code != http.StatusConflict {
		t.Fatalf("expired verify = %d, want 409", rr.Code)
	}
}

func TestOrderVerifyNotPaidAndProviderError(t *testing.T) {
	stripe, state := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	orderID := env.insertOrder(owner, "pending", 1000, "pi_order")

	notPaid := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
	if notPaid.Code != http.StatusOK {
		t.Fatalf("not paid verify status = %d, body = %s", notPaid.Code, notPaid.Body.String())
	}
	var resp struct {
		Paid  bool `json:"paid"`
		Order struct {
			Status string `json:"status"`
		} `json:"order"`
	}
	if err := json.Unmarshal(notPaid.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode not paid: %v", err)
	}
	if resp.Paid || resp.Order.Status != "pending" {
		t.Fatalf("not paid response = %+v", resp)
	}

	state.set(func(s *orderStripeStubState) { s.httpStatus = http.StatusInternalServerError })
	broken := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
	if broken.Code != http.StatusBadGateway {
		t.Fatalf("provider error verify = %d, want 502 (body=%s)", broken.Code, broken.Body.String())
	}
}

func TestOrderVerifyRateLimit(t *testing.T) {
	stripe, _ := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	orderID := env.insertOrder(owner, "pending", 1000, "pi_order")

	for i := 0; i < 5; i++ {
		rr := env.request(owner, http.MethodPost,
			fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate limited", i+1)
		}
	}
	sixth := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
	if sixth.Code != http.StatusTooManyRequests {
		t.Fatalf("6th verify = %d, want 429", sixth.Code)
	}
}

func TestOrderCompleteRequiresPaidAndReason(t *testing.T) {
	stripe, state := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	adminID := env.createUser(0)
	admin := &auth.Principal{UserID: adminID, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersManage}}
	readOnly := &auth.Principal{UserID: adminID, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersRead}}
	validReason := "人工核实渠道已收款，客服确认完成"

	paidOrder := env.insertOrder(owner, "pending", 1000, "pi_order")
	state.set(func(s *orderStripeStubState) {
		s.status = "succeeded"
		s.amount = 1000
		s.amountReceived = 1000
	})

	// 非管理员与只有 read 的 admin 都被功能级拒绝。
	if rr := env.request(owner, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", paidOrder),
		map[string]any{"manual_reason": validReason}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin complete = %d, want 403", rr.Code)
	}
	if rr := env.requestAs(readOnly, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", paidOrder),
		map[string]any{"manual_reason": validReason}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("read-only admin complete = %d, want 403", rr.Code)
	}

	// 原因长度（按 rune）必须 10–200。
	for _, reason := range []string{"九个字符不够长", strings.Repeat("a", 201)} {
		rr := env.requestAs(admin, http.MethodPost,
			fmt.Sprintf("/api/orders/%d/complete", paidOrder),
			map[string]any{"manual_reason": reason}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("reason %q code = %d, want 400", reason, rr.Code)
		}
	}

	// 渠道未确认收款 → 400 且不入账。
	unpaid := env.insertOrder(owner, "pending", 1000, "pi_unpaid")
	state.set(func(s *orderStripeStubState) { s.status = "requires_payment_method"; s.amountReceived = 0 })
	if rr := env.requestAs(admin, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", unpaid),
		map[string]any{"manual_reason": validReason}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("unpaid complete = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	var unpaidStatus string
	var unpaidCredits int64
	if err := env.db.QueryRow(`SELECT status, credited_credits FROM orders WHERE id = $1`, unpaid).
		Scan(&unpaidStatus, &unpaidCredits); err != nil {
		t.Fatalf("read unpaid: %v", err)
	}
	if unpaidStatus != "pending" || unpaidCredits != 0 {
		t.Fatalf("unpaid order mutated: %s/%d", unpaidStatus, unpaidCredits)
	}

	// 渠道查询失败 → 400。
	state.set(func(s *orderStripeStubState) { s.httpStatus = http.StatusBadGateway })
	if rr := env.requestAs(admin, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", paidOrder),
		map[string]any{"manual_reason": validReason}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("query failure complete = %d, want 400", rr.Code)
	}

	// 恢复渠道状态并完成：使用与 webhook 相同的幂等入账路径。
	state.set(func(s *orderStripeStubState) {
		s.httpStatus = 0
		s.status = "succeeded"
		s.amountReceived = 1000
	})
	done := env.requestAs(admin, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", paidOrder),
		map[string]any{"manual_reason": validReason}, nil)
	if done.Code != http.StatusOK {
		t.Fatalf("complete status = %d, body = %s", done.Code, done.Body.String())
	}
	view := decodeOrderDetail(t, done)
	if view.Status != "succeeded" || !view.ManualCompleted ||
		view.ManualReason == nil || *view.ManualReason != validReason {
		t.Fatalf("completed order = %+v", view)
	}
	var operator int64
	var credits int64
	if err := env.db.QueryRow(`SELECT manual_operator_id, credited_credits FROM orders WHERE id = $1`,
		paidOrder).Scan(&operator, &credits); err != nil {
		t.Fatalf("read completed order: %v", err)
	}
	if operator != adminID || credits != 1000 {
		t.Fatalf("operator/credits = %d/%d", operator, credits)
	}
	var walletRows int64
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'topup' AND reference_id = $2`,
		owner, strconv.FormatInt(paidOrder, 10)).Scan(&walletRows); err != nil {
		t.Fatalf("count wallet: %v", err)
	}
	if walletRows != 1 {
		t.Fatalf("wallet rows = %d, want 1", walletRows)
	}

	// 已终态订单再次 complete → 409。
	again := env.requestAs(admin, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", paidOrder),
		map[string]any{"manual_reason": validReason}, nil)
	if again.Code != http.StatusConflict {
		t.Fatalf("repeat complete = %d, want 409", again.Code)
	}
	expired := env.insertOrder(owner, "expired", 1000, "pi_order_expired")
	if rr := env.requestAs(admin, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/complete", expired),
		map[string]any{"manual_reason": validReason}, nil); rr.Code != http.StatusConflict {
		t.Fatalf("expired complete = %d, want 409", rr.Code)
	}
}

// TestOrderVerifyAdminScope 钉住 review round 1 的 P-18 裁决：verify 会触发
// 入账（状态变更），所以只读管理员（orders:read）不能替他人核实；
// orders:manage 才有跨用户范围。
func TestOrderVerifyAdminScope(t *testing.T) {
	stripe, state := newOrderStripeStub(t)
	env := newTopupHTTPTestEnv(t, stripe.URL)
	owner := env.createUser(0)
	adminID := env.createUser(0)
	orderID := env.insertOrder(owner, "pending", 1000, "pi_scope")
	state.set(func(s *orderStripeStubState) {
		s.status = "succeeded"
		s.amount = 1000
		s.amountReceived = 1000
	})

	readOnly := &auth.Principal{UserID: adminID, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersRead}}
	if rr := env.requestAs(readOnly, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("read-only admin verify = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}
	var status string
	var credits int64
	if err := env.db.QueryRow(`SELECT status, credited_credits FROM orders WHERE id = $1`, orderID).
		Scan(&status, &credits); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "pending" || credits != 0 {
		t.Fatalf("read-only admin changed order: %s/%d", status, credits)
	}

	manage := &auth.Principal{UserID: adminID, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermOrdersManage}}
	rr := env.requestAs(manage, http.MethodPost,
		fmt.Sprintf("/api/orders/%d/verify", orderID), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("orders:manage admin verify = %d, body = %s", rr.Code, rr.Body.String())
	}
	view := decodeOrderDetail(t, rr)
	// verify 响应用 {paid, order} 包装，这里直接解码包装体。
	var resp struct {
		Paid  bool               `json:"paid"`
		Order orderDetailPayload `json:"order"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode verify response: %v", err)
	}
	if !resp.Paid || resp.Order.Status != "succeeded" || resp.Order.PaidAt == nil {
		t.Fatalf("manage admin verify response = %+v (view=%+v)", resp, view)
	}
}

// ---------------------------------------------------------------------------
// S3：管理员退款端点
// ---------------------------------------------------------------------------

// TestRefundOrderEndpoint 验证 HTTP 面：settings:manage 才能退款；
// 非订阅/不可退状态 409；钱包退款成功返回更新后的订单视图。
func TestRefundOrderEndpoint(t *testing.T) {
	env := newTopupHTTPTestEnv(t, "http://127.0.0.1:1")
	userID := env.createUser(1_000)
	now := time.Now().UTC()

	// 一笔未开始、未消耗的钱包订阅订单（provider=wallet 不触渠道）。
	key := fmt.Sprintf("refund-http-%d", time.Now().UnixNano())
	var orderID int64
	if err := env.db.QueryRow(`
		INSERT INTO orders
			(user_id, provider, amount, currency, status, idempotency_key,
			 biz_kind, biz_ref_id, paid_at)
		VALUES ($1, 'wallet', 9900, 'CNY', 'succeeded', $2, 'subscribe', 1, now())
		RETURNING id`, userID, key).Scan(&orderID); err != nil {
		t.Fatalf("insert wallet order: %v", err)
	}
	var subID int64
	if err := env.db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 amount_total, amount_used, allow_wallet_overflow, order_id, currency,
			 price_paid_amount, period_days, reset_on_renew, grace_days)
		VALUES ($1, 1, 'HTTP Refund', 'active', $2, $3, 10000, 0, true, $4, 'CNY',
		        9900, 30, false, 0)
		RETURNING id`, userID, now.Add(24*time.Hour), now.Add(30*24*time.Hour),
		orderID).Scan(&subID); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM subscription_events WHERE subscription_id = $1`, subID)
		_, _ = env.db.Exec(`DELETE FROM subscriptions WHERE id = $1`, subID)
	})
	if _, err := env.db.Exec(`
		INSERT INTO wallet_transactions
			(user_id, type, amount, balance_after, status, reference_type, reference_id)
		VALUES ($1, 'subscription_purchase', -9900, 1000, 'confirmed', 'order', $2)`,
		userID, strconv.FormatInt(orderID, 10)); err != nil {
		t.Fatalf("insert wallet purchase: %v", err)
	}

	path := fmt.Sprintf("/api/orders/%d/refund", orderID)

	t.Run("普通用户 403", func(t *testing.T) {
		rr := env.request(userID, http.MethodPost, path,
			map[string]any{"reason": "想退款"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("仅有 orders:manage 的管理员 403", func(t *testing.T) {
		p := &auth.Principal{UserID: 9001, Role: rbac.RoleAdmin,
			Permissions: []string{rbac.PermOrdersManage}}
		rr := env.requestAs(p, http.MethodPost, path, map[string]any{"reason": "想退款"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d，期望 403（brief 冻结为 settings:manage）", rr.Code)
		}
	})

	t.Run("settings:manage 完成钱包退款", func(t *testing.T) {
		p := &auth.Principal{UserID: 9002, Role: rbac.RoleAdmin,
			Permissions: []string{rbac.PermSettingsManage}}
		rr := env.requestAs(p, http.MethodPost, path, map[string]any{"reason": "未开始使用"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var order orderDetailPayload
		if err := json.Unmarshal(rr.Body.Bytes(), &order); err != nil {
			t.Fatalf("decode order: %v", err)
		}
		if order.Status != "refunded" || order.RefundedAmount != 9900 || order.RefundedAt == nil {
			t.Fatalf("refund response = %+v", order)
		}
		var balance int64
		if err := env.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).
			Scan(&balance); err != nil {
			t.Fatalf("read balance: %v", err)
		}
		if balance != 10_900 {
			t.Fatalf("balance = %d，期望 10900", balance)
		}

		// 幂等重放仍为 200 且不再次回补。
		replay := env.requestAs(p, http.MethodPost, path, map[string]any{"reason": "重复"}, nil)
		if replay.Code != http.StatusOK {
			t.Fatalf("replay status = %d（body=%s）", replay.Code, replay.Body.String())
		}
		if err := env.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).
			Scan(&balance); err != nil {
			t.Fatalf("read balance after replay: %v", err)
		}
		if balance != 10_900 {
			t.Fatalf("replay 后 balance = %d，期望仍为 10900", balance)
		}
	})
}
