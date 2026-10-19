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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
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

	h, err := NewTopupHandlers(TopupHandlerConfig{
		DB:            gormDB,
		Redis:         rdb,
		Config:        topupHTTPTestConfig(stripeBaseURL),
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
	if userID > 0 {
		req = req.WithContext(auth.WithPrincipal(req.Context(),
			&auth.Principal{UserID: userID, Role: "user"}))
	}
	return serve(e.router, req)
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
		AmountOptions    []int64                       `json:"amount_options"`
		MinTopup         int64                         `json:"min_topup"`
		MaxTopup         int64                         `json:"max_topup"`
		QuotaPerUnit     int64                         `json:"quota_per_unit"`
		EnabledProviders []string                      `json:"enabled_providers"`
		Providers        map[string]map[string]*string `json:"providers"`
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
	if len(resp.EnabledProviders) != 1 || resp.EnabledProviders[0] != billing.ProviderStripe {
		t.Fatalf("enabled providers = %v", resp.EnabledProviders)
	}
	pk := resp.Providers[billing.ProviderStripe]["publishable_key"]
	if pk == nil || *pk != "pk_test_123" {
		t.Fatalf("publishable_key = %v", pk)
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
