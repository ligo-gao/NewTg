package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

// 本文件是 M5 Task 5（worker 订单过期回收 + 支付宝轮询兜底）的验收。
//
// 冻结口径（裁决 P-1/P-5/P-20、GC）：
//   - 订单过期：pending 且 created_at < now()-30min → expired；条件 UPDATE
//     保证重复扫描幂等、终态不被改写。
//   - 兑换码没有过期 worker：expired 是读时虚拟态（platform-01 R11）。
//   - 支付宝轮询：每 2 分钟扫 pending 且创建超过 5 分钟的订单，调
//     alipay.trade.query；已付款走 settleOrder 同一幂等入账路径。
//   - 渠道关单失败只告警，不阻塞 DB 迁移；渠道查询失败不中断整轮扫描。
//
// 与仓内既有集成测试一致：未设置 NEWTG_TEST_DB_DSN 时由 openBillingTestDB 跳过。

var expiryTestSeq atomic.Int64

// insertExpiryOrder 直接落一条指定状态/创建时间的订单，绕过下单流程。
// provider_txn_id 可空（简单过期用例不需要渠道单号）。
func insertExpiryOrder(t *testing.T, db *sql.DB, userID int64,
	provider, status string, createdAt time.Time, txn *string) int64 {
	t.Helper()
	key := fmt.Sprintf("expiry-%d-%d", time.Now().UnixNano(), expiryTestSeq.Add(1))
	var id int64
	err := db.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status,
		                    idempotency_key, provider_txn_id, created_at)
		VALUES ($1, $2, 1000, 'CNY', $3, $4, $5, $6)
		RETURNING id`,
		userID, provider, status, key, txn, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert expiry order: %v", err)
	}
	return id
}

func orderStatus(t *testing.T, db *sql.DB, orderID int64) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		t.Fatalf("read order %d status: %v", orderID, err)
	}
	return status
}

func countOrderCredits(t *testing.T, db *sql.DB, orderID int64) int64 {
	t.Helper()
	var count int64
	err := db.QueryRow(`
		SELECT count(*) FROM wallet_transactions
		WHERE type = 'topup' AND reference_type = 'order' AND reference_id = $1`,
		fmt.Sprintf("%d", orderID)).Scan(&count)
	if err != nil {
		t.Fatalf("count order credits: %v", err)
	}
	return count
}

// expiryAlipayServer 是可编排的支付宝网关桩：按 out_trade_no 返回查询状态，
// 并可按需让 query/close 失败。签名往返复用 topup_test.go 的 RSA 工具。
type expiryAlipayServer struct {
	mu         sync.Mutex
	query      map[string]string // out_trade_no -> trade_status
	queryFail  map[string]bool   // out_trade_no -> HTTP 500
	closeFail  bool
	closeCalls int
}

func (e *expiryAlipayServer) setQuery(txn, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.query[txn] = status
}

func (e *expiryAlipayServer) setQueryFail(txn string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queryFail[txn] = true
}

func newExpiryAlipayServer(t *testing.T, privatePEM, publicPEM string,
	state *expiryAlipayServer) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		params := make(map[string]string, len(query))
		for key := range query {
			params[key] = query.Get(key)
		}
		if err := verifyRSA2Request(params, publicPEM); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		method := params["method"]
		outTradeNo := outTradeNoFromBiz(params["biz_content"])

		state.mu.Lock()
		fail := state.queryFail[outTradeNo]
		status := state.query[outTradeNo]
		closeFail := state.closeFail
		state.mu.Unlock()

		var node string
		switch method {
		case "alipay.trade.query":
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if status == "" {
				status = "WAIT_BUYER_PAY"
			}
			node = fmt.Sprintf(`{"code":"10000","msg":"Success","out_trade_no":%q,`+
				`"trade_status":%q,"total_amount":"10.00"}`, outTradeNo, status)
		case "alipay.trade.close":
			state.mu.Lock()
			state.closeCalls++
			state.mu.Unlock()
			if closeFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
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

// ---------------------------------------------------------------------------
// 订单过期扫描
// ---------------------------------------------------------------------------

// TestExpiryPendingOrderExpiresAndScanIsIdempotent 覆盖 TC-F-04-07-01：
// pending 31 分钟 → expired；29 分钟的 pending 与终态订单都不受影响；
// 重复扫描不重复操作。
func TestExpiryPendingOrderExpiresAndScanIsIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	now := time.Now().UTC()
	expiredID := insertExpiryOrder(t, db, userID, ProviderStripe,
		string(OrderPending), now.Add(-31*time.Minute), nil)
	freshID := insertExpiryOrder(t, db, userID, ProviderStripe,
		string(OrderPending), now.Add(-29*time.Minute), nil)
	succeededID := insertExpiryOrder(t, db, userID, ProviderStripe,
		string(OrderSucceeded), now.Add(-31*time.Minute), nil)
	// 已过期订单不再参与任何迁移（终态保护）。
	terminalExpiredID := insertExpiryOrder(t, db, userID, ProviderStripe,
		string(OrderExpired), now.Add(-31*time.Minute), nil)

	count, err := ExpirePendingOrders(context.Background(), gormDB, nil, now, OrderExpiry)
	if err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	if got := orderStatus(t, db, expiredID); got != string(OrderExpired) {
		t.Fatalf("expired order status = %q, want expired", got)
	}
	if got := orderStatus(t, db, freshID); got != string(OrderPending) {
		t.Fatalf("fresh order status = %q, want pending", got)
	}
	if got := orderStatus(t, db, succeededID); got != string(OrderSucceeded) {
		t.Fatalf("succeeded order status = %q, want succeeded", got)
	}
	if got := orderStatus(t, db, terminalExpiredID); got != string(OrderExpired) {
		t.Fatalf("terminal expired order status = %q", got)
	}

	// 重复扫描：没有 pending 超时订单，affected=0，任何状态都不变。
	count, err = ExpirePendingOrders(context.Background(), gormDB, nil, now, OrderExpiry)
	if err != nil {
		t.Fatalf("second ExpirePendingOrders: %v", err)
	}
	if count != 0 {
		t.Fatalf("second scan expired %d orders, want 0", count)
	}
	if got := orderStatus(t, db, expiredID); got != string(OrderExpired) {
		t.Fatalf("after second scan status = %q, want expired", got)
	}
}

// TestExpiryCloseFailureStillExpires 覆盖 TC-F-04-07-02：渠道关单失败只
// 告警，DB 迁移照常发生。
func TestExpiryCloseFailureStillExpires(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}, closeFail: true}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc, err := NewTopupService(TopupServiceConfig{
		DB:     gormDB,
		Config: alipayTestConfig(server.URL, "app_test", privatePEM, publicPEM),
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	now := time.Now().UTC()
	txn := "TGTIMEOUT1"
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-31*time.Minute), &txn)

	count, err := ExpirePendingOrders(context.Background(), gormDB, svc, now, OrderExpiry)
	if err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderExpired) {
		t.Fatalf("order status = %q, want expired despite close failure", got)
	}
}

// ---------------------------------------------------------------------------
// 支付宝轮询兜底
// ---------------------------------------------------------------------------

// TestExpiryAlipayPollCreditsPaidOrderOnce 覆盖回调丢失场景：轮询发现
// TRADE_SUCCESS 后走同一入账路径，重复轮询不重复到账。
func TestExpiryAlipayPollCreditsPaidOrderOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc, err := NewTopupService(TopupServiceConfig{
		DB:     gormDB,
		Config: alipayTestConfig(server.URL, "app_test", privatePEM, publicPEM),
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	now := time.Now().UTC()
	txn := "TGPOLLPAID1"
	state.setQuery(txn, "TRADE_SUCCESS")
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-6*time.Minute), &txn)
	// 创建不足 5 分钟的订单不参与本轮轮询。
	freshTxn := "TGPOLLFRESH1"
	state.setQuery(freshTxn, "TRADE_SUCCESS")
	freshID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-2*time.Minute), &freshTxn)

	if err := PollPendingAlipayOrders(context.Background(), gormDB, svc, now, AlipayPollMinAge); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderSucceeded) {
		t.Fatalf("order status = %q, want succeeded", got)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("balance = %d, want 1000", got)
	}
	if got := countOrderCredits(t, db, orderID); got != 1 {
		t.Fatalf("credit rows = %d, want 1", got)
	}
	if got := orderStatus(t, db, freshID); got != string(OrderPending) {
		t.Fatalf("fresh order status = %q, want pending", got)
	}

	// 第二次轮询：订单已终态，不再命中 pending 扫描；余额与流水不变。
	if err := PollPendingAlipayOrders(context.Background(), gormDB, svc, now, AlipayPollMinAge); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("balance after second poll = %d, want 1000", got)
	}
	if got := countOrderCredits(t, db, orderID); got != 1 {
		t.Fatalf("credit rows after second poll = %d, want 1", got)
	}
}

// TestExpiryAlipayPollTradeClosedExpires：TRADE_CLOSED 条件 UPDATE 为 expired。
func TestExpiryAlipayPollTradeClosedExpires(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc, err := NewTopupService(TopupServiceConfig{
		DB:     gormDB,
		Config: alipayTestConfig(server.URL, "app_test", privatePEM, publicPEM),
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	now := time.Now().UTC()
	txn := "TGPOLLCLOSED1"
	state.setQuery(txn, "TRADE_CLOSED")
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-6*time.Minute), &txn)

	if err := PollPendingAlipayOrders(context.Background(), gormDB, svc, now, AlipayPollMinAge); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderExpired) {
		t.Fatalf("order status = %q, want expired", got)
	}
	if got := userBalance(t, db, userID); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

// TestExpiryAlipayPollProviderErrorContinues：单个订单查询失败只记录，
// 同轮其它订单照常处理（fault isolation）。
func TestExpiryAlipayPollProviderErrorContinues(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc, err := NewTopupService(TopupServiceConfig{
		DB:     gormDB,
		Config: alipayTestConfig(server.URL, "app_test", privatePEM, publicPEM),
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	now := time.Now().UTC()
	badTxn := "TGPOLLERROR1"
	goodTxn := "TGPOLLGOOD1"
	state.setQueryFail(badTxn)
	state.setQuery(goodTxn, "TRADE_SUCCESS")
	badID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-6*time.Minute), &badTxn)
	goodID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-6*time.Minute), &goodTxn)

	if err := PollPendingAlipayOrders(context.Background(), gormDB, svc, now, AlipayPollMinAge); err != nil {
		t.Fatalf("poll returned error, loop must continue: %v", err)
	}
	if got := orderStatus(t, db, badID); got != string(OrderPending) {
		t.Fatalf("failed-query order status = %q, want pending", got)
	}
	if got := orderStatus(t, db, goodID); got != string(OrderSucceeded) {
		t.Fatalf("good order status = %q, want succeeded", got)
	}
}

// ---------------------------------------------------------------------------
// 到期前的最终收款核实（复审 Important #1）
// ---------------------------------------------------------------------------

func newExpiryAlipayServiceForTest(t *testing.T, gormDB *gorm.DB,
	serverURL, privatePEM, publicPEM string) *TopupService {
	t.Helper()
	svc, err := NewTopupService(TopupServiceConfig{
		DB:     gormDB,
		Config: alipayTestConfig(serverURL, "app_test", privatePEM, publicPEM),
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	return svc
}

// TestExpiryPrecheckPaidOrderCreditsInsteadOfExpiring：订单在到期扫描时
// 已被渠道确认付款（最后一个轮询间隙内支付）→ 入账，绝不置 expired，
// 且重复扫描不重复入账。
func TestExpiryPrecheckPaidOrderCreditsInsteadOfExpiring(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc := newExpiryAlipayServiceForTest(t, gormDB, server.URL, privatePEM, publicPEM)

	now := time.Now().UTC()
	txn := "TGPRECHECKPAID1"
	state.setQuery(txn, "TRADE_SUCCESS")
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-31*time.Minute), &txn)

	count, err := ExpirePendingOrders(context.Background(), gormDB, svc, now, OrderExpiry)
	if err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired count = %d, want 0 (paid order must be credited)", count)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderSucceeded) {
		t.Fatalf("order status = %q, want succeeded", got)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("balance = %d, want 1000", got)
	}
	if got := countOrderCredits(t, db, orderID); got != 1 {
		t.Fatalf("credit rows = %d, want 1", got)
	}

	// 重复扫描：已是终态，不再命中 pending；余额不变。
	if _, err := ExpirePendingOrders(context.Background(), gormDB, svc, now, OrderExpiry); err != nil {
		t.Fatalf("second ExpirePendingOrders: %v", err)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("balance after second scan = %d, want 1000", got)
	}
}

// TestExpiryPrecheckTradeClosedExpiresWithoutClose：渠道明确 TRADE_CLOSED
// 时直接过期，不再重复调用 trade.close。
func TestExpiryPrecheckTradeClosedExpiresWithoutClose(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc := newExpiryAlipayServiceForTest(t, gormDB, server.URL, privatePEM, publicPEM)

	now := time.Now().UTC()
	txn := "TGPRECHECKCLOSED1"
	state.setQuery(txn, "TRADE_CLOSED")
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-31*time.Minute), &txn)

	count, err := ExpirePendingOrders(context.Background(), gormDB, svc, now, OrderExpiry)
	if err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderExpired) {
		t.Fatalf("order status = %q, want expired", got)
	}
	state.mu.Lock()
	calls := state.closeCalls
	state.mu.Unlock()
	if calls != 0 {
		t.Fatalf("trade.close called %d times for TRADE_CLOSED order, want 0", calls)
	}
}

// TestExpiryPrecheckStillUnpaidExpires：渠道确认仍未付款 → 正常过期，
// 且先尝试关单（close 失败也不影响，见上一个用例）。
func TestExpiryPrecheckStillUnpaidExpires(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc := newExpiryAlipayServiceForTest(t, gormDB, server.URL, privatePEM, publicPEM)

	now := time.Now().UTC()
	txn := "TGPRECHECKUNPAID1"
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-31*time.Minute), &txn)

	count, err := ExpirePendingOrders(context.Background(), gormDB, svc, now, OrderExpiry)
	if err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderExpired) {
		t.Fatalf("order status = %q, want expired", got)
	}
}

// TestExpiryPrecheckProviderErrorKeepsPending：到期时渠道查询失败 →
// 本轮保持 pending（不能把可能已付款的订单关掉），下一轮恢复后正常过期。
func TestExpiryPrecheckProviderErrorKeepsPending(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	privatePEM, publicPEM := alipayTestKeys(t)
	state := &expiryAlipayServer{query: map[string]string{}, queryFail: map[string]bool{}}
	server := newExpiryAlipayServer(t, privatePEM, publicPEM, state)
	svc := newExpiryAlipayServiceForTest(t, gormDB, server.URL, privatePEM, publicPEM)

	now := time.Now().UTC()
	txn := "TGPRECHECKERROR1"
	state.setQueryFail(txn)
	orderID := insertExpiryOrder(t, db, userID, ProviderAlipay,
		string(OrderPending), now.Add(-31*time.Minute), &txn)

	count, err := ExpirePendingOrders(context.Background(), gormDB, svc, now, OrderExpiry)
	if err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired count = %d, want 0 on provider error", count)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderPending) {
		t.Fatalf("order status = %q, want pending after query error", got)
	}

	// 渠道恢复且确认未付款：下一轮可以正常过期。
	state.mu.Lock()
	delete(state.queryFail, txn)
	state.query[txn] = "WAIT_BUYER_PAY"
	state.mu.Unlock()
	count, err = ExpirePendingOrders(context.Background(), gormDB, svc, now.Add(time.Minute), OrderExpiry)
	if err != nil {
		t.Fatalf("second ExpirePendingOrders: %v", err)
	}
	if count != 1 {
		t.Fatalf("second scan expired %d, want 1", count)
	}
	if got := orderStatus(t, db, orderID); got != string(OrderExpired) {
		t.Fatalf("order status = %q, want expired", got)
	}
}

// ---------------------------------------------------------------------------
// 单轮批量上限与跨轮次排空（复审 Important #2）
// ---------------------------------------------------------------------------

// TestExpiryBatchDrainsAcrossRounds：积压超过 ExpiryScanBatchSize 时，
// 每轮只处理上限条（最旧优先），剩余订单由后续轮次继续且不跳过任何行。
func TestExpiryBatchDrainsAcrossRounds(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	const extra = 5
	total := ExpiryScanBatchSize + extra
	now := time.Now().UTC()
	// 一条 SQL 造积压：created_at 从 31 分钟前开始逐秒更早，全部命中扫描。
	if _, err := db.Exec(`
		INSERT INTO orders (user_id, provider, amount, currency, status,
		                    idempotency_key, created_at)
		SELECT $1, 'stripe', 1000, 'CNY', 'pending',
		       'expiry-batch-' || $2 || '-' || g,
		       $3::timestamptz - (g || ' seconds')::interval
		FROM generate_series(1, $4) AS g`,
		userID, fmt.Sprintf("%d", time.Now().UnixNano()), now.Add(-31*time.Minute), total); err != nil {
		t.Fatalf("insert batch orders: %v", err)
	}

	first, err := ExpirePendingOrders(context.Background(), gormDB, nil, now, OrderExpiry)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if first != ExpiryScanBatchSize {
		t.Fatalf("first batch expired %d, want limit %d", first, ExpiryScanBatchSize)
	}
	second, err := ExpirePendingOrders(context.Background(), gormDB, nil, now, OrderExpiry)
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if second != extra {
		t.Fatalf("second batch expired %d, want %d", second, extra)
	}
	third, err := ExpirePendingOrders(context.Background(), gormDB, nil, now, OrderExpiry)
	if err != nil {
		t.Fatalf("third batch: %v", err)
	}
	if third != 0 {
		t.Fatalf("third batch expired %d, want 0", third)
	}
	var pending int
	if err := db.QueryRow(
		`SELECT count(*) FROM orders WHERE user_id = $1 AND status = 'pending'`,
		userID).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending orders left = %d, want 0 (no row skipped)", pending)
	}
}

// ---------------------------------------------------------------------------
// 兑换码：过期是虚拟态，不存在 worker
// ---------------------------------------------------------------------------

// TestExpiryRedemptionCodeSemanticsUnchanged 验证 P-5/R11：到期兑换码仍
// 保持 status='unused'（不物化 expired、没有兑换码过期 worker），但核销
// 路径按读时派生拒绝。
func TestExpiryRedemptionCodeSemanticsUnchanged(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)

	rawCode := "ABCDEFGHJKLMNPQRSTVWXYZ0123"
	hash := HashRedemptionCode(NormalizeRedemptionCode(rawCode))
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := db.Exec(`
		INSERT INTO redemption_codes (code_hash, credits, status, created_by, expires_at)
		VALUES ($1, 100, 'unused', $2, $3)`, hash, userID, past); err != nil {
		t.Fatalf("insert redemption code: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE code_hash = $1`, hash) })

	// 订单过期扫描不触碰 redemption_codes。
	if _, err := ExpirePendingOrders(context.Background(), gormDB, nil,
		time.Now().UTC(), OrderExpiry); err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE code_hash = $1`, hash).Scan(&status); err != nil {
		t.Fatalf("read redemption status: %v", err)
	}
	if status != "unused" {
		t.Fatalf("redemption status = %q, want unused (virtual expiry)", status)
	}

	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), mapConfig{})
	if _, _, err := svc.RedeemDetailed(context.Background(), userID, rawCode); !errors.Is(err, ErrRedemptionCodeExpired) {
		t.Fatalf("RedeemDetailed err = %v, want ErrRedemptionCodeExpired", err)
	}
	if got := orderStatusCount(t, db, "unused", hash); got != 1 {
		t.Fatalf("unused redemption count = %d, want 1", got)
	}
}

func orderStatusCount(t *testing.T, db *sql.DB, status, hash string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(
		`SELECT count(*) FROM redemption_codes WHERE code_hash = $1 AND status = $2`,
		hash, status).Scan(&count); err != nil {
		t.Fatalf("count redemption status: %v", err)
	}
	return count
}

// ---------------------------------------------------------------------------
// worker 装配：可注入周期与时钟
// ---------------------------------------------------------------------------

// TestExpiryWorkerLoopInjectsClockAndIntervals 验证循环接线本身：使用注入
// 的扫描函数与固定时钟，不依赖 DB/渠道；单轮扫描失败不终止循环。
func TestExpiryWorkerLoopInjectsClockAndIntervals(t *testing.T) {
	fixed := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var (
		orderCalls atomic.Int64
		pollCalls  atomic.Int64
		clockSaw   atomic.Int64
	)
	orderSignal := make(chan struct{}, 8)
	pollSignal := make(chan struct{}, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deps := ExpiryWorkerDeps{
		Now:            func() time.Time { return fixed },
		OrderInterval:  5 * time.Millisecond,
		AlipayInterval: 5 * time.Millisecond,
		OrderScan: func(_ context.Context, now time.Time) (int, error) {
			if now.Equal(fixed) {
				clockSaw.Add(1)
			}
			if orderCalls.Add(1) == 1 {
				// 第一轮失败必须被记录并继续，而不是让循环退出。
				return 0, errors.New("scan failure")
			}
			select {
			case orderSignal <- struct{}{}:
			default:
			}
			return 0, nil
		},
		AlipayPoll: func(_ context.Context, now time.Time) error {
			if now.Equal(fixed) {
				clockSaw.Add(1)
			}
			pollCalls.Add(1)
			select {
			case pollSignal <- struct{}{}:
			default:
			}
			return errors.New("provider unavailable")
		},
	}

	done := make(chan error, 1)
	go func() { done <- RunExpiryLoop(ctx, deps) }()

	// 等待：订单扫描至少成功两轮、轮询至少两轮。
	for i := 0; i < 2; i++ {
		select {
		case <-orderSignal:
		case <-time.After(2 * time.Second):
			t.Fatal("order scan did not run on the injected interval")
		}
		select {
		case <-pollSignal:
		case <-time.After(2 * time.Second):
			t.Fatal("alipay poll did not run on the injected interval")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunExpiryLoop returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunExpiryLoop did not stop on context cancellation")
	}
	if got := clockSaw.Load(); got < 4 {
		t.Fatalf("scans observed fixed clock %d times, want >= 4", got)
	}
}
