package billing

// Epay（易支付）渠道验收（modules-04 F-04-21；C1）。
//
// 与 topup_test.go 同一手法：需要真实 PostgreSQL，未设置 NEWTG_TEST_DB_DSN
// 时跳过。收银台是本地表单，不需要渠道 API 桩；api_url 用 httptest TLS
// 服务器满足 https 校验，测试只验证表单字段与签名向量。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"gorm.io/gorm"
)

const epayTestKey = "testkey"

func epayTestConfig(apiURL string) mapConfig {
	return mapConfig{
		ConfigOnlineEnabled:     json.RawMessage("true"),
		ConfigEpayEnabled:       json.RawMessage("true"),
		ConfigEpayAPIURL:        json.RawMessage(strconv.Quote(apiURL)),
		ConfigEpayPID:           json.RawMessage(`"1001"`),
		ConfigEpayKey:           json.RawMessage(`"testkey"`),
		ConfigSiteServerAddress: json.RawMessage(`"https://tg.example.com"`),
		ConfigQuotaPerUnit:      json.RawMessage("100"),
	}
}

func newEpayTopupService(t *testing.T, gormDB *gorm.DB, apiURL string) *TopupService {
	t.Helper()
	svc, err := NewTopupService(TopupServiceConfig{DB: gormDB, Config: epayTestConfig(apiURL)})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	return svc
}

// epayNotifyParams 构造一条回调的字段集合（未签名）；测试可在改字段后
// 重新签名，验证签名合法但语义非法的情况。
func epayNotifyParams(txn, money, status string) map[string]string {
	return map[string]string{
		"pid":          "1001",
		"trade_no":     "202610200001",
		"out_trade_no": txn,
		"type":         "wxpay",
		"name":         "NewTG order",
		"money":        money,
		"trade_status": status,
	}
}

// epayNotifyForm 构造一条已签名的回调；sign_type 刻意加入，用来证明它不
// 参与签名（标准易支付网关会附带该字段）。
func epayNotifyForm(txn, money, status string) url.Values {
	flat := epayNotifyParams(txn, money, status)
	flat["sign"] = epaySign(flat, epayTestKey)
	flat["sign_type"] = "MD5"
	return epayValues(flat)
}

func epayValues(flat map[string]string) url.Values {
	form := url.Values{}
	for key, value := range flat {
		form.Set(key, value)
	}
	return form
}

// TestEpaySignatureVector 固定样例：任何规范化/排序/密钥拼接的回归都会
// 改变哈希，立即失败。
//
// 期望值由独立脚本按标准易支付算法（升序 a=b&… 后**直接**拼接 key，无 `&`）
// 用 Python hashlib 计算，不是从本实现反推。
func TestEpaySignatureVector(t *testing.T) {
	fields := map[string]string{
		"pid":          "1001",
		"type":         "wxpay",
		"out_trade_no": "TGEP1",
		"notify_url":   "https://example.com/api/webhooks/epay",
		"return_url":   "https://example.com/wallet",
		"name":         "Test",
		"money":        "10.00",
		"param":        "",
		"sign_type":    "MD5",
		"device":       "pc",
	}
	canonical := "device=pc&money=10.00&name=Test&notify_url=https://example.com/api/webhooks/epay" +
		"&out_trade_no=TGEP1&pid=1001&return_url=https://example.com/wallet&type=wxpay"
	if got := epayCanonical(fields); got != canonical {
		t.Fatalf("canonical = %q\n期望 %q", got, canonical)
	}
	if got, want := epaySign(fields, epayTestKey), "baf5c7e32cb08e09e976bd6e14b9162f"; got != want {
		t.Fatalf("sign = %s，期望 %s", got, want)
	}

	notify := map[string]string{
		"pid":          "1001",
		"trade_no":     "202610200001",
		"out_trade_no": "TGEP1",
		"type":         "wxpay",
		"name":         "Test",
		"money":        "10.00",
		"trade_status": "TRADE_SUCCESS",
		"sign_type":    "MD5",
	}
	notify["sign"] = epaySign(notify, epayTestKey)
	if notify["sign"] != "22f0f3b554c28fd028cecbb93d0c9f64" {
		t.Fatalf("notify sign = %s，期望 22f0f3b554c28fd028cecbb93d0c9f64", notify["sign"])
	}
	if err := VerifyEpaySignature(notify, epayTestKey); err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
	notify["money"] = "10.01"
	if err := VerifyEpaySignature(notify, epayTestKey); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("篡改金额应验签失败，实际 %v", err)
	}
}

// TestYuanToCentsContract 验证支付宝/易支付共用的金额解析只接受渠道契约
// 形状（`^\d+(\.\d{1,2})?$`）：指数、符号、3 位小数与空白都必须拒绝。
func TestYuanToCentsContract(t *testing.T) {
	valid := map[string]int64{
		"10": 1000, "10.0": 1000, "10.00": 1000, "0.01": 1, "99.99": 9999,
	}
	for in, want := range valid {
		got, err := yuanToCents(in)
		if err != nil || got != want {
			t.Fatalf("yuanToCents(%q) = %d, %v；期望 %d", in, got, err, want)
		}
	}
	for _, in := range []string{
		"10.000", "1e1", "+10.00", "-10.00", " 10.00", "10.00 ", "10.", ".5",
		"0", "0.00", "", "abc", "1,000.00",
	} {
		if _, err := yuanToCents(in); err == nil {
			t.Fatalf("yuanToCents(%q) 应拒绝", in)
		}
	}
}

// TestEpayFullFormExternalVector 是一条完整收银台表单的外部黄金向量：
// 字段与签名均由独立脚本按标准算法（canonical + key，key 前无 `&`）算出。
// 若签名口径被改成 `&key` 变体，此用例立即失败。
func TestEpayFullFormExternalVector(t *testing.T) {
	fields := map[string]string{
		"pid":          "1001",
		"type":         "wxpay",
		"out_trade_no": "TGEP42",
		"notify_url":   "https://tg.example.com/api/webhooks/epay",
		"return_url":   "https://tg.example.com/wallet?pay=epay",
		"name":         "NewTG order 42",
		"money":        "12.30",
		"param":        "",
		"sign_type":    "MD5",
		"device":       "pc",
	}
	const canonical = "device=pc&money=12.30&name=NewTG order 42" +
		"&notify_url=https://tg.example.com/api/webhooks/epay&out_trade_no=TGEP42" +
		"&pid=1001&return_url=https://tg.example.com/wallet?pay=epay&type=wxpay"
	if got := epayCanonical(fields); got != canonical {
		t.Fatalf("canonical = %q\n期望 %q", got, canonical)
	}
	fields["sign"] = epaySign(fields, "s3cr3t")
	const want = "81bd2a04e662c7a9ed22fd8e6091cc06"
	if fields["sign"] != want {
		t.Fatalf("sign = %s，期望 %s（标准算法 canonical+key）", fields["sign"], want)
	}
	if err := VerifyEpaySignature(fields, "s3cr3t"); err != nil {
		t.Fatalf("黄金向量应通过验签: %v", err)
	}
	// `&key` 变体（错误实现）必须验签失败：防止实现回退到错误拼接。
	fields["sign"] = "a42e7b0692cd6f4a7c6627b680720670"
	if err := VerifyEpaySignature(fields, "s3cr3t"); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("非标准签名应失败，实际 %v", err)
	}
}

// TestEpayTopupCheckoutAndNotify 覆盖下单表单、成功入账与重复回调幂等。
func TestEpayTopupCheckoutAndNotify(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	server := httptest.NewTLSServer(notFoundHandler())
	t.Cleanup(server.Close)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newEpayTopupService(t, gormDB, server.URL)
	ctx := context.Background()

	result, err := svc.CreateTopupOrder(ctx, userID, 1000, "CNY", ProviderEpay,
		"epay-topup-"+strconv.FormatInt(timeNowSeq(), 10))
	if err != nil {
		t.Fatalf("create epay topup order: %v", err)
	}
	if result.PayURL != server.URL+"/submit.php" {
		t.Fatalf("pay_url = %q", result.PayURL)
	}
	if got := result.FormFields["money"]; got != "10.00" {
		t.Fatalf("money = %q，期望 10.00", got)
	}
	if got := result.FormFields["notify_url"]; got != "https://tg.example.com/api/webhooks/epay" {
		t.Fatalf("notify_url = %q", got)
	}
	// 标准易支付字段：type（不是 type_out）、sign_type=MD5、device=pc。
	if got := result.FormFields["type"]; got != "wxpay" {
		t.Fatalf("type = %q，期望 wxpay", got)
	}
	if _, ok := result.FormFields["type_out"]; ok {
		t.Fatal("表单不得再出现 type_out（标准字段是 type）")
	}
	if result.FormFields["sign_type"] != "MD5" || result.FormFields["device"] != "pc" {
		t.Fatalf("sign_type/device = %q/%q",
			result.FormFields["sign_type"], result.FormFields["device"])
	}
	if err := VerifyEpaySignature(result.FormFields, epayTestKey); err != nil {
		t.Fatalf("下单表单签名无效: %v", err)
	}
	txn := result.FormFields["out_trade_no"]
	if txn == "" || result.Order.ProviderTxnID == nil || *result.Order.ProviderTxnID != txn {
		t.Fatalf("out_trade_no 未落库: %+v", result.Order)
	}

	form := epayNotifyForm(txn, "10.00", "TRADE_SUCCESS")
	// 重复投递 10 次：每次都应 nil（渠道收到 success），余额只加一次。
	for i := 0; i < 10; i++ {
		if err := svc.HandleEpayNotify(ctx, form); err != nil {
			t.Fatalf("notify #%d: %v", i+1, err)
		}
	}
	order, err := svc.GetOrder(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != string(OrderSucceeded) || order.CreditedCredits != 1000 {
		t.Fatalf("订单状态/入账 = %s/%d", order.Status, order.CreditedCredits)
	}
	if bal := userBalance(t, db, userID); bal != 1000 {
		t.Fatalf("余额 = %d，期望 1000（重复回调不得二次入账）", bal)
	}
	if count := walletTxCount(t, db, userID); count != 1 {
		t.Fatalf("钱包流水 = %d，期望 1", count)
	}
}

// TestEpayNotifyRejectsBadSignatureAndAmount 覆盖验签失败、金额不符与
// 小数位 >2：三种情况都不入账。
func TestEpayNotifyRejectsBadSignatureAndAmount(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	server := httptest.NewTLSServer(notFoundHandler())
	t.Cleanup(server.Close)
	svc := newEpayTopupService(t, gormDB, server.URL)
	ctx := context.Background()

	// 验签失败：订单保持 pending。
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	first, err := svc.CreateTopupOrder(ctx, userID, 1000, "CNY", ProviderEpay,
		"epay-badsig-"+strconv.FormatInt(timeNowSeq(), 10))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bad := epayNotifyForm(*first.Order.ProviderTxnID, "10.00", "TRADE_SUCCESS")
	bad.Set("sign", "deadbeef")
	if err := svc.HandleEpayNotify(ctx, bad); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("错误签名应返回 ErrWebhookSignature，实际 %v", err)
	}
	if order, _ := svc.GetOrder(ctx, first.Order.ID); order.Status != string(OrderPending) {
		t.Fatalf("验签失败后订单状态 = %s，期望 pending", order.Status)
	}

	// 金额不符：订单 failed，不入账。
	userID = createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	second, err := svc.CreateTopupOrder(ctx, userID, 1000, "CNY", ProviderEpay,
		"epay-mismatch-"+strconv.FormatInt(timeNowSeq(), 10))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := svc.HandleEpayNotify(ctx, epayNotifyForm(*second.Order.ProviderTxnID, "9.99", "TRADE_SUCCESS")); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("金额不符应返回 ErrAmountMismatch，实际 %v", err)
	}
	if order, _ := svc.GetOrder(ctx, second.Order.ID); order.Status != string(OrderFailed) {
		t.Fatalf("金额不符后订单状态 = %s，期望 failed", order.Status)
	}
	if bal := userBalance(t, db, userID); bal != 0 {
		t.Fatalf("金额不符不得入账，余额 = %d", bal)
	}

	// 3 位小数：载荷非法（不四舍五入）。
	userID = createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	third, err := svc.CreateTopupOrder(ctx, userID, 1000, "CNY", ProviderEpay,
		"epay-precision-"+strconv.FormatInt(timeNowSeq(), 10))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := svc.HandleEpayNotify(ctx, epayNotifyForm(*third.Order.ProviderTxnID, "10.001", "TRADE_SUCCESS")); !errors.Is(err, ErrWebhookPayload) {
		t.Fatalf("3 位小数应返回 ErrWebhookPayload，实际 %v", err)
	}
}

// TestEpayNotifyRejectsPIDMismatchAndIntermediateStatus 覆盖「签名合法但
// pid 不符/缺失」与「非 TRADE_SUCCESS 中间态」：前者 400 且不入账，后者
// 返回 nil（HTTP 层回 success 停止重投）但订单保持 pending。
func TestEpayNotifyRejectsPIDMismatchAndIntermediateStatus(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	server := httptest.NewTLSServer(notFoundHandler())
	t.Cleanup(server.Close)
	svc := newEpayTopupService(t, gormDB, server.URL)
	ctx := context.Background()

	newOrder := func(prefix string) (*TopupOrder, int64) {
		t.Helper()
		userID := createBillingTestUser(t, db, 0)
		deleteTestOrders(t, db, userID)
		order, err := svc.CreateTopupOrder(ctx, userID, 1000, "CNY", ProviderEpay,
			prefix+"-"+strconv.FormatInt(timeNowSeq(), 10))
		if err != nil {
			t.Fatalf("create order: %v", err)
		}
		return order, userID
	}

	// pid 不匹配：同密钥签出的合法回调也不得入账（多站点共用密钥场景）。
	first, userID := newOrder("epay-pid-wrong")
	params := epayNotifyParams(*first.Order.ProviderTxnID, "10.00", "TRADE_SUCCESS")
	params["pid"] = "9999"
	params["sign"] = epaySign(params, epayTestKey)
	if err := svc.HandleEpayNotify(ctx, epayValues(params)); !errors.Is(err, ErrWebhookPayload) {
		t.Fatalf("pid 不符应返回 ErrWebhookPayload，实际 %v", err)
	}
	if order, _ := svc.GetOrder(ctx, first.Order.ID); order.Status != string(OrderPending) {
		t.Fatalf("pid 不符后订单状态 = %s，期望 pending", order.Status)
	}
	if bal := userBalance(t, db, userID); bal != 0 {
		t.Fatalf("pid 不符不得入账，余额 = %d", bal)
	}

	// pid 缺失：同样拒绝（不能因为「没带 pid」跳过校验）。
	second, _ := newOrder("epay-pid-missing")
	params = epayNotifyParams(*second.Order.ProviderTxnID, "10.00", "TRADE_SUCCESS")
	delete(params, "pid")
	params["sign"] = epaySign(params, epayTestKey)
	if err := svc.HandleEpayNotify(ctx, epayValues(params)); !errors.Is(err, ErrWebhookPayload) {
		t.Fatalf("缺失 pid 应返回 ErrWebhookPayload，实际 %v", err)
	}

	// 中间态：返回 nil 但订单保持 pending、不入账。
	third, thirdUser := newOrder("epay-intermediate")
	if err := svc.HandleEpayNotify(ctx,
		epayNotifyForm(*third.Order.ProviderTxnID, "10.00", "WAIT_BUYER_PAY")); err != nil {
		t.Fatalf("中间态应返回 nil（停止重投），实际 %v", err)
	}
	if order, _ := svc.GetOrder(ctx, third.Order.ID); order.Status != string(OrderPending) {
		t.Fatalf("中间态后订单状态 = %s，期望 pending", order.Status)
	}
	if bal := userBalance(t, db, thirdUser); bal != 0 {
		t.Fatalf("中间态不得入账，余额 = %d", bal)
	}
}

// TestEpaySubscriptionPurchaseAndRenewal 覆盖订阅购买建实例与续费推进。
func TestEpaySubscriptionPurchaseAndRenewal(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	server := httptest.NewTLSServer(notFoundHandler())
	t.Cleanup(server.Close)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 123400, 30, true, "enabled", "")
	svc := newEpayTopupService(t, gormDB, server.URL)
	ctx := context.Background()

	purchase, err := svc.CreateSubscriptionOrder(ctx, userID, planID, ProviderEpay,
		"epay-sub-"+strconv.FormatInt(timeNowSeq(), 10))
	if err != nil {
		t.Fatalf("create epay subscription order: %v", err)
	}
	if purchase.PayURL == "" || purchase.FormFields["money"] != "99.00" {
		t.Fatalf("订阅下单载荷不完整: %+v", purchase)
	}
	if err := svc.HandleEpayNotify(ctx,
		epayNotifyForm(*purchase.Order.ProviderTxnID, "99.00", "TRADE_SUCCESS")); err != nil {
		t.Fatalf("subscription notify: %v", err)
	}
	sub := readSubscriptionByOrder(t, db, purchase.Order.ID)
	if sub == nil {
		t.Fatal("支付成功后应创建订阅实例")
	}
	// 重复回调不得重复建实例。
	if err := svc.HandleEpayNotify(ctx,
		epayNotifyForm(*purchase.Order.ProviderTxnID, "99.00", "TRADE_SUCCESS")); err != nil {
		t.Fatalf("duplicate subscription notify: %v", err)
	}
	var instances int
	if err := db.QueryRow(`SELECT count(*) FROM subscriptions WHERE order_id = $1`,
		purchase.Order.ID).Scan(&instances); err != nil || instances != 1 {
		t.Fatalf("订阅实例数 = %d, err=%v", instances, err)
	}

	renewal, err := svc.CreateSubscriptionRenewalOrder(ctx, userID, sub.ID, ProviderEpay,
		"epay-renew-"+strconv.FormatInt(timeNowSeq(), 10))
	if err != nil {
		t.Fatalf("create epay renewal order: %v", err)
	}
	if err := svc.HandleEpayNotify(ctx,
		epayNotifyForm(*renewal.Order.ProviderTxnID, "99.00", "TRADE_SUCCESS")); err != nil {
		t.Fatalf("renewal notify: %v", err)
	}
	// 续费复用同一实例：order_id 仍指向首次购买订单，因此按首次订单重读。
	renewed := readSubscriptionByOrder(t, db, purchase.Order.ID)
	if renewed == nil {
		t.Fatal("续费订单应推进同一实例")
	}
	if !renewed.EndAt.After(sub.EndAt) {
		t.Fatalf("续费后 end_at = %v，未晚于 %v", renewed.EndAt, sub.EndAt)
	}
	if renewed.ID != sub.ID {
		t.Fatalf("续费应复用同一实例 %d，实际 %d", sub.ID, renewed.ID)
	}
}

// TestEpayNotifyRejectedWhenDisabled：未启用的渠道即使签名正确也不入账。
func TestEpayNotifyRejectedWhenDisabled(t *testing.T) {
	gormDB, _ := openBillingTestDB(t)
	cfg := epayTestConfig("https://pay.example.com")
	cfg[ConfigEpayEnabled] = json.RawMessage("false")
	svc, err := NewTopupService(TopupServiceConfig{DB: gormDB, Config: cfg})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	if err := svc.HandleEpayNotify(context.Background(),
		epayNotifyForm("TGEP1", "10.00", "TRADE_SUCCESS")); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("未启用渠道应返回 ErrProviderDisabled，实际 %v", err)
	}
}

// notFoundHandler 让 httptest TLS 服务器有合法响应（本渠道不下单外呼）。
func notFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
}

func timeNowSeq() int64 { return topupTestSeq.Add(1) }
