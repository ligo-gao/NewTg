package billing

// P2a 手动续费的计费侧验收（billing-08 §3.3 方案甲、任务书 P2a）。
//
// 与 subscription_purchase_test.go 相同，需要真实 PostgreSQL；未设置
// NEWTG_TEST_DB_DSN 时跳过。渠道协议用 Stripe httptest 桩，支付成功由
// 真实 webhook 路径驱动 settleOrder 的 renew 分支。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_p2?sslmode=disable' \
//	  go test ./internal/billing/ -run TestSubscriptionRenew -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

// insertRenewalSubscription 插入一条待续费实例，并在清理时移除它与其续费订单。
func insertRenewalSubscription(t *testing.T, db *sql.DB, userID, planID int64,
	title, status string, start, end time.Time, used int64, resetOnRenew bool) int64 {

	t.Helper()
	var nextReset any
	if resetOnRenew {
		nextReset = end
	}
	var id int64
	err := db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 currency, price_paid_amount, period_days, reset_on_renew, grace_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1000, $8, true,
		        'CNY', 9900, 30, $9, 0)
		RETURNING id`,
		userID, planID, title, status, start.UTC(), end.UTC(), nextReset, used,
		resetOnRenew).Scan(&id)
	if err != nil {
		t.Fatalf("insert renewal subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM orders WHERE biz_kind = 'renew' AND biz_ref_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, id)
	})
	return id
}

// readRenewalSubscription 读取续费测试关心的全部实例列（不含 created/updated）。
func readRenewalSubscription(t *testing.T, db *sql.DB, id int64) store.Subscription {
	t.Helper()
	var (
		sub         store.Subscription
		nextReset   sql.NullTime
		modelScope  []byte
		concurrency sql.NullInt64
		orderID     sql.NullInt64
	)
	err := db.QueryRow(`
		SELECT id, user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
		       next_reset_at, amount_total, amount_used, allow_wallet_overflow,
		       auto_renew, cancel_at_period_end, order_id, currency,
		       price_paid_amount, period_days, reset_on_renew, grace_days,
		       model_scope, concurrency_limit
		FROM subscriptions WHERE id = $1`, id).Scan(
		&sub.ID, &sub.UserID, &sub.PlanID, &sub.PlanTitleSnapshot, &sub.Status,
		&sub.StartAt, &sub.EndAt, &nextReset, &sub.AmountTotal, &sub.AmountUsed,
		&sub.AllowWalletOverflow, &sub.AutoRenew, &sub.CancelAtPeriodEnd,
		&orderID, &sub.Currency, &sub.PricePaidAmount, &sub.PeriodDays,
		&sub.ResetOnRenew, &sub.GraceDays, &modelScope, &concurrency)
	if err != nil {
		t.Fatalf("read renewal subscription %d: %v", id, err)
	}
	if nextReset.Valid {
		sub.NextResetAt = &nextReset.Time
	}
	if concurrency.Valid {
		v := int32(concurrency.Int64)
		sub.ConcurrencyLimit = &v
	}
	sub.ModelScope = modelScope
	return sub
}

// renewViaStripe 用真实 webhook 路径驱动一次续费结算。
func renewViaStripe(t *testing.T, svc *TopupService, db *sql.DB, orderID, amount int64) error {
	t.Helper()
	txnID := providerTxnID(t, db, orderID)
	payload := stripeEventPayload(txnID, amount, "cny")
	sig := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)
	return svc.HandleStripeWebhook(context.Background(), payload, sig)
}

func assertTimeNear(t *testing.T, got, want time.Time, tolerance time.Duration, label string) {
	t.Helper()
	delta := got.Sub(want)
	if delta < -tolerance || delta > tolerance {
		t.Fatalf("%s = %s，期望 %s（±%s）", label, got, want, tolerance)
	}
}

func TestSubscriptionRenewActiveExtendsSameInstance(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 9900, 5000, 30, true, "enabled", `["gpt-4o"]`)
	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(20 * 24 * time.Hour)
	subID := insertRenewalSubscription(t, db, userID, planID, "Old Title",
		"active", now.Add(-10*24*time.Hour), oldEnd, 777, true)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("renew-active-%d", topupTestSeq.Add(1))

	beforeWallet := walletTxCount(t, db, userID)
	order, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create renewal order: %v", err)
	}
	if order.Order.BizKind != store.OrderBizKindRenew || order.Order.BizRefID == nil ||
		*order.Order.BizRefID != subID {
		t.Fatalf("订单判别列 = %s/%v，期望 renew/%d", order.Order.BizKind, order.Order.BizRefID, subID)
	}
	if order.Order.Amount != 9900 || order.Order.Currency != "CNY" ||
		order.Order.Status != string(OrderPending) || order.ClientSecret == "" {
		t.Fatalf("续费订单 = %+v / secret=%q", order.Order, order.ClientSecret)
	}
	meta := readSubscriptionMetadata(t, db, order.Order.ID)
	if _, ok := meta["renewal"]; !ok {
		t.Fatalf("channel_metadata 缺少 renewal 命名空间: %v", meta)
	}
	if _, ok := meta["subscription"]; ok {
		t.Fatalf("续费订单不得写 subscription 命名空间: %v", meta)
	}

	// 同 key 重放：返回同一订单，不新增行。
	replay, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID, ProviderStripe, key)
	if err != nil || !replay.Replayed || replay.Order.ID != order.Order.ID {
		t.Fatalf("同 key 重放 = %+v / %v", replay, err)
	}
	var orderCount int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&orderCount); err != nil {
		t.Fatalf("count renewal orders: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("同 key 订单数 = %d，期望 1", orderCount)
	}

	if err := renewViaStripe(t, svc, db, order.Order.ID, 9900); err != nil {
		t.Fatalf("renew webhook: %v", err)
	}
	sub := readRenewalSubscription(t, db, subID)
	// GREATEST(end_at, now) + 30 天：active 实例在原到期日上累加。
	assertTimeNear(t, sub.EndAt.UTC(), oldEnd.Add(30*24*time.Hour), 2*time.Second, "end_at")
	if sub.Status != "active" || sub.AmountUsed != 0 || sub.AmountTotal != 5000 {
		t.Fatalf("续费后实例 = status:%s used:%d total:%d", sub.Status, sub.AmountUsed, sub.AmountTotal)
	}
	if sub.PlanTitleSnapshot != "S3 Test Plan" || sub.PeriodDays != 30 ||
		sub.PricePaidAmount != 9900 || !sub.ResetOnRenew {
		t.Fatalf("续费快照未按当前套餐更新: %+v", sub)
	}
	if sub.NextResetAt == nil {
		t.Fatal("reset_on_renew=true 时 next_reset_at 必须为新的 end_at")
	}
	assertTimeNear(t, sub.NextResetAt.UTC(), sub.EndAt.UTC(), time.Second, "next_reset_at")
	if !sub.StartAt.UTC().Equal(now.Add(-10 * 24 * time.Hour)) {
		t.Fatalf("续费不得改写 start_at: %s", sub.StartAt)
	}
	if got := walletTxCount(t, db, userID); got != beforeWallet {
		t.Fatalf("续费不得写 wallet_transactions：%d → %d", beforeWallet, got)
	}

	// S1：续费与 renewed 事件同事务（该实例是测试直接插入的，没有 created）。
	renewEvents := readSubscriptionEvents(t, db, subID)
	if len(renewEvents) != 1 || renewEvents[0].Kind != store.SubscriptionEventRenewed {
		t.Fatalf("续费后事件 = %+v，期望恰一条 renewed", renewEvents)
	}

	// 重复回调：订单已 succeeded，直接幂等返回，end_at 不再推进。
	if err := renewViaStripe(t, svc, db, order.Order.ID, 9900); err != nil {
		t.Fatalf("replay renewal webhook: %v", err)
	}
	after := readRenewalSubscription(t, db, subID)
	if !after.EndAt.Equal(sub.EndAt) {
		t.Fatalf("重复回调再次推进 end_at：%s → %s", sub.EndAt, after.EndAt)
	}
	if after.AmountUsed != 0 || after.Status != "active" {
		t.Fatalf("重复回调改写了实例: %+v", after)
	}
	if got := readSubscriptionEvents(t, db, subID); len(got) != 1 {
		t.Fatalf("重复回调后事件数 = %d，期望 1（不得重复追加 renewed）", len(got))
	}
}

func TestSubscriptionRenewExpiredRestoresActive(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 9900, 5000, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	subID := insertRenewalSubscription(t, db, userID, planID, "Old Title",
		"expired", now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour), 100, true)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("renew-expired-%d", topupTestSeq.Add(1))
	order, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create expired renewal order: %v", err)
	}
	if err := renewViaStripe(t, svc, db, order.Order.ID, 9900); err != nil {
		t.Fatalf("renew webhook: %v", err)
	}
	sub := readRenewalSubscription(t, db, subID)
	if sub.Status != "active" {
		t.Fatalf("expired 续费后 status = %s，期望 active", sub.Status)
	}
	// expired：从 now 起算，而不是在早已过去的 end_at 上补周期。
	assertTimeNear(t, sub.EndAt.UTC(), now.Add(30*24*time.Hour), 3*time.Second, "end_at")
	if sub.AmountUsed != 0 {
		t.Fatalf("续费后 amount_used = %d，期望 0", sub.AmountUsed)
	}
	if sub.NextResetAt == nil {
		t.Fatal("next_reset_at 必须等于新的 end_at")
	}
	assertTimeNear(t, sub.NextResetAt.UTC(), sub.EndAt.UTC(), time.Second, "next_reset_at")
}

func TestSubscriptionRenewRejectsCancelledDisabledAndForeign(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	otherID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	deleteTestOrders(t, db, otherID)

	planID := insertSubscriptionPlan(t, db, 9900, 5000, 30, true, "enabled", "")
	now := time.Now().UTC()
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	t.Run("cancelled 409", func(t *testing.T) {
		subID := insertRenewalSubscription(t, db, userID, planID, "T",
			"cancelled", now.Add(-time.Hour), now.Add(30*24*time.Hour), 0, true)
		key := fmt.Sprintf("renew-cancelled-%d", topupTestSeq.Add(1))
		if _, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID,
			ProviderStripe, key); !errors.Is(err, ErrSubscriptionNotRenewable) {
			t.Fatalf("cancelled 续费 err = %v，期望 ErrSubscriptionNotRenewable", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("套餐停用 409", func(t *testing.T) {
		subID := insertRenewalSubscription(t, db, userID, planID, "T",
			"active", now.Add(-time.Hour), now.Add(30*24*time.Hour), 0, true)
		if _, err := db.Exec(`UPDATE subscription_plans SET status = 'disabled' WHERE id = $1`, planID); err != nil {
			t.Fatalf("disable plan: %v", err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`UPDATE subscription_plans SET status = 'enabled' WHERE id = $1`, planID)
		})
		key := fmt.Sprintf("renew-disabled-%d", topupTestSeq.Add(1))
		if _, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID,
			ProviderStripe, key); !errors.Is(err, ErrSubscriptionPlanUnavailable) {
			t.Fatalf("停用套餐续费 err = %v，期望 ErrSubscriptionPlanUnavailable", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("他人实例 404 语义", func(t *testing.T) {
		subID := insertRenewalSubscription(t, db, otherID, planID, "T",
			"active", now.Add(-time.Hour), now.Add(30*24*time.Hour), 0, true)
		key := fmt.Sprintf("renew-foreign-%d", topupTestSeq.Add(1))
		if _, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID,
			ProviderStripe, key); !errors.Is(err, ErrRenewalSubscriptionNotFound) {
			t.Fatalf("他人实例续费 err = %v，期望 ErrRenewalSubscriptionNotFound", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("跨实例复用幂等键 409", func(t *testing.T) {
		subA := insertRenewalSubscription(t, db, userID, planID, "A",
			"active", now.Add(-time.Hour), now.Add(30*24*time.Hour), 0, true)
		subB := insertRenewalSubscription(t, db, userID, planID, "B",
			"active", now.Add(-time.Hour), now.Add(30*24*time.Hour), 0, true)
		key := fmt.Sprintf("renew-cross-%d", topupTestSeq.Add(1))
		if _, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subA,
			ProviderStripe, key); err != nil {
			t.Fatalf("首次续费: %v", err)
		}
		if _, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subB,
			ProviderStripe, key); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("跨实例同 key err = %v，期望 ErrIdempotencyConflict", err)
		}
	})
}

func assertNoOrderWithKey(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&count); err != nil {
		t.Fatalf("count orders by key: %v", err)
	}
	if count != 0 {
		t.Fatalf("被拒绝的续费不得留订单（key=%s，%d 行）", key, count)
	}
}

func TestSubscriptionRenewZeroPriceIsImmediate(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 0, 500, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(10 * 24 * time.Hour)
	subID := insertRenewalSubscription(t, db, userID, planID, "Old",
		"active", now.Add(-20*24*time.Hour), oldEnd, 321, true)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	beforeWallet := walletTxCount(t, db, userID)
	key := fmt.Sprintf("renew-zero-%d", topupTestSeq.Add(1))
	result, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create zero-price renewal: %v", err)
	}
	if result.Replayed || result.ClientSecret != "" || result.QRCode != "" {
		t.Fatalf("0 元续费不应调用渠道: %+v", result)
	}
	if result.Order.Status != string(OrderSucceeded) || result.Order.Amount != 0 {
		t.Fatalf("0 元续费订单 = %+v", result.Order)
	}
	sub := readRenewalSubscription(t, db, subID)
	assertTimeNear(t, sub.EndAt.UTC(), oldEnd.Add(30*24*time.Hour), 2*time.Second, "end_at")
	if sub.AmountUsed != 0 || sub.AmountTotal != 500 || sub.Status != "active" {
		t.Fatalf("0 元续费后实例 = %+v", sub)
	}
	if got := walletTxCount(t, db, userID); got != beforeWallet {
		t.Fatalf("0 元续费不得写钱包流水：%d → %d", beforeWallet, got)
	}
}

func TestSubscriptionRenewFailureLeavesOrderAndInstanceUntouched(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 9900, 5000, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	t.Run("结算前实例进入 cancelled：订单不 succeeded、不扩展", func(t *testing.T) {
		oldEnd := now.Add(10 * 24 * time.Hour)
		subID := insertRenewalSubscription(t, db, userID, planID, "T",
			"active", now.Add(-time.Hour), oldEnd, 50, true)
		key := fmt.Sprintf("renew-race-%d", topupTestSeq.Add(1))
		order, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID,
			ProviderStripe, key)
		if err != nil {
			t.Fatalf("create renewal order: %v", err)
		}
		if _, err := db.Exec(`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, subID); err != nil {
			t.Fatalf("cancel subscription concurrently: %v", err)
		}
		if err := renewViaStripe(t, svc, db, order.Order.ID, 9900); err == nil {
			t.Fatal("实例已 cancelled 时结算必须失败")
		}
		assertOrderStatus(t, db, order.Order.ID, string(OrderPending))
		sub := readRenewalSubscription(t, db, subID)
		if sub.Status != "cancelled" || !sub.EndAt.Equal(oldEnd) || sub.AmountUsed != 50 {
			t.Fatalf("失败结算不得扩展实例: %+v", sub)
		}
	})

	t.Run("金额不符：订单 refund_required、实例不扩展", func(t *testing.T) {
		oldEnd := now.Add(10 * 24 * time.Hour)
		subID := insertRenewalSubscription(t, db, userID, planID, "T",
			"active", now.Add(-time.Hour), oldEnd, 50, true)
		key := fmt.Sprintf("renew-mismatch-%d", topupTestSeq.Add(1))
		order, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID,
			ProviderStripe, key)
		if err != nil {
			t.Fatalf("create renewal order: %v", err)
		}
		if err := renewViaStripe(t, svc, db, order.Order.ID, 1); !errors.Is(err, ErrAmountMismatch) {
			t.Fatalf("金额不符 err = %v，期望 ErrAmountMismatch", err)
		}
		// P2-1：渠道确认已收款，订单不能标 failed（钱会凭空挂在「失败」里），
		// 落到 refund_required 等人工退款。
		assertOrderStatus(t, db, order.Order.ID, string(OrderRefundRequired))
		sub := readRenewalSubscription(t, db, subID)
		if !sub.EndAt.Equal(oldEnd) || sub.AmountUsed != 50 {
			t.Fatalf("失败结算不得扩展实例: %+v", sub)
		}
	})
}

func TestSubscriptionRenewMetadataKeepsOnlyRenewalNamespace(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	planID := insertSubscriptionPlan(t, db, 9900, 5000, 30, true, "enabled", "")
	now := time.Now().UTC()
	subID := insertRenewalSubscription(t, db, userID, planID, "T",
		"active", now.Add(-time.Hour), now.Add(30*24*time.Hour), 0, true)
	svc := newStripeTopupService(t, gormDB, stripe.URL)
	order, err := svc.CreateSubscriptionRenewalOrder(context.Background(), userID, subID,
		ProviderStripe, fmt.Sprintf("renew-meta-%d", topupTestSeq.Add(1)))
	if err != nil {
		t.Fatalf("create renewal order: %v", err)
	}
	meta := readSubscriptionMetadata(t, db, order.Order.ID)
	var snap map[string]any
	if err := json.Unmarshal(meta["renewal"], &snap); err != nil {
		t.Fatalf("decode renewal snapshot: %v", err)
	}
	if snap["subscription_id"] != float64(subID) || snap["plan_id"] != float64(planID) ||
		snap["period_days"] != float64(30) || snap["grant_credits"] != float64(5000) ||
		snap["price_amount"] != float64(9900) || snap["currency"] != "CNY" {
		t.Fatalf("renewal snapshot = %v", snap)
	}
}
