package billing

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

// 本文件是 M5 Task 3（Stripe + 支付宝充值 + 幂等入账）的计费侧验收。
//
// 集成测试需要真实 PostgreSQL（未设置 NEWTG_TEST_DB_DSN 时跳过）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test ./internal/billing/ -run 'TestTopup|TestStripe|TestAlipay|TestOrderCreate' -v
//
// 渠道协议用 httptest 桩验证：Stripe 校验 Bearer 与表单；支付宝用生成的
// RSA 密钥对完成 precreate 请求签名 / 响应验签 / notify 验签的完整往返。

var topupTestSeq atomic.Int64

// stripeTestConfig 返回可下单的 Stripe 配置（密钥为明文，测试无 Sealer）。
func stripeTestConfig() mapConfig {
	return mapConfig{
		ConfigOnlineEnabled:        json.RawMessage("true"),
		ConfigStripeEnabled:        json.RawMessage("true"),
		ConfigStripeSecretKey:      json.RawMessage(`"sk_test_123"`),
		ConfigStripeWebhookSecret:  json.RawMessage(`"whsec_test_123"`),
		ConfigStripePublishableKey: json.RawMessage(`"pk_test_123"`),
		ConfigQuotaPerUnit:         json.RawMessage("100"),
	}
}

// alipayTestConfig 返回可下单的支付宝配置。
func alipayTestConfig(gatewayURL, appID, privateKey, publicKey string) mapConfig {
	return mapConfig{
		ConfigOnlineEnabled:    json.RawMessage("true"),
		ConfigAlipayEnabled:    json.RawMessage("true"),
		ConfigAlipayAppID:      json.RawMessage(strconv.Quote(appID)),
		ConfigAlipayPrivateKey: json.RawMessage(strconv.Quote(privateKey)),
		ConfigAlipayPublicKey:  json.RawMessage(strconv.Quote(publicKey)),
		ConfigAlipayGatewayURL: json.RawMessage(strconv.Quote(gatewayURL)),
		ConfigAlipaySandbox:    json.RawMessage("false"),
		ConfigQuotaPerUnit:     json.RawMessage("100"),
	}
}

// newStripeTopupService 构造指向 httptest 桩的 Stripe 服务。
func newStripeTopupService(t *testing.T, gormDB *gorm.DB, baseURL string) *TopupService {
	t.Helper()
	svc, err := NewTopupService(TopupServiceConfig{
		DB:            gormDB,
		Config:        stripeTestConfig(),
		StripeBaseURL: baseURL,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	return svc
}

// newStripeTestServer 是 Stripe PaymentIntent 的 httptest 桩。
func newStripeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payment_intents" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk_test_123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("amount") == "" || r.PostForm.Get("currency") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"pi_test_%s","client_secret":"pi_test_%s_secret_abc"}`,
			r.PostForm.Get("amount"), r.PostForm.Get("amount"))
	}))
	t.Cleanup(server.Close)
	return server
}

// stripeSignatureHeader 生成 Stripe 的 t=...,v1=... 头。
func stripeSignatureHeader(secret string, timestamp int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

// stripeEventPayload 构造最小 succeeded 事件。
func stripeEventPayload(intentID string, amount int64, currency string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":%q,"amount":%d,"amount_received":%d,"currency":%q,"status":"succeeded"}}}`,
		intentID, amount, amount, currency))
}

// alipayTestKeys 生成 RSA-2048 密钥对与 PEM（PKCS#8 / PKIX）。
func alipayTestKeys(t *testing.T) (privatePEM, publicPEM string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	privatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	publicPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privatePEM, publicPEM
}

// newAlipayTestServer 是支付宝网关桩：校验请求签名，返回带签名的响应。
// 服务端使用与客户端相同的密钥对（测试中二者同属"我方"）。
func newAlipayTestServer(t *testing.T, privatePEM, publicPEM string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		params := make(map[string]string, len(query))
		for key := range query {
			params[key] = query.Get(key)
		}
		if err := verifyRSA2(params, publicPEM); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad sign"}`))
			return
		}
		method := params["method"]
		var node string
		switch method {
		case "alipay.trade.precreate":
			node = fmt.Sprintf(`{"code":"10000","msg":"Success","out_trade_no":%q,"qr_code":"https://qr.test/pay"}`,
				outTradeNoFromBiz(params["biz_content"]))
		case "alipay.trade.query":
			node = `{"code":"10000","msg":"Success","trade_status":"TRADE_SUCCESS","total_amount":"10.00"}`
		case "alipay.trade.close":
			node = `{"code":"10000","msg":"Success"}`
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig, err := signRSA2Raw([]byte(node), privatePEM)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		nodeKey := "alipay_" + strings.ReplaceAll(method, ".", "_") + "_response"
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{%q:%s,"sign":%q}`, nodeKey, node, sig)
	}))
	t.Cleanup(server.Close)
	return server
}

func outTradeNoFromBiz(biz string) string {
	var parsed struct {
		OutTradeNo string `json:"out_trade_no"`
	}
	_ = json.Unmarshal([]byte(biz), &parsed)
	return parsed.OutTradeNo
}

// signRSA2Raw 对原始字节签名（响应节点签名口径）。
func signRSA2Raw(raw []byte, privatePEM string) (string, error) {
	key, err := parseRSAPrivateKey(privatePEM)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// alipayNotify 构造带 RSA2 签名的异步通知参数。
func alipayNotify(t *testing.T, privatePEM, appID, outTradeNo, totalAmount, status string) url.Values {
	t.Helper()
	params := map[string]string{
		"app_id":       appID,
		"out_trade_no": outTradeNo,
		"trade_no":     "2026101922001400000000000001",
		"trade_status": status,
		"total_amount": totalAmount,
		"sign_type":    "RSA2",
	}
	sign, err := signRSA2(params, privatePEM)
	if err != nil {
		t.Fatalf("sign notify: %v", err)
	}
	params["sign"] = sign
	values := url.Values{}
	for key, value := range params {
		values.Set(key, value)
	}
	return values
}

// ---------------------------------------------------------------------------
// 配置面
// ---------------------------------------------------------------------------

func TestTopupOptionsDefaultsAndProviderGating(t *testing.T) {
	gormDB, _ := openBillingTestDB(t)

	// 未配置渠道：在线支付默认开启，但没有任何可用渠道。
	svc, err := NewTopupService(TopupServiceConfig{DB: gormDB, Config: mapConfig{}})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	opts, err := svc.Options(context.Background())
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if opts.MinTopup != DefaultMinTopupCents || opts.MaxTopup != MaxTopupCents {
		t.Fatalf("min/max = %d/%d, want %d/%d", opts.MinTopup, opts.MaxTopup, DefaultMinTopupCents, MaxTopupCents)
	}
	if len(opts.EnabledProviders) != 0 {
		t.Fatalf("enabled providers = %v, want empty", opts.EnabledProviders)
	}
	wantPresets := []int64{1000, 3000, 5000, 10000, 30000, 50000}
	if len(opts.AmountOptions) != len(wantPresets) {
		t.Fatalf("amount options = %v, want %v", opts.AmountOptions, wantPresets)
	}
	for i := range wantPresets {
		if opts.AmountOptions[i] != wantPresets[i] {
			t.Fatalf("amount options = %v, want %v", opts.AmountOptions, wantPresets)
		}
	}

	// 关闭总开关：即使渠道配置齐全也不可用（P-13）。
	closed := stripeTestConfig()
	closed[ConfigOnlineEnabled] = json.RawMessage("false")
	svc, _ = NewTopupService(TopupServiceConfig{DB: gormDB, Config: closed})
	opts, err = svc.Options(context.Background())
	if err != nil {
		t.Fatalf("Options(closed): %v", err)
	}
	if len(opts.EnabledProviders) != 0 {
		t.Fatalf("online disabled should hide providers, got %v", opts.EnabledProviders)
	}

	// 配置齐全：stripe 可用并下发 publishable_key。
	svc, _ = NewTopupService(TopupServiceConfig{DB: gormDB, Config: stripeTestConfig()})
	opts, err = svc.Options(context.Background())
	if err != nil {
		t.Fatalf("Options(stripe): %v", err)
	}
	if len(opts.EnabledProviders) != 1 || opts.EnabledProviders[0] != ProviderStripe {
		t.Fatalf("enabled providers = %v, want [stripe]", opts.EnabledProviders)
	}
	if opts.StripePublishableKey != "pk_test_123" {
		t.Fatalf("publishable key = %q", opts.StripePublishableKey)
	}
}

// ---------------------------------------------------------------------------
// 订单创建与幂等
// ---------------------------------------------------------------------------

func TestTopupCreateOrderIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("idem-%d", topupTestSeq.Add(1))

	first, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe, key)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if first.Replayed {
		t.Fatal("first create must not be a replay")
	}
	if first.ClientSecret == "" {
		t.Fatal("client_secret is required")
	}
	if first.Order.ProviderTxnID == nil || !strings.HasPrefix(*first.Order.ProviderTxnID, "pi_test_") {
		t.Fatalf("provider_txn_id = %v", first.Order.ProviderTxnID)
	}

	// 同 key 第二次：返回同一订单，不新建。
	second, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe, key)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if !second.Replayed || second.Order.ID != first.Order.ID {
		t.Fatalf("replay = %+v, want same order %d", second, first.Order.ID)
	}

	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != 1 {
		t.Fatalf("orders with key = %d, want 1", count)
	}
}

func TestTopupCreateOrderIdempotencyConflict(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	owner := createBillingTestUser(t, db, 0)
	other := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, owner)
	deleteTestOrders(t, db, other)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("conflict-%d", topupTestSeq.Add(1))
	if _, err := svc.CreateTopupOrder(context.Background(), owner, 1000, "CNY", ProviderStripe, key); err != nil {
		t.Fatalf("owner create: %v", err)
	}
	// P-2：全表唯一 key 被他人占用 → 冲突，绝不返回他人订单内容。
	_, err := svc.CreateTopupOrder(context.Background(), other, 1000, "CNY", ProviderStripe, key)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestTopupCreateOrderAmountValidation(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	cases := []struct {
		name   string
		amount int64
	}{
		{"below minimum", 999},
		{"zero", 0},
		{"negative", -100},
		{"above maximum", MaxTopupCents + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateTopupOrder(context.Background(), userID, tc.amount, "CNY", ProviderStripe,
				fmt.Sprintf("amt-%d-%d", topupTestSeq.Add(1), tc.amount))
			if !errors.Is(err, ErrInvalidTopupAmount) {
				t.Fatalf("err = %v, want ErrInvalidTopupAmount", err)
			}
		})
	}

	// 上限本身允许（含等于）。
	if _, err := svc.CreateTopupOrder(context.Background(), userID, MaxTopupCents, "CNY", ProviderStripe,
		fmt.Sprintf("amt-max-%d", topupTestSeq.Add(1))); err != nil {
		t.Fatalf("max amount should be allowed: %v", err)
	}
}

func TestOrderCreateProviderFailureMarksOrderFailed(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	svc := newStripeTopupService(t, gormDB, server.URL)

	key := fmt.Sprintf("fail-%d", topupTestSeq.Add(1))
	_, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe, key)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	var status, failure string
	if err := db.QueryRow(`SELECT status, failure_code FROM orders WHERE idempotency_key = $1`, key).
		Scan(&status, &failure); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "failed" || failure != "provider_unavailable" {
		t.Fatalf("status/failure = %s/%s, want failed/provider_unavailable", status, failure)
	}
}

// ---------------------------------------------------------------------------
// Stripe webhook
// ---------------------------------------------------------------------------

func TestStripeWebhookSignature(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	order, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe,
		fmt.Sprintf("sig-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	payload := stripeEventPayload(*order.Order.ProviderTxnID, 1000, "cny")
	now := time.Now()

	t.Run("bad signature", func(t *testing.T) {
		header := "t=" + strconv.FormatInt(now.Unix(), 10) + ",v1=" + strings.Repeat("0", 64)
		err := svc.HandleStripeWebhook(context.Background(), payload, header)
		if !errors.Is(err, ErrWebhookSignature) {
			t.Fatalf("err = %v, want ErrWebhookSignature", err)
		}
		assertOrderStatus(t, db, order.Order.ID, "pending")
		assertWalletCount(t, db, userID, 0)
	})

	t.Run("expired timestamp", func(t *testing.T) {
		old := now.Add(-StripeTimestampTolerance - time.Minute).Unix()
		header := stripeSignatureHeader("whsec_test_123", old, payload)
		err := svc.HandleStripeWebhook(context.Background(), payload, header)
		if !errors.Is(err, ErrWebhookSignature) {
			t.Fatalf("err = %v, want ErrWebhookSignature", err)
		}
		assertOrderStatus(t, db, order.Order.ID, "pending")
		assertWalletCount(t, db, userID, 0)
	})

	t.Run("future timestamp beyond tolerance", func(t *testing.T) {
		future := now.Add(StripeTimestampTolerance + time.Minute).Unix()
		header := stripeSignatureHeader("whsec_test_123", future, payload)
		err := svc.HandleStripeWebhook(context.Background(), payload, header)
		if !errors.Is(err, ErrWebhookSignature) {
			t.Fatalf("err = %v, want ErrWebhookSignature", err)
		}
	})

	t.Run("valid signature", func(t *testing.T) {
		header := stripeSignatureHeader("whsec_test_123", now.Unix(), payload)
		if err := svc.HandleStripeWebhook(context.Background(), payload, header); err != nil {
			t.Fatalf("HandleStripeWebhook: %v", err)
		}
	})
}

func TestStripeWebhookCreditsExactlyOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	order, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe,
		fmt.Sprintf("once-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	payload := stripeEventPayload(*order.Order.ProviderTxnID, 1000, "cny")
	header := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)

	for i := 0; i < 3; i++ {
		if err := svc.HandleStripeWebhook(context.Background(), payload, header); err != nil {
			t.Fatalf("callback %d: %v", i, err)
		}
	}

	var (
		status           string
		credited         int64
		paidAtValid      bool
		providerAmount   int64
		providerCurrency string
		rawCallback      []byte
	)
	if err := db.QueryRow(`SELECT status, credited_credits, paid_at IS NOT NULL,
		provider_amount, provider_currency, raw_callback FROM orders WHERE id = $1`, order.Order.ID).
		Scan(&status, &credited, &paidAtValid, &providerAmount, &providerCurrency, &rawCallback); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "succeeded" || !paidAtValid {
		t.Fatalf("status/paid_at = %s/%v, want succeeded/true", status, paidAtValid)
	}
	// 1000 分 = ¥10 × 100 积分/元 × 1.0 = 1000 积分。
	if credited != 1000 {
		t.Fatalf("credited_credits = %d, want 1000", credited)
	}
	if providerAmount != 1000 || providerCurrency != "CNY" {
		t.Fatalf("provider amount/currency = %d/%s", providerAmount, providerCurrency)
	}
	assertWalletCount(t, db, userID, 1)
	assertWalletReference(t, db, userID, "order", strconv.FormatInt(order.Order.ID, 10))

	var raw map[string]any
	if err := json.Unmarshal(rawCallback, &raw); err != nil {
		t.Fatalf("raw_callback is not json: %v", err)
	}
	if _, ok := raw["signature"]; ok {
		t.Fatal("raw_callback must not contain signature headers")
	}
	if got := raw["payment_intent"]; got != *order.Order.ProviderTxnID {
		t.Fatalf("raw_callback payment_intent = %v", got)
	}
}

func TestStripeWebhookAmountMismatch(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	order, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe,
		fmt.Sprintf("mismatch-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	payload := stripeEventPayload(*order.Order.ProviderTxnID, 999, "cny")
	header := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)

	err = svc.HandleStripeWebhook(context.Background(), payload, header)
	if !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("err = %v, want ErrAmountMismatch", err)
	}
	var status, failure string
	if err := db.QueryRow(`SELECT status, failure_code FROM orders WHERE id = $1`, order.Order.ID).
		Scan(&status, &failure); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "failed" || failure != "amount_mismatch" {
		t.Fatalf("status/failure = %s/%s, want failed/amount_mismatch", status, failure)
	}
	assertWalletCount(t, db, userID, 0)
}

func TestStripeWebhookCurrencyMismatch(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	order, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderStripe,
		fmt.Sprintf("currency-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	payload := stripeEventPayload(*order.Order.ProviderTxnID, 1000, "usd")
	header := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)

	err = svc.HandleStripeWebhook(context.Background(), payload, header)
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("err = %v, want ErrCurrencyMismatch", err)
	}
	var status, failure string
	if err := db.QueryRow(`SELECT status, failure_code FROM orders WHERE id = $1`, order.Order.ID).
		Scan(&status, &failure); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "failed" || failure != "currency_mismatch" {
		t.Fatalf("status/failure = %s/%s, want failed/currency_mismatch", status, failure)
	}
	assertWalletCount(t, db, userID, 0)
}

// ---------------------------------------------------------------------------
// 支付宝 RSA2
// ---------------------------------------------------------------------------

func TestAlipayPrecreateAndNotify(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	privatePEM, publicPEM := alipayTestKeys(t)
	gateway := newAlipayTestServer(t, privatePEM, publicPEM)
	appID := "2021000000000001"

	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	cfg := alipayTestConfig(gateway.URL, appID, privatePEM, publicPEM)
	svc, err := NewTopupService(TopupServiceConfig{DB: gormDB, Config: cfg})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	created, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderAlipay,
		fmt.Sprintf("alipay-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create alipay order: %v", err)
	}
	if created.QRCode != "https://qr.test/pay" {
		t.Fatalf("qr_code = %q", created.QRCode)
	}
	if created.Order.ProviderTxnID == nil || *created.Order.ProviderTxnID == "" {
		t.Fatal("out_trade_no must be stored before precreate returns")
	}
	outTradeNo := *created.Order.ProviderTxnID

	// 伪造签名：拒绝且不入账。
	forged := alipayNotify(t, privatePEM, appID, outTradeNo, "10.00", "TRADE_SUCCESS")
	forged.Set("sign", base64.StdEncoding.EncodeToString([]byte("not-a-signature")))
	if err := svc.HandleAlipayNotify(context.Background(), forged); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("forged notify err = %v, want ErrWebhookSignature", err)
	}
	assertOrderStatus(t, db, created.Order.ID, "pending")
	assertWalletCount(t, db, userID, 0)

	// 合法通知：入账一次；重复通知幂等。
	notify := alipayNotify(t, privatePEM, appID, outTradeNo, "10.00", "TRADE_SUCCESS")
	for i := 0; i < 3; i++ {
		if err := svc.HandleAlipayNotify(context.Background(), notify); err != nil {
			t.Fatalf("notify %d: %v", i, err)
		}
	}
	var status string
	var credited int64
	if err := db.QueryRow(`SELECT status, credited_credits FROM orders WHERE id = $1`, created.Order.ID).
		Scan(&status, &credited); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "succeeded" || credited != 1000 {
		t.Fatalf("status/credits = %s/%d, want succeeded/1000", status, credited)
	}
	assertWalletCount(t, db, userID, 1)
	assertWalletReference(t, db, userID, "order", strconv.FormatInt(created.Order.ID, 10))

	// 金额不一致：单独订单，置 failed 且不入账。
	second, err := svc.CreateTopupOrder(context.Background(), userID, 2000, "CNY", ProviderAlipay,
		fmt.Sprintf("alipay-mismatch-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create second order: %v", err)
	}
	mismatch := alipayNotify(t, privatePEM, appID, *second.Order.ProviderTxnID, "10.00", "TRADE_SUCCESS")
	if err := svc.HandleAlipayNotify(context.Background(), mismatch); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("mismatch err = %v, want ErrAmountMismatch", err)
	}
	var secondStatus, failure string
	if err := db.QueryRow(`SELECT status, failure_code FROM orders WHERE id = $1`, second.Order.ID).
		Scan(&secondStatus, &failure); err != nil {
		t.Fatalf("read second order: %v", err)
	}
	if secondStatus != "failed" || failure != "amount_mismatch" {
		t.Fatalf("second status/failure = %s/%s", secondStatus, failure)
	}
	assertWalletCount(t, db, userID, 1)
}

func TestAlipayNotifyIgnoresNonSuccessStatus(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	privatePEM, publicPEM := alipayTestKeys(t)
	gateway := newAlipayTestServer(t, privatePEM, publicPEM)
	appID := "2021000000000002"
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	svc, _ := NewTopupService(TopupServiceConfig{DB: gormDB,
		Config: alipayTestConfig(gateway.URL, appID, privatePEM, publicPEM)})
	order, err := svc.CreateTopupOrder(context.Background(), userID, 1000, "CNY", ProviderAlipay,
		fmt.Sprintf("alipay-wait-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	waiting := alipayNotify(t, privatePEM, appID, *order.Order.ProviderTxnID, "10.00", "WAIT_BUYER_PAY")
	if err := svc.HandleAlipayNotify(context.Background(), waiting); err != nil {
		t.Fatalf("WAIT_BUYER_PAY should be ignored without error: %v", err)
	}
	assertOrderStatus(t, db, order.Order.ID, "pending")
	assertWalletCount(t, db, userID, 0)
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// deleteTestOrders 在用户清理之外删除订单（orders 无 FK），并以 cleanup
// 保证即使测试失败也不污染共享库。
func deleteTestOrders(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM orders WHERE user_id = $1`, userID) })
	_, _ = db.Exec(`DELETE FROM orders WHERE user_id = $1`, userID)
}

func assertOrderStatus(t *testing.T, db *sql.DB, orderID int64, want string) {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		t.Fatalf("read order %d status: %v", orderID, err)
	}
	if status != want {
		t.Fatalf("order %d status = %s, want %s", orderID, status, want)
	}
}

func assertWalletCount(t *testing.T, db *sql.DB, userID, want int64) {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'topup'`,
		userID).Scan(&count); err != nil {
		t.Fatalf("count wallet rows: %v", err)
	}
	if count != want {
		t.Fatalf("topup wallet rows = %d, want %d", count, want)
	}
}

func assertWalletReference(t *testing.T, db *sql.DB, userID int64, refType, refID string) {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'topup' AND reference_type = $2 AND reference_id = $3`,
		userID, refType, refID).Scan(&count); err != nil {
		t.Fatalf("count wallet refs: %v", err)
	}
	if count != 1 {
		t.Fatalf("wallet rows with reference %s/%s = %d, want 1", refType, refID, count)
	}
}
