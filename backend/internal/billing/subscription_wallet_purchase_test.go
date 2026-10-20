package billing

// P2b 余额购买订阅的计费侧验收（任务书 P2b 冻结语义 1–7）。
//
// 与其它 billing 集成测试相同，需要真实 PostgreSQL；未设置
// NEWTG_TEST_DB_DSN 时跳过。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_p2b?sslmode=disable' \
//	  go test -tags=integration ./internal/billing/ -run TestWalletSubscription -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

// newWalletTopupServiceWithQPU 构造使用指定 billing.quota_per_unit 的充值服务。
// 钱包购买必须按 QPU 把「分」换算成积分，默认 100 的用例看不出换算错误，
// 因此换算验收需要显式覆盖 QPU。
func newWalletTopupServiceWithQPU(t *testing.T, gormDB *gorm.DB, qpu int64) *TopupService {
	t.Helper()
	cfg := stripeTestConfig()
	cfg[ConfigQuotaPerUnit] = json.RawMessage(strconv.FormatInt(qpu, 10))
	svc, err := NewTopupService(TopupServiceConfig{
		DB:            gormDB,
		Config:        cfg,
		StripeBaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	return svc
}

// readWalletBalance 读取用户钱包余额。
func readWalletBalance(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var balance int64
	if err := db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).
		Scan(&balance); err != nil {
		t.Fatalf("read balance of user %d: %v", userID, err)
	}
	return balance
}

// countSubscriptionPurchaseTx 统计某用户的 subscription_purchase 流水。
func countSubscriptionPurchaseTx(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'subscription_purchase'`, userID).Scan(&count); err != nil {
		t.Fatalf("count subscription_purchase rows: %v", err)
	}
	return count
}

func countSubscriptionsForUser(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM subscriptions WHERE user_id = $1`,
		userID).Scan(&count); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	return count
}

func countOrdersByKey(t *testing.T, db *sql.DB, key string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&count); err != nil {
		t.Fatalf("count orders by key: %v", err)
	}
	return count
}

// TestWalletSubscriptionPurchaseDebitsAndActivates 覆盖冻结语义 2/4/6：
// 余额充足 → 同一事务内扣款、写一条负额流水、订单 succeeded、实例生效；
// 同 key 重放不二次扣款；重复结算回调也不会再动钱包。
func TestWalletSubscriptionPurchaseDebitsAndActivates(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 10_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9_900, 123_400, 30, true, "enabled", `["gpt-4o"]`)
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("p2b-happy-%d", topupTestSeq.Add(1))

	result, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	if err != nil {
		t.Fatalf("wallet purchase: %v", err)
	}
	if result.Replayed || result.ClientSecret != "" || result.QRCode != "" {
		t.Fatalf("余额购买不应有支付凭据/重放标记: %+v", result)
	}
	order := result.Order
	if order.Status != string(OrderSucceeded) || order.Provider != ProviderWallet ||
		order.Amount != 9_900 || order.Currency != "CNY" ||
		order.BizKind != store.OrderBizKindSubscribe ||
		order.BizRefID == nil || *order.BizRefID != planID {
		t.Fatalf("订单 = status:%s provider:%s amount:%d biz:%s ref:%v",
			order.Status, order.Provider, order.Amount, order.BizKind, order.BizRefID)
	}
	if got := readWalletBalance(t, db, userID); got != 100 {
		t.Fatalf("balance = %d，期望 100", got)
	}

	var (
		txType  string
		amount  int64
		balance int64
		status  string
		refType sql.NullString
		refID   sql.NullString
	)
	if err := db.QueryRow(`SELECT type, amount, balance_after, status, reference_type, reference_id
		FROM wallet_transactions WHERE user_id = $1`, userID).
		Scan(&txType, &amount, &balance, &status, &refType, &refID); err != nil {
		t.Fatalf("read wallet transaction: %v", err)
	}
	if txType != walletTxTypeSubscriptionPurchase || amount != -9_900 ||
		balance != 100 || status != "confirmed" {
		t.Fatalf("流水 = type:%s amount:%d balance_after:%d status:%s",
			txType, amount, balance, status)
	}
	if !refType.Valid || refType.String != "order" ||
		!refID.Valid || refID.String != strconv.FormatInt(order.ID, 10) {
		t.Fatalf("流水引用 = %v/%v，期望 order/%d", refType, refID, order.ID)
	}

	sub := readSubscriptionByOrder(t, db, order.ID)
	if sub == nil || sub.Status != "active" || sub.PricePaidAmount != 9_900 {
		t.Fatalf("订阅实例 = %+v", sub)
	}

	// 同 key 重放：同一订单，不再扣款/建实例。
	replay, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Order.ID != order.ID {
		t.Fatalf("重放 = %+v，期望同一订单 %d", replay, order.ID)
	}
	if got := readWalletBalance(t, db, userID); got != 100 {
		t.Fatalf("重放后 balance = %d，期望 100", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 1 {
		t.Fatalf("重放后 subscription_purchase 流水 = %d，期望 1", got)
	}
	if got := countSubscriptionsForUser(t, db, userID); got != 1 {
		t.Fatalf("重放后实例数 = %d，期望 1", got)
	}

	// 重复回调：钱包订单已 succeeded，结算入口只刷新内存状态，不再扣款。
	if err := svc.settleOrder(context.Background(), order, 9_900, "CNY", []byte(`{}`)); err != nil {
		t.Fatalf("duplicate callback: %v", err)
	}
	if got := readWalletBalance(t, db, userID); got != 100 {
		t.Fatalf("重复回调后 balance = %d，期望 100", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 1 {
		t.Fatalf("重复回调后流水 = %d，期望 1", got)
	}
}

// TestWalletSubscriptionPurchaseInsufficientRollsBack 覆盖冻结语义 2：
// 余额不足返回 *QuotaError（402），且订单/流水/实例零写入、余额不变。
func TestWalletSubscriptionPurchaseInsufficientRollsBack(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 5_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("p2b-short-%d", topupTestSeq.Add(1))

	_, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("err = %v，期望 ErrInsufficientQuota", err)
	}
	var quotaErr *QuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Required != 9_900 || quotaErr.Available != 5_000 {
		t.Fatalf("QuotaError = %+v", quotaErr)
	}
	if got := readWalletBalance(t, db, userID); got != 5_000 {
		t.Fatalf("balance = %d，期望 5000", got)
	}
	if got := countOrdersByKey(t, db, key); got != 0 {
		t.Fatalf("余额不足留下 %d 个订单，期望 0", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 0 {
		t.Fatalf("余额不足留下 %d 条流水，期望 0", got)
	}
	if got := countSubscriptionsForUser(t, db, userID); got != 0 {
		t.Fatalf("余额不足留下 %d 个实例，期望 0", got)
	}
}

// TestWalletSubscriptionPurchaseConcurrentSameKeyDebitsOnce 覆盖冻结语义 4：
// 并发同 key 只产生一份订单、一条流水、一个实例、一次扣款。
func TestWalletSubscriptionPurchaseConcurrentSameKeyDebitsOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 10_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("p2b-race-%d", topupTestSeq.Add(1))

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	orders := make([]*TopupOrder, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			orders[i], errs[i] = svc.CreateSubscriptionOrder(
				context.Background(), userID, planID, ProviderWallet, key)
		}(i)
	}
	wg.Wait()

	var orderID int64
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if orders[i] == nil || orders[i].Order == nil {
			t.Fatalf("worker %d 返回空订单", i)
		}
		if orderID == 0 {
			orderID = orders[i].Order.ID
		} else if orders[i].Order.ID != orderID {
			t.Fatalf("worker %d 订单 id = %d，期望 %d", i, orders[i].Order.ID, orderID)
		}
	}
	if got := countOrdersByKey(t, db, key); got != 1 {
		t.Fatalf("并发同 key 订单数 = %d，期望 1", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 1 {
		t.Fatalf("并发同 key 流水数 = %d，期望 1", got)
	}
	if got := countSubscriptionsForUser(t, db, userID); got != 1 {
		t.Fatalf("并发同 key 实例数 = %d，期望 1", got)
	}
	if got := readWalletBalance(t, db, userID); got != 100 {
		t.Fatalf("并发同 key 后 balance = %d，期望 100（只扣一次）", got)
	}
}

// TestWalletSubscriptionPurchaseRollbackOnInstanceFailure 覆盖冻结语义 2 的
// 回滚要求：订阅实例插入失败时，钱包扣减与流水、订单全部回滚。
func TestWalletSubscriptionPurchaseRollbackOnInstanceFailure(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 10_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("p2b-rollback-%d", topupTestSeq.Add(1))

	// 用触发器注入「实例插入失败」：只对本次计划行生效，避免影响其它测试。
	if _, err := db.Exec(`CREATE OR REPLACE FUNCTION p2b_fail_subscription_insert()
		RETURNS trigger AS $f$
		BEGIN
			RAISE EXCEPTION 'p2b injected subscription insert failure';
		END;
		$f$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER p2b_fail_subscription_insert
		BEFORE INSERT ON subscriptions
		FOR EACH ROW WHEN (NEW.plan_id = %d)
		EXECUTE FUNCTION p2b_fail_subscription_insert()`, planID)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS p2b_fail_subscription_insert ON subscriptions`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS p2b_fail_subscription_insert()`)
	})

	_, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	if err == nil {
		t.Fatal("实例插入失败必须让整个事务失败")
	}
	if got := readWalletBalance(t, db, userID); got != 10_000 {
		t.Fatalf("回滚后 balance = %d，期望 10000", got)
	}
	if got := countOrdersByKey(t, db, key); got != 0 {
		t.Fatalf("回滚后订单数 = %d，期望 0", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 0 {
		t.Fatalf("回滚后流水数 = %d，期望 0", got)
	}
	if got := countSubscriptionsForUser(t, db, userID); got != 0 {
		t.Fatalf("回滚后实例数 = %d，期望 0", got)
	}
}

// TestWalletSubscriptionPurchaseZeroPrice 覆盖冻结语义 5：0 元套餐即时生效，
// 不写钱包流水（provider=wallet 也一样）。
func TestWalletSubscriptionPurchaseZeroPrice(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 0, 500, 7, false, "enabled", "")
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("p2b-free-%d", topupTestSeq.Add(1))

	result, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	if err != nil {
		t.Fatalf("free wallet purchase: %v", err)
	}
	if result.ClientSecret != "" || result.Order.Status != string(OrderSucceeded) {
		t.Fatalf("0 元订单 = %+v", result)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 0 {
		t.Fatalf("0 元套餐写了 %d 条 subscription_purchase 流水，期望 0", got)
	}
	if got := readWalletBalance(t, db, userID); got != 0 {
		t.Fatalf("0 元套餐改变了余额：%d", got)
	}
	if sub := readSubscriptionByOrder(t, db, result.Order.ID); sub == nil || sub.Status != "active" {
		t.Fatalf("0 元套餐未即时生效: %+v", sub)
	}
}

// TestWalletSubscriptionPurchaseAccountIsolation 覆盖冻结语义 4 的跨账户
// 隔离：同一 Idempotency-Key 不能被另一个用户重放；各用户余额独立。
func TestWalletSubscriptionPurchaseAccountIsolation(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	owner := createBillingTestUser(t, db, 20_000)
	other := createBillingTestUser(t, db, 20_000)
	deleteTestOrders(t, db, owner)
	deleteTestOrders(t, db, other)
	planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("p2b-owner-%d", topupTestSeq.Add(1))

	if _, err := svc.CreateSubscriptionOrder(context.Background(), owner, planID, ProviderWallet, key); err != nil {
		t.Fatalf("owner purchase: %v", err)
	}
	if _, err := svc.CreateSubscriptionOrder(context.Background(), other, planID, ProviderWallet, key); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("他人重放 err = %v，期望 ErrIdempotencyConflict", err)
	}
	if got := readWalletBalance(t, db, other); got != 20_000 {
		t.Fatalf("他人余额被改动：%d", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, other); got != 0 {
		t.Fatalf("他人产生 %d 条流水", got)
	}

	// 同一 key 换成另一个 provider 也是冲突（幂等键绑定订单内容）。
	if _, err := svc.CreateSubscriptionOrder(context.Background(), owner, planID, ProviderStripe, key); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("provider 变更重放 err = %v，期望 ErrIdempotencyConflict", err)
	}
	if got := readWalletBalance(t, db, owner); got != 10_100 {
		t.Fatalf("owner balance = %d，期望 10100", got)
	}
}

// TestWalletSubscriptionPurchaseBalanceNeverNegative 证明条件 UPDATE 在并发
// 多单时不会把余额扣成负数：余额只够一单，两笔不同 key 并发，恰一单成功。
func TestWalletSubscriptionPurchaseBalanceNeverNegative(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 10_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.CreateSubscriptionOrder(context.Background(), userID, planID,
				ProviderWallet, fmt.Sprintf("p2b-twice-%d-%d", i, topupTestSeq.Add(1)))
		}(i)
	}
	wg.Wait()

	success := 0
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrInsufficientQuota):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("成功订单数 = %d，期望恰 1", success)
	}
	if got := readWalletBalance(t, db, userID); got != 100 {
		t.Fatalf("balance = %d，期望 100（不得为负/多扣）", got)
	}
	if got := countSubscriptionPurchaseTx(t, db, userID); got != 1 {
		t.Fatalf("流水数 = %d，期望 1", got)
	}
	if got := countSubscriptionsForUser(t, db, userID); got != 1 {
		t.Fatalf("实例数 = %d，期望 1", got)
	}
}

// TestWalletSubscriptionPurchaseUsesQuotaPerUnit 覆盖 HIGH 修复：扣款额必须
// 按 billing.quota_per_unit 把套餐价（分）换算成积分，而不是直接用「分」。
// ¥99 套餐在 QPU=200 时扣 19,800 积分、QPU=50 时扣 4,950 积分；订单金额
// 仍是 CNY 分（9,900），流水与 402 的 Required 都是积分口径。
func TestWalletSubscriptionPurchaseUsesQuotaPerUnit(t *testing.T) {
	cases := []struct {
		name       string
		qpu        int64
		balance    int64
		wantDebit  int64
		wantRemain int64
	}{
		{name: "QPU=200 ¥99 → 19800", qpu: 200, balance: 20_000, wantDebit: 19_800, wantRemain: 200},
		{name: "QPU=50 ¥99 → 4950", qpu: 50, balance: 10_000, wantDebit: 4_950, wantRemain: 5_050},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gormDB, db := openBillingTestDB(t)
			userID := createBillingTestUser(t, db, tc.balance)
			deleteTestOrders(t, db, userID)
			planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
			svc := newWalletTopupServiceWithQPU(t, gormDB, tc.qpu)
			key := fmt.Sprintf("p2b-qpu-%d-%d", tc.qpu, topupTestSeq.Add(1))

			result, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
			if err != nil {
				t.Fatalf("wallet purchase: %v", err)
			}
			// 订单金额是合同价（分），不随 QPU 变化。
			if result.Order.Amount != 9_900 {
				t.Fatalf("订单金额 = %d，期望 9900（分）", result.Order.Amount)
			}
			if got := readWalletBalance(t, db, userID); got != tc.wantRemain {
				t.Fatalf("balance = %d，期望 %d", got, tc.wantRemain)
			}
			var amount, balanceAfter int64
			if err := db.QueryRow(`SELECT amount, balance_after FROM wallet_transactions
				WHERE user_id = $1 AND type = 'subscription_purchase'`, userID).
				Scan(&amount, &balanceAfter); err != nil {
				t.Fatalf("read wallet transaction: %v", err)
			}
			if amount != -tc.wantDebit || balanceAfter != tc.wantRemain {
				t.Fatalf("流水 = amount:%d balance_after:%d，期望 -%d/%d",
					amount, balanceAfter, tc.wantDebit, tc.wantRemain)
			}
			sub := readSubscriptionByOrder(t, db, result.Order.ID)
			if sub == nil || sub.PricePaidAmount != 9_900 {
				t.Fatalf("订阅实例 = %+v，price_paid_amount 应为订单金额 9900", sub)
			}
		})
	}
}

// TestWalletSubscriptionPurchaseInsufficientReportsCredits 证明 402 的
// Required 是积分（按 QPU 换算后），不是订单的「分」。
func TestWalletSubscriptionPurchaseInsufficientReportsCredits(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 10_000)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9_900, 100, 30, true, "enabled", "")
	svc := newWalletTopupServiceWithQPU(t, gormDB, 200)
	key := fmt.Sprintf("p2b-qpu-short-%d", topupTestSeq.Add(1))

	_, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	var quotaErr *QuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("err = %v，期望 *QuotaError", err)
	}
	if quotaErr.Required != 19_800 || quotaErr.Available != 10_000 {
		t.Fatalf("QuotaError = %+v，期望 Required=19800 Available=10000", quotaErr)
	}
	if got := readWalletBalance(t, db, userID); got != 10_000 {
		t.Fatalf("余额不足时 balance = %d，期望 10000", got)
	}
	if got := countOrdersByKey(t, db, key); got != 0 {
		t.Fatalf("余额不足留下 %d 个订单，期望 0", got)
	}
}

// TestWalletSubscriptionPurchaseCreditsFloorGuard 覆盖 required<=0 守卫：
// QPU 把扣款抹平为 0 时与充值入账同口径拒绝，不得以 0 积分交付付费套餐。
func TestWalletSubscriptionPurchaseCreditsFloorGuard(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 10_000)
	deleteTestOrders(t, db, userID)
	// 1 分 × QPU=1 = 0.01 积分，floor 后为 0。
	planID := insertSubscriptionPlan(t, db, 1, 100, 30, true, "enabled", "")
	svc := newWalletTopupServiceWithQPU(t, gormDB, 1)
	key := fmt.Sprintf("p2b-floor-%d", topupTestSeq.Add(1))

	_, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderWallet, key)
	if !errors.Is(err, ErrInvalidFundingCredit) {
		t.Fatalf("err = %v，期望 ErrInvalidFundingCredit", err)
	}
	if got := readWalletBalance(t, db, userID); got != 10_000 {
		t.Fatalf("balance = %d，期望 10000", got)
	}
	if got := countOrdersByKey(t, db, key); got != 0 {
		t.Fatalf("floor 守卫失败后留下 %d 个订单，期望 0", got)
	}
	if got := countSubscriptionsForUser(t, db, userID); got != 0 {
		t.Fatalf("floor 守卫失败后留下 %d 个实例，期望 0", got)
	}
}
