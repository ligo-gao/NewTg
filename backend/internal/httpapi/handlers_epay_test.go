package httpapi

// Epay 回调端点的 HTTP 验收（modules-04 F-04-21；C1）。
//
// 复用 handlers_topup_test.go 的 env（真实 PostgreSQL + miniredis）。
// 渠道侧没有外呼，api_url 用 httptest TLS 服务器只为满足 https 校验。

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/billing"
)

func epayTLSStub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

// withEpayConfig 在既有充值配置上打开 epay。
func withEpayConfig(apiURL string) func(topupConfigMap) {
	return func(cfg topupConfigMap) {
		cfg[billing.ConfigEpayEnabled] = json.RawMessage("true")
		cfg[billing.ConfigEpayAPIURL] = json.RawMessage(strconv.Quote(apiURL))
		cfg[billing.ConfigEpayPID] = json.RawMessage(`"1001"`)
		cfg[billing.ConfigEpayKey] = json.RawMessage(`"testkey"`)
		cfg[billing.ConfigSiteServerAddress] = json.RawMessage(`"https://tg.example.com"`)
	}
}

func epayCallbackForm(txn, money, status string) url.Values {
	flat := map[string]string{
		"pid":          "1001",
		"trade_no":     "202610200001",
		"out_trade_no": txn,
		"type":         "wxpay",
		"name":         "NewTG order",
		"money":        money,
		"trade_status": status,
	}
	flat["sign"] = epaySignForTest(flat, "testkey")
	flat["sign_type"] = "MD5"
	form := url.Values{}
	for key, value := range flat {
		form.Set(key, value)
	}
	return form
}

func (e *topupTestEnv) postEpayForm(method string, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	path := "/api/webhooks/epay"
	var req *http.Request
	if method == http.MethodGet {
		req = httptest.NewRequest(method, path+"?"+form.Encode(), http.NoBody)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return serve(e.router, req)
}

func TestEpayWebhookCreditsOnceAndIsIdempotent(t *testing.T) {
	server := epayTLSStub(t)
	env := newTopupHTTPTestEnvWithConfig(t, "", withEpayConfig(server.URL))
	userID := env.createUser(0)

	rr := env.request(userID, http.MethodPost, "/api/orders", map[string]any{
		"amount": 1000, "provider": "epay",
	}, map[string]string{"Idempotency-Key": "epay-http-" + strconv.FormatInt(nowSeq(), 10)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create order status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create order: %v", err)
	}
	if created.PayURL != server.URL+"/submit.php" || created.FormFields["money"] != "10.00" {
		t.Fatalf("epay 支付载荷 = %+v", created)
	}
	txn := created.FormFields["out_trade_no"]

	form := epayCallbackForm(txn, "10.00", "TRADE_SUCCESS")
	for i := 0; i < 2; i++ {
		rr = env.postEpayForm(http.MethodPost, form)
		if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "success" {
			t.Fatalf("callback #%d = %d %q", i+1, rr.Code, rr.Body.String())
		}
	}
	// GET 变体同样可入账（此时订单已 succeeded，幂等返回 success）。
	rr = env.postEpayForm(http.MethodGet, form)
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "success" {
		t.Fatalf("GET callback = %d %q", rr.Code, rr.Body.String())
	}

	var status string
	var credited int64
	if err := env.db.QueryRow(`SELECT status, credited_credits FROM orders WHERE id = $1`,
		created.Order.ID).Scan(&status, &credited); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "succeeded" || credited != 1000 {
		t.Fatalf("order = %s/%d", status, credited)
	}
	var balance int64
	if err := env.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != 1000 {
		t.Fatalf("balance = %d，期望 1000", balance)
	}
}

func TestEpayWebhookGetFirstDeliveryCredits(t *testing.T) {
	server := epayTLSStub(t)
	env := newTopupHTTPTestEnvWithConfig(t, "", withEpayConfig(server.URL))
	userID := env.createUser(0)

	rr := env.request(userID, http.MethodPost, "/api/orders", map[string]any{
		"amount": 1000, "provider": "epay",
	}, map[string]string{"Idempotency-Key": "epay-get-" + strconv.FormatInt(nowSeq(), 10)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create order status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create order: %v", err)
	}

	// 首次投递即 GET（query 形态）也必须入账：渠道可能只用 GET 回调。
	rr = env.postEpayForm(http.MethodGet,
		epayCallbackForm(created.FormFields["out_trade_no"], "10.00", "TRADE_SUCCESS"))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "success" {
		t.Fatalf("GET first delivery = %d %q", rr.Code, rr.Body.String())
	}

	var status string
	var credited int64
	if err := env.db.QueryRow(`SELECT status, credited_credits FROM orders WHERE id = $1`,
		created.Order.ID).Scan(&status, &credited); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "succeeded" || credited != 1000 {
		t.Fatalf("order = %s/%d", status, credited)
	}
	var balance int64
	if err := env.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != 1000 {
		t.Fatalf("balance = %d，期望 1000", balance)
	}
}

// TestEpayWebhookRateLimitedFailClosed：超过每分钟阈值后返回 429 + fail，
// 且不进入验签/入账（资金入口的限流不可降级）。
func TestEpayWebhookRateLimitedFailClosed(t *testing.T) {
	server := epayTLSStub(t)
	env := newTopupHTTPTestEnvWithConfig(t, "", withEpayConfig(server.URL))
	userID := env.createUser(0)

	rr := env.request(userID, http.MethodPost, "/api/orders", map[string]any{
		"amount": 1000, "provider": "epay",
	}, map[string]string{"Idempotency-Key": "epay-429-" + strconv.FormatInt(nowSeq(), 10)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create order status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create order: %v", err)
	}

	// 预置当前窗口计数为阈值：下一条请求必然 429（避免循环 61 次的窗口
	// 边界抖动）。
	bucket := time.Now().Unix() / int64(webhookRateWindow/time.Second)
	key := webhookRateEpay + strconv.FormatInt(bucket, 10)
	if err := env.rdb.Set(context.Background(), key, webhookRateLimit, time.Minute).Err(); err != nil {
		t.Fatalf("seed rate limiter: %v", err)
	}

	rr = env.postEpayForm(http.MethodPost,
		epayCallbackForm(created.FormFields["out_trade_no"], "10.00", "TRADE_SUCCESS"))
	if rr.Code != http.StatusTooManyRequests || strings.TrimSpace(rr.Body.String()) != "fail" {
		t.Fatalf("rate limited = %d %q", rr.Code, rr.Body.String())
	}
	var status string
	if err := env.db.QueryRow(`SELECT status FROM orders WHERE id = $1`,
		created.Order.ID).Scan(&status); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "pending" {
		t.Fatalf("限流请求不得入账，订单状态 = %s", status)
	}
	var balance int64
	if err := env.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).
		Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != 0 {
		t.Fatalf("限流请求不得入账，余额 = %d", balance)
	}
}

func TestEpayWebhookRejectsBadSignature(t *testing.T) {
	server := epayTLSStub(t)
	env := newTopupHTTPTestEnvWithConfig(t, "", withEpayConfig(server.URL))
	form := epayCallbackForm("TGEP999", "10.00", "TRADE_SUCCESS")
	form.Set("sign", "deadbeef")
	rr := env.postEpayForm(http.MethodPost, form)
	if rr.Code != http.StatusBadRequest || strings.TrimSpace(rr.Body.String()) != "fail" {
		t.Fatalf("bad signature = %d %q", rr.Code, rr.Body.String())
	}
}

func TestEpayWebhookUnknownOrderDoesNotLeak(t *testing.T) {
	server := epayTLSStub(t)
	env := newTopupHTTPTestEnvWithConfig(t, "", withEpayConfig(server.URL))
	rr := env.postEpayForm(http.MethodPost, epayCallbackForm("TGEP404404", "10.00", "TRADE_SUCCESS"))
	if rr.Code != http.StatusBadRequest || strings.TrimSpace(rr.Body.String()) != "fail" {
		t.Fatalf("unknown order = %d %q（应与验签失败同形状）", rr.Code, rr.Body.String())
	}
}

func TestEpayWebhookDisabledReturns503(t *testing.T) {
	// 默认配置没有 pay.epay.* ⇒ 未启用。
	env := newTopupHTTPTestEnv(t, "")
	rr := env.postEpayForm(http.MethodPost, epayCallbackForm("TGEP1", "10.00", "TRADE_SUCCESS"))
	if rr.Code != http.StatusServiceUnavailable || strings.TrimSpace(rr.Body.String()) != "fail" {
		t.Fatalf("disabled = %d %q", rr.Code, rr.Body.String())
	}
}

func nowSeq() int64 {
	return time.Now().UnixNano()
}

// epaySignForTest 是协议签名的独立实现（不调用 billing 的私有函数），
// 用固定口径生成回调签名：排除 sign/sign_type 与空值，升序拼接后直接
// 追加 key（标准易支付 `$signstr.$key`，key 前没有 `&`），再 md5。
func epaySignForTest(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for name, value := range params {
		if name == "sign" || name == "sign_type" || value == "" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, name := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(params[name])
	}
	b.WriteString(key)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
