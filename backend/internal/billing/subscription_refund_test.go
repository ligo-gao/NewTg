package billing

// S3 订阅购买退款的计费侧验收（P-13、R-30–R-34）。
//
// 与其它 billing 集成测试相同，需要真实 PostgreSQL；未设置
// NEWTG_TEST_DB_DSN 时跳过：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_s3?sslmode=disable' \
//	  go test ./internal/billing/ -run TestSubscriptionRefund -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 测试样本
// ---------------------------------------------------------------------------

// insertRefundOrder 直接落一笔订阅订单（退款用例绕过支付/回调，只验证退款）。
func insertRefundOrder(t *testing.T, db *sql.DB, userID int64, provider, status,
	bizKind string, bizRefID int64, amount int64, txnID string) int64 {

	t.Helper()
	key := fmt.Sprintf("refund-%d-%d", time.Now().UnixNano(), topupTestSeq.Add(1))
	var txn any
	if txnID != "" {
		txn = txnID
	}
	var paidAt any
	if status != string(OrderPending) {
		paidAt = time.Now().UTC()
	}
	var id int64
	if err := db.QueryRow(`
		INSERT INTO orders
			(user_id, provider, amount, currency, status, idempotency_key,
			 biz_kind, biz_ref_id, provider_txn_id, paid_at)
		VALUES ($1, $2, $3, 'CNY', $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		userID, provider, amount, status, key, bizKind, bizRefID, txn, paidAt).Scan(&id); err != nil {
		t.Fatalf("insert refund order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_events WHERE subscription_id IN
			(SELECT id FROM subscriptions WHERE order_id = $1 OR id = $2)`, id, bizRefID)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE order_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE reference_type = 'order' AND reference_id = $2`,
			id, strconv.FormatInt(id, 10))
		_, _ = db.Exec(`DELETE FROM orders WHERE id = $1`, id)
	})
	return id
}

// insertRefundSubscription 插入退款用例所需的订阅实例。
//
// startAt 由用例指定：P-13 的「未开始」判定完全依赖它，不能让测试的
// 创建时刻决定结果。
func insertRefundSubscription(t *testing.T, db *sql.DB, userID, planID int64,
	startAt time.Time, amountUsed int64, orderID *int64) int64 {

	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 order_id, currency, price_paid_amount, period_days, reset_on_renew, grace_days)
		VALUES ($1, $2, 'S3 Refund Plan', 'active', $3, $4, NULL, 10000, $5, true,
		        $6, 'CNY', 9900, 30, false, 0)
		RETURNING id`,
		userID, planID, startAt.UTC(), startAt.UTC().Add(30*24*time.Hour),
		amountUsed, orderID).Scan(&id)
	if err != nil {
		t.Fatalf("insert refund subscription: %v", err)
	}
	return id
}

func readOrderRefundState(t *testing.T, db *sql.DB, orderID int64) (status, failure string,
	refundedAmount int64, refundedAt *time.Time) {

	t.Helper()
	var failureCode sql.NullString
	var refunded sql.NullTime
	if err := db.QueryRow(`
		SELECT status, failure_code, refunded_amount, refunded_at
		  FROM orders WHERE id = $1`, orderID).
		Scan(&status, &failureCode, &refundedAmount, &refunded); err != nil {
		t.Fatalf("read order %d refund state: %v", orderID, err)
	}
	if failureCode.Valid {
		failure = failureCode.String
	}
	if refunded.Valid {
		at := refunded.Time
		refundedAt = &at
	}
	return status, failure, refundedAmount, refundedAt
}

func readSubscriptionStatus(t *testing.T, db *sql.DB, subscriptionID int64) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM subscriptions WHERE id = $1`,
		subscriptionID).Scan(&status); err != nil {
		t.Fatalf("read subscription %d status: %v", subscriptionID, err)
	}
	return status
}

// stripeRefundStub 记录退款调用；failFirst=true 时第一次返回 500，用于
// 验证 R-32 的「失败可重试且不二次出账」。
type stripeRefundStub struct {
	mu         sync.Mutex
	calls      int
	failFirst  bool
	lastKey    string
	lastIntent string
	lastAmount int64
}

func (s *stripeRefundStub) snapshot() (int, string, string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.lastKey, s.lastIntent, s.lastAmount
}

func newStripeRefundStub(t *testing.T, failFirst bool) (*httptest.Server, *stripeRefundStub) {
	t.Helper()
	state := &stripeRefundStub{failFirst: failFirst}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/refunds" || r.Method != http.MethodPost {
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
		amount, _ := strconv.ParseInt(r.PostForm.Get("amount"), 10, 64)
		state.mu.Lock()
		state.calls++
		state.lastKey = r.Header.Get("Idempotency-Key")
		state.lastIntent = r.PostForm.Get("payment_intent")
		state.lastAmount = amount
		fail := state.failFirst && state.calls == 1
		state.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"re_test_1","status":"succeeded","amount":%d,"currency":"cny"}`, amount)
	}))
	t.Cleanup(server.Close)
	return server, state
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestSubscriptionRefundRejectsUsedSubscription 覆盖 Step 1 / P-13：
// amount_used > 0 的订阅即使未开始也必须 422（ErrSubscriptionInUse），
// 且不得触碰渠道。
func TestSubscriptionRefundRejectsUsedSubscription(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe, state := newStripeRefundStub(t, false)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderStripe,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 9900, "pi_used")
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 5, &orderID)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	_, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "买错了", ActorUserID: 1,
	})
	if !errors.Is(err, ErrSubscriptionInUse) {
		t.Fatalf("err = %v，期望 ErrSubscriptionInUse", err)
	}
	status, _, amount, _ := readOrderRefundState(t, db, orderID)
	if status != string(OrderSucceeded) || amount != 0 {
		t.Fatalf("订单被改动: status=%s refunded_amount=%d", status, amount)
	}
	if calls, _, _, _ := state.snapshot(); calls != 0 {
		t.Fatalf("P-13 拒绝时不应调用渠道，calls=%d", calls)
	}
	if got := readSubscriptionStatus(t, db, subID); got != "active" {
		t.Fatalf("订阅状态 = %s，期望 active", got)
	}
}

// TestSubscriptionRefundWalletReturnsCredits 覆盖 Step 2 / R-31：
// 未开始且未消耗的钱包购买订单在同一事务内退款：订单 refunded、
// 钱包按原始扣减额回补、订阅取消、事件 detail 只含白名单字段；
// 重复调用命中幂等，不再回补。
func TestSubscriptionRefundWalletReturnsCredits(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 1_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderWallet,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 9900, "")
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 0, &orderID)
	if _, err := db.Exec(`
		INSERT INTO wallet_transactions
			(user_id, type, amount, balance_after, status, reference_type, reference_id)
		VALUES ($1, 'subscription_purchase', -9900, 1000, 'confirmed', 'order', $2)`,
		userID, strconv.FormatInt(orderID, 10)); err != nil {
		t.Fatalf("insert purchase wallet row: %v", err)
	}

	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	result, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "未开始使用", ActorUserID: 9,
	})
	if err != nil {
		t.Fatalf("wallet refund: %v", err)
	}
	if !result.Refunded || result.Amount != 9900 || result.Order.Status != string(OrderRefunded) {
		t.Fatalf("result = %+v / order=%+v", result, result.Order)
	}
	status, _, amount, refundedAt := readOrderRefundState(t, db, orderID)
	if status != string(OrderRefunded) || amount != 9900 || refundedAt == nil {
		t.Fatalf("订单 refunded 事实 = status:%s amount:%d at:%v", status, amount, refundedAt)
	}
	if got := readWalletBalance(t, db, userID); got != 10_900 {
		t.Fatalf("退款后余额 = %d，期望 10900（1000 + 9900）", got)
	}
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'reversal' AND reference_type = 'order' AND reference_id = $2`,
		userID, strconv.FormatInt(orderID, 10)).Scan(&count); err != nil {
		t.Fatalf("count reversal: %v", err)
	}
	if count != 1 {
		t.Fatalf("reversal 流水 = %d 行，期望 1", count)
	}
	if got := readSubscriptionStatus(t, db, subID); got != "cancelled" {
		t.Fatalf("订阅状态 = %s，期望 cancelled", got)
	}
	events := readSubscriptionEvents(t, db, subID)
	if len(events) != 1 || events[0].Kind != store.SubscriptionEventRefunded {
		t.Fatalf("事件 = %+v，期望一条 refunded", events)
	}
	var detail map[string]any
	if err := json.Unmarshal(events[0].Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail) != 3 || detail["order_id"] != float64(orderID) ||
		detail["amount"] != float64(9900) || detail["reason"] != "未开始使用" {
		t.Fatalf("refunded detail 只允许 order_id/amount/reason: %v", detail)
	}

	// R-31 幂等：重复退款返回既有结果，不再回补。
	replay, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "重复点击", ActorUserID: 9,
	})
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if replay.Refunded || replay.Amount != 9900 {
		t.Fatalf("replay = %+v，期望 Refunded=false / Amount=9900", replay)
	}
	if got := readWalletBalance(t, db, userID); got != 10_900 {
		t.Fatalf("幂等重放后余额 = %d，期望仍为 10900", got)
	}
}

// TestSubscriptionRefundProviderFailureIsRetryable 覆盖 Step 3 / R-32：
// 渠道 500 → 订单 refund_failed（不碰订阅/钱包），重试成功后 refunded；
// 渠道幂等键稳定，且只产生一条 refunded 事件。
func TestSubscriptionRefundProviderFailureIsRetryable(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe, state := newStripeRefundStub(t, true)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderStripe,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 9900, "pi_retry")
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 0, &orderID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	_, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "渠道故障", ActorUserID: 1,
	})
	if !errors.Is(err, ErrRefundProviderFailed) {
		t.Fatalf("err = %v，期望 ErrRefundProviderFailed", err)
	}
	status, failure, amount, _ := readOrderRefundState(t, db, orderID)
	if status != string(OrderRefundFailed) || amount != 0 || failure != "provider_unavailable" {
		t.Fatalf("失败后订单 = status:%s failure:%s amount:%d", status, failure, amount)
	}
	if got := readSubscriptionStatus(t, db, subID); got != "active" {
		t.Fatalf("渠道失败不得改动订阅，实际 = %s", got)
	}

	result, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "渠道故障", ActorUserID: 1,
	})
	if err != nil {
		t.Fatalf("retry refund: %v", err)
	}
	if !result.Refunded || result.Order.Status != string(OrderRefunded) {
		t.Fatalf("retry result = %+v", result)
	}
	calls, key, intent, refundAmount := state.snapshot()
	if calls != 2 || intent != "pi_retry" || refundAmount != 9900 {
		t.Fatalf("渠道调用 = calls:%d intent:%s amount:%d", calls, intent, refundAmount)
	}
	if key != fmt.Sprintf("refund-order-%d", orderID) {
		t.Fatalf("幂等键 = %q，期望 refund-order-%d", key, orderID)
	}
	if got := readSubscriptionStatus(t, db, subID); got != "cancelled" {
		t.Fatalf("退款成功后订阅状态 = %s，期望 cancelled", got)
	}
	refundedEvents := 0
	for _, event := range readSubscriptionEvents(t, db, subID) {
		if event.Kind == store.SubscriptionEventRefunded {
			refundedEvents++
		}
	}
	if refundedEvents != 1 {
		t.Fatalf("refunded 事件 = %d，期望 1", refundedEvents)
	}

	// 成功后再次调用：幂等命中，不触发第三次渠道调用。
	if _, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "再次点击", ActorUserID: 1,
	}); err != nil {
		t.Fatalf("post-success replay: %v", err)
	}
	if calls, _, _, _ := state.snapshot(); calls != 2 {
		t.Fatalf("成功后重放触发新渠道调用: calls=%d", calls)
	}
}

// TestSubscriptionRefundRequiredOrder 覆盖 R-30：S2 产生的 refund_required
// 订单（钱已收、履约失败）必须可退，且不因为订阅已消耗/已开始被 P-13 拒绝。
func TestSubscriptionRefundRequiredOrder(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe, _ := newStripeRefundStub(t, false)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(-10*24*time.Hour), 500, nil)
	orderID := insertRefundOrder(t, db, userID, ProviderStripe,
		string(OrderRefundRequired), store.OrderBizKindChange, subID, 4900, "pi_change")
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	result, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "套餐迁移失败", ActorUserID: 1,
	})
	if err != nil {
		t.Fatalf("refund_required refund: %v", err)
	}
	if !result.Refunded || result.Amount != 4900 || result.Order.Status != string(OrderRefunded) {
		t.Fatalf("result = %+v", result)
	}
	status, failure, amount, _ := readOrderRefundState(t, db, orderID)
	if status != string(OrderRefunded) || amount != 4900 || failure != "" {
		t.Fatalf("订单 = status:%s failure:%s amount:%d", status, failure, amount)
	}
	events := readSubscriptionEvents(t, db, subID)
	if len(events) != 1 || events[0].Kind != store.SubscriptionEventRefunded {
		t.Fatalf("事件 = %+v", events)
	}
}

// TestSubscriptionRefundEpayUnsupported：易支付没有退款 API，必须 fail closed
// 且不改订单状态（人工在渠道后台退款后由运营对账，而不是伪造成可重试失败）。
func TestSubscriptionRefundEpayUnsupported(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderEpay,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 9900, "epay_1")
	insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 0, &orderID)
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")

	_, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "渠道无退款接口", ActorUserID: 1,
	})
	if !errors.Is(err, ErrProviderActionUnsupported) {
		t.Fatalf("err = %v，期望 ErrProviderActionUnsupported", err)
	}
	if status, _, _, _ := readOrderRefundState(t, db, orderID); status != string(OrderSucceeded) {
		t.Fatalf("不支持的渠道不得改订单状态，实际 = %s", status)
	}
}

// TestSubscriptionRefundAlipay：支付宝退款通过 alipay.trade.refund 完成，
// out_request_no 固定（渠道幂等锚点）。
func TestSubscriptionRefundAlipay(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	privatePEM, publicPEM := alipayTestKeys(t)
	gateway := newAlipayTestServer(t, privatePEM, publicPEM)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderAlipay,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 9900, "ali_1")
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 0, &orderID)

	svc, err := NewTopupService(TopupServiceConfig{
		DB:               gormDB,
		Config:           alipayTestConfig(gateway.URL, "app_test", privatePEM, publicPEM),
		AlipayGatewayURL: gateway.URL,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	result, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "未开始使用", ActorUserID: 1,
	})
	if err != nil {
		t.Fatalf("alipay refund: %v", err)
	}
	if !result.Refunded || result.Order.Status != string(OrderRefunded) {
		t.Fatalf("result = %+v", result)
	}
	if got := readSubscriptionStatus(t, db, subID); got != "cancelled" {
		t.Fatalf("订阅状态 = %s，期望 cancelled", got)
	}
}

// TestRequestSubscriptionRefundMarksOrder：用户侧申请只写订单标记，不动钱；
// 重复申请幂等；已消耗/已开始返回 ErrSubscriptionInUse。
func TestRequestSubscriptionRefundMarksOrder(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderStripe,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 9900, "pi_req")
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 0, &orderID)
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")

	request, err := svc.RequestSubscriptionRefund(context.Background(), subID, userID, "买错了")
	if err != nil {
		t.Fatalf("request refund: %v", err)
	}
	if request.OrderID != orderID || request.Reason != "买错了" || request.RequestedAt.IsZero() {
		t.Fatalf("request = %+v", request)
	}
	status, _, amount, _ := readOrderRefundState(t, db, orderID)
	if status != string(OrderSucceeded) || amount != 0 {
		t.Fatalf("申请不得动订单资金事实: status=%s amount=%d", status, amount)
	}
	var reason sql.NullString
	var requestedAt sql.NullTime
	if err := db.QueryRow(`SELECT refund_reason, refund_requested_at FROM orders WHERE id = $1`,
		orderID).Scan(&reason, &requestedAt); err != nil {
		t.Fatalf("read refund request: %v", err)
	}
	if !requestedAt.Valid || !reason.Valid || reason.String != "买错了" {
		t.Fatalf("退款请求标记 = %v / %v", reason, requestedAt)
	}

	// 重复申请：保持第一次的理由与时刻，不报错。
	replay, err := svc.RequestSubscriptionRefund(context.Background(), subID, userID, "第二次理由")
	if err != nil {
		t.Fatalf("idempotent request: %v", err)
	}
	if replay.Reason != "买错了" || !replay.RequestedAt.Equal(request.RequestedAt) {
		t.Fatalf("重复申请 = %+v，期望保留首次标记", replay)
	}

	// 已消耗的订阅不能申请（P-13）。
	usedSubID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 1, nil)
	if _, err := svc.RequestSubscriptionRefund(context.Background(), usedSubID, userID, "想退"); !errors.Is(err, ErrSubscriptionInUse) {
		t.Fatalf("已消耗订阅 err = %v，期望 ErrSubscriptionInUse", err)
	}
}

// TestSubscriptionRefundFreeOrder：0 元免费套餐订单没有资金需要移动，
// 即使 provider 是 stripe 也不能因为「没有渠道交易号」被判成退款失败。
func TestSubscriptionRefundFreeOrder(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 0, 1000, 30, false, "enabled", "")

	now := time.Now().UTC()
	orderID := insertRefundOrder(t, db, userID, ProviderStripe,
		string(OrderSucceeded), store.OrderBizKindSubscribe, planID, 0, "")
	subID := insertRefundSubscription(t, db, userID, planID, now.Add(24*time.Hour), 0, &orderID)
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")

	result, err := svc.RefundSubscription(context.Background(), RefundInput{
		OrderID: orderID, Reason: "免费套餐不需要", ActorUserID: 1,
	})
	if err != nil {
		t.Fatalf("free order refund: %v", err)
	}
	if !result.Refunded || result.Amount != 0 || result.Order.Status != string(OrderRefunded) {
		t.Fatalf("result = %+v", result)
	}
	if got := readSubscriptionStatus(t, db, subID); got != "cancelled" {
		t.Fatalf("订阅状态 = %s，期望 cancelled", got)
	}
}
