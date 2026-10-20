package billing

// S3 订阅购买的计费侧验收（billing-08 §3.2/§3.8、任务书 A1）。
//
// 与 topup_test.go 相同，需要真实 PostgreSQL；未设置 NEWTG_TEST_DB_DSN 时
// 跳过。渠道协议用 httptest 桩验证「支付成功后由真实回调路径建实例」。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_s3?sslmode=disable' \
//	  go test ./internal/billing/ -run TestSubscription -v

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

// insertSubscriptionPlan 插入一条测试套餐，并在清理时移除其订单/实例。
func insertSubscriptionPlan(t *testing.T, db *sql.DB, price, grant int64,
	period int, resetOnRenew bool, status, scope string) int64 {

	t.Helper()
	key := fmt.Sprintf("s3-%d-%d", time.Now().UnixNano(), topupTestSeq.Add(1))
	var id int64
	var scopeArg any
	if scope != "" {
		scopeArg = scope
	}
	err := db.QueryRow(`
		INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency, grant_credits,
			 reset_on_renew, allow_wallet_overflow, model_scope, grace_days, status)
		VALUES ($1, 'S3 Test Plan', $2, $3, 'CNY', $4, $5, true, $6, 0, $7)
		RETURNING id`, key, period, price, grant, resetOnRenew, scopeArg, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscription plan: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE plan_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM orders WHERE biz_kind = 'subscribe' AND biz_ref_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, id)
	})
	return id
}

func walletTxCount(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`,
		userID).Scan(&count); err != nil {
		t.Fatalf("count wallet transactions: %v", err)
	}
	return count
}

func readSubscriptionByOrder(t *testing.T, db *sql.DB, orderID int64) *store.Subscription {
	t.Helper()
	var (
		sub         store.Subscription
		nextReset   sql.NullTime
		concurrency sql.NullInt64
		modelScope  []byte
	)
	err := db.QueryRow(`
		SELECT id, user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
		       next_reset_at, amount_total, amount_used, allow_wallet_overflow,
		       auto_renew, cancel_at_period_end, order_id, currency,
		       price_paid_amount, period_days, reset_on_renew, grace_days,
		       model_scope, concurrency_limit
		FROM subscriptions WHERE order_id = $1`, orderID).Scan(
		&sub.ID, &sub.UserID, &sub.PlanID, &sub.PlanTitleSnapshot, &sub.Status,
		&sub.StartAt, &sub.EndAt, &nextReset, &sub.AmountTotal, &sub.AmountUsed,
		&sub.AllowWalletOverflow, &sub.AutoRenew, &sub.CancelAtPeriodEnd,
		&sub.OrderID, &sub.Currency, &sub.PricePaidAmount, &sub.PeriodDays,
		&sub.ResetOnRenew, &sub.GraceDays, &modelScope, &concurrency)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read subscription by order %d: %v", orderID, err)
	}
	if nextReset.Valid {
		sub.NextResetAt = &nextReset.Time
	}
	if concurrency.Valid {
		v := int32(concurrency.Int64)
		sub.ConcurrencyLimit = &v
	}
	sub.ModelScope = modelScope
	return &sub
}

func providerTxnID(t *testing.T, db *sql.DB, orderID int64) string {
	t.Helper()
	var txn string
	if err := db.QueryRow(`SELECT provider_txn_id FROM orders WHERE id = $1`, orderID).
		Scan(&txn); err != nil {
		t.Fatalf("read provider_txn_id: %v", err)
	}
	return txn
}

func TestSubscriptionCreateOrderFieldsAndIdempotency(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 9900, 123400, 30, true, "enabled", `["gpt-4o"]`)
	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("sub-idem-%d", topupTestSeq.Add(1))

	first, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create subscription order: %v", err)
	}
	if first.Replayed || first.ClientSecret == "" {
		t.Fatalf("首次下单应创建渠道支付单: %+v", first)
	}
	order := first.Order
	if order.Status != string(OrderPending) || order.BizKind != store.OrderBizKindSubscribe ||
		order.BizRefID == nil || *order.BizRefID != planID {
		t.Fatalf("订单判别列 = kind:%s ref:%v status:%s", order.BizKind, order.BizRefID, order.Status)
	}
	if order.Amount != 9900 || order.Currency != "CNY" {
		t.Fatalf("金额/币种 = %d/%s，期望套餐值 9900/CNY", order.Amount, order.Currency)
	}
	if len(order.ChannelMetadata) == 0 {
		t.Fatal("channel_metadata 必须保存下单快照")
	}

	// 同 key 重放：同一订单、不新建。
	second, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Order.ID != order.ID {
		t.Fatalf("重放 = %+v，期望同一订单 %d", second, order.ID)
	}
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != 1 {
		t.Fatalf("同 key 订单数 = %d，期望 1", count)
	}

	// 同 key 指向另一套餐 → 409 语义（ErrIdempotencyConflict）。
	otherPlan := insertSubscriptionPlan(t, db, 100, 10, 7, true, "enabled", "")
	if _, err := svc.CreateSubscriptionOrder(context.Background(), userID, otherPlan, ProviderStripe, key); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("跨套餐重放 = %v，期望 ErrIdempotencyConflict", err)
	}
}

func TestSubscriptionSettleCreatesInstanceWithoutWallet(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 9900, 123400, 30, true, "enabled", `["gpt-4o","claude-3"]`)
	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("sub-settle-%d", topupTestSeq.Add(1))

	result, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orderID := result.Order.ID
	if sub := readSubscriptionByOrder(t, db, orderID); sub != nil {
		t.Fatalf("支付前不得有订阅实例: %+v", sub)
	}
	walletBefore := walletTxCount(t, db, userID)

	txn := providerTxnID(t, db, orderID)
	payload := stripeEventPayload(txn, 9900, "cny")
	sig := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)
	if err := svc.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("webhook: %v", err)
	}

	assertOrderStatus(t, db, orderID, "succeeded")
	var credited int64
	if err := db.QueryRow(`SELECT credited_credits FROM orders WHERE id = $1`, orderID).
		Scan(&credited); err != nil {
		t.Fatalf("read credited_credits: %v", err)
	}
	if credited != 0 {
		t.Fatalf("订阅订单 credited_credits = %d，期望 0（不入钱包）", credited)
	}
	if got := walletTxCount(t, db, userID); got != walletBefore {
		t.Fatalf("订阅购买不得写 wallet_transactions：%d → %d", walletBefore, got)
	}

	sub := readSubscriptionByOrder(t, db, orderID)
	if sub == nil {
		t.Fatal("支付成功后必须创建 subscriptions 实例")
	}
	if sub.UserID != userID || sub.PlanID != planID || sub.PlanTitleSnapshot != "S3 Test Plan" ||
		sub.Status != "active" {
		t.Fatalf("实例身份/快照 = %+v", sub)
	}
	if sub.AmountTotal != 123400 || sub.AmountUsed != 0 {
		t.Fatalf("额度 = total:%d used:%d", sub.AmountTotal, sub.AmountUsed)
	}
	if sub.OrderID == nil || *sub.OrderID != orderID {
		t.Fatalf("order_id = %v", sub.OrderID)
	}
	if sub.Currency != "CNY" || sub.PricePaidAmount != 9900 {
		t.Fatalf("购买快照 = currency:%s price:%d", sub.Currency, sub.PricePaidAmount)
	}
	if sub.PeriodDays != 30 || !sub.ResetOnRenew || sub.GraceDays != 0 {
		t.Fatalf("周期快照 = days:%d reset:%v grace:%d", sub.PeriodDays, sub.ResetOnRenew, sub.GraceDays)
	}
	if !sub.AllowWalletOverflow {
		t.Fatal("allow_wallet_overflow 快照必须来自套餐")
	}
	if sub.NextResetAt == nil || !sub.NextResetAt.Equal(sub.EndAt) {
		t.Fatalf("next_reset_at = %v，期望等于 end_at %v", sub.NextResetAt, sub.EndAt)
	}
	if got := sub.EndAt.Sub(sub.StartAt); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Fatalf("end_at-start_at = %v，期望约 30 天", got)
	}
	if string(sub.ModelScope) != `["gpt-4o", "claude-3"]` &&
		string(sub.ModelScope) != `["gpt-4o","claude-3"]` {
		t.Fatalf("model_scope 快照 = %s", sub.ModelScope)
	}

	// 重复回调：只一行实例、钱包仍无新流水。
	if err := svc.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("replay webhook: %v", err)
	}
	var subCount int64
	if err := db.QueryRow(`SELECT count(*) FROM subscriptions WHERE order_id = $1`, orderID).
		Scan(&subCount); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subCount != 1 {
		t.Fatalf("重复回调后实例数 = %d，期望 1", subCount)
	}
	if got := walletTxCount(t, db, userID); got != walletBefore {
		t.Fatalf("重复回调用钱包流水数 = %d，期望 %d", got, walletBefore)
	}
}

func TestSubscriptionZeroPriceCreatesImmediately(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 0, 5000, 7, false, "enabled", "")
	// 0 元套餐不应触碰渠道：指向一个必然连不上的地址，若被调用就会失败。
	svc := newStripeTopupService(t, gormDB, "http://127.0.0.1:1")
	key := fmt.Sprintf("sub-free-%d", topupTestSeq.Add(1))

	result, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create free subscription: %v", err)
	}
	if result.Replayed {
		t.Fatal("首次 0 元购买不是重放")
	}
	if result.ClientSecret != "" || result.QRCode != "" {
		t.Fatalf("0 元套餐不应返回渠道载荷: %+v", result)
	}
	if result.Order.Status != string(OrderSucceeded) || result.Order.PaidAt == nil {
		t.Fatalf("0 元订单必须直接 succeeded: %+v", result.Order)
	}
	sub := readSubscriptionByOrder(t, db, result.Order.ID)
	if sub == nil || sub.Status != "active" || sub.AmountTotal != 5000 {
		t.Fatalf("0 元订阅实例 = %+v", sub)
	}
	if sub.NextResetAt != nil {
		t.Fatalf("reset_on_renew=false 时 next_reset_at 必须为 NULL，得到 %v", sub.NextResetAt)
	}
	if got := walletTxCount(t, db, userID); got != 0 {
		t.Fatalf("0 元订阅不得写钱包流水，得到 %d 行", got)
	}

	replay, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("replay free: %v", err)
	}
	if !replay.Replayed || replay.Order.ID != result.Order.ID {
		t.Fatalf("0 元重放 = %+v", replay)
	}
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM subscriptions WHERE order_id = $1`, result.Order.ID).
		Scan(&count); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if count != 1 {
		t.Fatalf("0 元重放后实例数 = %d，期望 1", count)
	}
}

func TestSubscriptionSettleRollsBackOnInstanceFailure(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	planID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	svc := newStripeTopupService(t, gormDB, stripe.URL)
	key := fmt.Sprintf("sub-rollback-%d", topupTestSeq.Add(1))

	result, err := svc.CreateSubscriptionOrder(context.Background(), userID, planID, ProviderStripe, key)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orderID := result.Order.ID
	// 把快照里的 model_scope 改成对象：subscriptions_model_scope_chk 会拒绝，
	// 用来证明「订单 UPDATE 与实例 INSERT 同事务，实例失败则订单保持 pending」。
	if _, err := db.Exec(`UPDATE orders SET channel_metadata =
		jsonb_set(channel_metadata, '{model_scope}', '{"bad":true}'::jsonb) WHERE id = $1`, orderID); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}

	walletBefore := walletTxCount(t, db, userID)
	txn := providerTxnID(t, db, orderID)
	payload := stripeEventPayload(txn, 9900, "cny")
	sig := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)
	if err := svc.HandleStripeWebhook(context.Background(), payload, sig); err == nil {
		t.Fatal("实例插入失败时结算必须返回错误")
	}

	assertOrderStatus(t, db, orderID, "pending")
	if sub := readSubscriptionByOrder(t, db, orderID); sub != nil {
		t.Fatalf("失败回滚后不得留下 subscriptions 行: %+v", sub)
	}
	if got := walletTxCount(t, db, userID); got != walletBefore {
		t.Fatalf("失败路径不得写钱包流水：%d → %d", walletBefore, got)
	}
}

func TestSubscriptionCreateOrderRejectsUnavailablePlans(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	if _, err := svc.CreateSubscriptionOrder(context.Background(), userID, 99999999,
		ProviderStripe, fmt.Sprintf("sub-missing-%d", topupTestSeq.Add(1))); !errors.Is(err, ErrSubscriptionPlanUnavailable) {
		t.Fatalf("不存在套餐 = %v，期望 ErrSubscriptionPlanUnavailable", err)
	}

	disabledPlan := insertSubscriptionPlan(t, db, 100, 10, 7, true, "disabled", "")
	if _, err := svc.CreateSubscriptionOrder(context.Background(), userID, disabledPlan,
		ProviderStripe, fmt.Sprintf("sub-disabled-%d", topupTestSeq.Add(1))); !errors.Is(err, ErrSubscriptionPlanUnavailable) {
		t.Fatalf("停用套餐 = %v，期望 ErrSubscriptionPlanUnavailable", err)
	}

	validPlan := insertSubscriptionPlan(t, db, 100, 10, 7, true, "enabled", "")
	if _, err := svc.CreateSubscriptionOrder(context.Background(), userID, validPlan,
		"paypal", fmt.Sprintf("sub-provider-%d", topupTestSeq.Add(1))); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("非法渠道 = %v，期望 ErrInvalidProvider", err)
	}

	// 渠道未配置：providerReady 失败在插单前发生，不得留下订单。
	noConfig, err := NewTopupService(TopupServiceConfig{DB: gormDB})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	key := fmt.Sprintf("sub-noconfig-%d", topupTestSeq.Add(1))
	if _, err := noConfig.CreateSubscriptionOrder(context.Background(), userID, validPlan,
		ProviderStripe, key); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("未配置渠道 = %v，期望 ErrProviderDisabled", err)
	}
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != 0 {
		t.Fatalf("渠道未配置时不得留订单，得到 %d 行", count)
	}
}
