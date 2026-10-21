package billing

// S2 订阅升级/降级的计费侧验收（billing-08 §3.6、任务书 P-10/P-11/P-12、
// R-23～R-25）。
//
// 分两层：
//   - 纯公式用例（TestSubscriptionChange*Formula）不需要数据库，用固定时钟
//     断言「四舍五入到分」与「时长向下取整到秒」两个冻结口径；
//   - 其余用例需要真实 PostgreSQL（未设置 NEWTG_TEST_DB_DSN 时跳过），
//     渠道协议用 Stripe httptest 桩，支付成功由真实 webhook 路径驱动
//     settleOrder 的 change 分支。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_s2?sslmode=disable' \
//	  go test ./internal/billing/ -run TestSubscriptionChange -v

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

// ---------------------------------------------------------------------------
// 纯公式（无数据库）
// ---------------------------------------------------------------------------

// TestSubscriptionChangeRemainingValueFormula 覆盖 P-11 的剩余价值公式：
// 周期中点、恰好到期（剩余 0 秒）、已过期（不产生负额）、四舍五入到分、
// 以及降级延长后「剩余价值可以超过原价」的形状。
func TestSubscriptionChangeRemainingValueFormula(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	cases := []struct {
		name   string
		price  int64
		period int32
		endAt  time.Time
		want   int64
	}{
		{"周期中点", 9900, 30, now.Add(15 * day), 4950},
		{"剩余 0 秒（恰好到期）", 9900, 30, now, 0},
		{"已过期不产生负额", 9900, 30, now.Add(-72 * time.Hour), 0},
		{"剩余 1 秒向下取整", 9900, 30, now.Add(time.Second), 0},
		{"剩余 1 天", 20000, 20, now.Add(day), 1000},
		// 9900 × 5 天 / 30 天 = 1650 整；再用 100/3 天看四舍五入：
		// 100 × 1 天 / 3 天 = 33.33 → 33；100 × 2 天 / 3 天 = 66.67 → 67。
		{"四舍五入向下", 100, 3, now.Add(day), 33},
		{"四舍五入向上", 100, 3, now.Add(2 * day), 67},
		// 半值：1 × 1 天 / 2 天 = 0.5 → 1（round 半值远离零）。
		{"半值进位", 1, 2, now.Add(day), 1},
		// 降级延长过的实例：剩余时间可以超过一个周期，剩余价值随之线性放大。
		{"超过一个周期", 9900, 30, now.Add(45 * day), 14850},
		{"价格为 0 的免费实例", 0, 30, now.Add(15 * day), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subscriptionRemainingValue(tc.price, tc.period, tc.endAt, now); got != tc.want {
				t.Fatalf("剩余价值 = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// TestSubscriptionChangeAmountsFormula 覆盖 P-11 的应补/折算规则：
// 升级补足差价得到完整新周期；降级不退款、差额按新套餐单价折算时长
// （向下取整到秒）；免费套餐给一个完整周期且不产生负额。
func TestSubscriptionChangeAmountsFormula(t *testing.T) {
	const periodSeconds = int64(30) * 86400

	cases := []struct {
		name        string
		remainValue int64
		newPrice    int64
		newPeriod   int32
		wantDelta   int64
		wantSeconds int64
	}{
		{"升级到更贵套餐补差价", 4950, 19900, 30, 14950, periodSeconds},
		{"剩余价值为 0 按全新周期计", 0, 19900, 30, 19900, periodSeconds},
		{"剩余价值恰好等于新价", 9900, 9900, 30, 0, periodSeconds},
		// 6600 / 4900 个周期 = 40.408… 天 → 向下取整到秒。
		{"降级折算延长时长", 6600, 4900, 30, 0, 6600 * periodSeconds / 4900},
		// 剩余价值略小于新价：仍需补足差价，换完整周期（不出现负额）。
		{"剩余价值略小于新价", 1, 3, 30, 2, periodSeconds},
		// 剩余价值 ≫ 新价：折算时长超过一个周期（降级延长）。
		{"剩余价值大于新价", 9800, 4900, 30, 0, 2 * periodSeconds},
		{"免费套餐给完整周期", 0, 0, 30, 0, periodSeconds},
		{"免费套餐但有剩余价值", 5000, 0, 30, 0, periodSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, seconds := subscriptionChangeAmounts(tc.remainValue, tc.newPrice, tc.newPeriod)
			if delta != tc.wantDelta || seconds != tc.wantSeconds {
				t.Fatalf("(应补, 时长) = (%d, %d)，期望 (%d, %d)",
					delta, seconds, tc.wantDelta, tc.wantSeconds)
			}
			if delta < 0 {
				t.Fatalf("应补差价不得为负：%d", delta)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 数据库用例
// ---------------------------------------------------------------------------

// insertChangeSubscription 插入一条待升降级的实例，并在清理时移除它与其
// change 订单。price/grant/used/period 全部可指定，便于构造「旧套餐更贵」
// 与「旧套餐更便宜」两类样本。
func insertChangeSubscription(t *testing.T, db *sql.DB, userID, planID int64,
	status string, pricePaid, grant, used int64, periodDays int32,
	start, end time.Time) int64 {

	t.Helper()
	var nextReset any
	if end.After(start) {
		nextReset = end.UTC()
	}
	var id int64
	err := db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 currency, price_paid_amount, period_days, reset_on_renew, grace_days)
		VALUES ($1, $2, 'Old Plan', $3, $4, $5, $6, $7, $8, true,
		        'CNY', $9, $10, true, 0)
		RETURNING id`,
		userID, planID, status, start.UTC(), end.UTC(), nextReset,
		grant, used, pricePaid, periodDays).Scan(&id)
	if err != nil {
		t.Fatalf("insert change subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_events WHERE subscription_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM orders WHERE biz_kind = 'change' AND biz_ref_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, id)
	})
	return id
}

// changeViaStripe 用真实 webhook 路径驱动一次升降级结算。
func changeViaStripe(t *testing.T, svc *TopupService, db *sql.DB, orderID, amount int64) error {
	t.Helper()
	txnID := providerTxnID(t, db, orderID)
	payload := stripeEventPayload(txnID, amount, "cny")
	sig := stripeSignatureHeader("whsec_test_123", time.Now().Unix(), payload)
	return svc.HandleStripeWebhook(context.Background(), payload, sig)
}

// readChangeEventDetail 解码某实例第一条事件（或指定 kind 的第一条）的 detail。
func readChangeEventDetail(t *testing.T, db *sql.DB, subscriptionID int64, kind string) map[string]any {
	t.Helper()
	for _, event := range readSubscriptionEvents(t, db, subscriptionID) {
		if event.Kind != kind {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			t.Fatalf("decode %s event detail: %v（%s）", kind, err, event.Detail)
		}
		return detail
	}
	t.Fatalf("没有找到 %s 事件（实例 %d）", kind, subscriptionID)
	return nil
}

// TestSubscriptionChangeUpgradeMidPeriod 覆盖 Step 1：周期中点升级，
// 应补 = 新价 − 剩余价值，走订单通道；支付成功后同一实例迁移到新套餐，
// 额度重算并追加 upgraded 事件。
func TestSubscriptionChangeUpgradeMidPeriod(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	newPlanID := insertSubscriptionPlan(t, db, 19900, 5000, 30, true, "enabled", `["gpt-4o"]`)
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-15 * 24 * time.Hour)
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		9900, 1000, 400, 30, start, now.Add(15*24*time.Hour))

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	beforeWallet := walletTxCount(t, db, userID)
	key := fmt.Sprintf("change-upgrade-%d", topupTestSeq.Add(1))

	result, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("create change order: %v", err)
	}
	// 剩余 15 天 × 9900 / 30 天 = 4950；应补 = 19900 − 4950 = 14950。
	// 构造与求值之间有毫秒级漂移，允许 1 分的抖动（剩余秒向下取整只会
	// 让剩余价值变小或不变）。
	if result.PriceDelta < 14949 || result.PriceDelta > 14950 {
		t.Fatalf("应补差价 = %d，期望 14949..14950", result.PriceDelta)
	}
	if result.EventKind != store.SubscriptionEventUpgraded || result.CreditsDelta != 4000 {
		t.Fatalf("事件/额度差额 = %s/%d，期望 upgraded/4000", result.EventKind, result.CreditsDelta)
	}
	if result.Order.ID == 0 || result.Order.BizKind != store.OrderBizKindChange ||
		result.Order.BizRefID == nil || *result.Order.BizRefID != subID ||
		result.Order.Amount != result.PriceDelta || result.Order.Currency != "CNY" ||
		result.Order.Status != string(OrderPending) || result.ClientSecret == "" {
		t.Fatalf("订单 = %+v / secret=%q", result.Order, result.ClientSecret)
	}
	meta := readSubscriptionMetadata(t, db, result.Order.ID)
	if _, ok := meta["change"]; !ok {
		t.Fatalf("channel_metadata 缺少 change 命名空间: %v", meta)
	}
	if _, ok := meta["renewal"]; ok {
		t.Fatalf("升降级订单不得写 renewal 命名空间: %v", meta)
	}

	// 支付完成前实例不得被改动。
	before := readRenewalSubscription(t, db, subID)
	if before.PlanID != oldPlanID || before.AmountTotal != 1000 || before.AmountUsed != 400 {
		t.Fatalf("下单阶段就改动了实例: %+v", before)
	}

	if err := changeViaStripe(t, svc, db, result.Order.ID, result.PriceDelta); err != nil {
		t.Fatalf("change webhook: %v", err)
	}
	after := readRenewalSubscription(t, db, subID)
	if after.PlanID != newPlanID || after.PlanTitleSnapshot != "S3 Test Plan" ||
		after.AmountTotal != 5000 || after.PricePaidAmount != 19900 ||
		after.Status != "active" {
		t.Fatalf("升级后实例 = %+v", after)
	}
	// P-12：used 保留（pre_consume 累计），不被清零。
	if after.AmountUsed != 400 {
		t.Fatalf("升级后 amount_used = %d，期望保留 400", after.AmountUsed)
	}
	// 应补 > 0 ⇒ 换成完整新周期（30 天，从支付成功时刻起算）。
	if delta := after.EndAt.UTC().Sub(now); delta < 29*24*time.Hour || delta > 31*24*time.Hour {
		t.Fatalf("升级后 end_at = %s（距 now %s），期望约 30 天", after.EndAt, delta)
	}
	if !after.StartAt.UTC().Equal(start) {
		t.Fatalf("升降级不得改写 start_at：%s → %s", start, after.StartAt)
	}
	if after.NextResetAt == nil || !after.NextResetAt.UTC().Equal(after.EndAt.UTC()) {
		t.Fatalf("reset_on_renew=true 时 next_reset_at 必须为新 end_at: %v", after.NextResetAt)
	}
	if after.ID != subID {
		t.Fatalf("实例 id 被改写：%d → %d", subID, after.ID)
	}
	if got := walletTxCount(t, db, userID); got != beforeWallet {
		t.Fatalf("升降级不得写钱包流水：%d → %d", beforeWallet, got)
	}

	// R-24：事件与状态变更同事务，detail 只有四个键。
	events := readSubscriptionEvents(t, db, subID)
	if len(events) != 1 || events[0].Kind != store.SubscriptionEventUpgraded {
		t.Fatalf("升降级后事件 = %+v，期望恰一条 upgraded", events)
	}
	detail := readChangeEventDetail(t, db, subID, store.SubscriptionEventUpgraded)
	if len(detail) != 4 {
		t.Fatalf("事件 detail 键集合 = %v，期望恰四个（from_plan_id/to_plan_id/price_delta/credits_delta）", detail)
	}
	if detail["from_plan_id"] != float64(oldPlanID) || detail["to_plan_id"] != float64(newPlanID) ||
		detail["price_delta"] != float64(result.PriceDelta) || detail["credits_delta"] != float64(4000) {
		t.Fatalf("事件 detail = %v", detail)
	}

	// 重复回调：订单已 succeeded，只幂等返回，不再改写实例。
	if err := changeViaStripe(t, svc, db, result.Order.ID, result.PriceDelta); err != nil {
		t.Fatalf("replay change webhook: %v", err)
	}
	replayed := readRenewalSubscription(t, db, subID)
	if !replayed.EndAt.Equal(after.EndAt) || replayed.AmountUsed != after.AmountUsed ||
		replayed.PlanID != after.PlanID {
		t.Fatalf("重复回调改写了实例: %+v → %+v", after, replayed)
	}
	if got := readSubscriptionEvents(t, db, subID); len(got) != 1 {
		t.Fatalf("重复回调后事件数 = %d，期望 1（不得重复追加 upgraded）", len(got))
	}

	// 同 key 重放：返回同一订单，不再折算一次。
	again, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: key,
	})
	if err != nil || !again.Replayed || again.OrderID != result.Order.ID {
		t.Fatalf("同 key 重放 = %+v / %v", again, err)
	}
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&count); err != nil {
		t.Fatalf("count change orders: %v", err)
	}
	if count != 1 {
		t.Fatalf("同 key 订单数 = %d，期望 1", count)
	}
}

// TestSubscriptionChangeZeroRemainingChargesFullPeriod 覆盖 Step 2：剩余 0 秒
// （恰好到期）不产生负额，按全新周期计——应补 = 新套餐全价。
func TestSubscriptionChangeZeroRemainingChargesFullPeriod(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	newPlanID := insertSubscriptionPlan(t, db, 19900, 5000, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-30 * 24 * time.Hour)
	// status 仍是 active、end_at 已经到点：到期 worker 尚未推进的中间态。
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		9900, 1000, 700, 30, start, now.Add(-time.Minute))

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	result, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: fmt.Sprintf("change-zero-rem-%d", topupTestSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("create change order: %v", err)
	}
	if result.PriceDelta != 19900 || result.NewEndAt.Before(now.Add(29*24*time.Hour)) {
		t.Fatalf("剩余 0 秒时应补 = %d / new_end_at = %s，期望全价与完整周期",
			result.PriceDelta, result.NewEndAt)
	}
	if err := changeViaStripe(t, svc, db, result.Order.ID, 19900); err != nil {
		t.Fatalf("change webhook: %v", err)
	}
	after := readRenewalSubscription(t, db, subID)
	if after.AmountTotal != 5000 || after.AmountUsed != 700 || after.PricePaidAmount != 19900 {
		t.Fatalf("升级后实例 = %+v", after)
	}
	if delta := after.EndAt.UTC().Sub(now); delta < 29*24*time.Hour || delta > 31*24*time.Hour {
		t.Fatalf("end_at = %s（距 now %s），期望约 30 天", after.EndAt, delta)
	}
	if after.EndAt.Before(after.StartAt) {
		t.Fatalf("end_at 必须晚于 start_at：%s ≤ %s", after.EndAt, after.StartAt)
	}
}

// TestSubscriptionChangeDowngradeExtendsWithoutPayment 覆盖 Step 3：降级
// 不退款，差额折算为延长时长，且不改 start_at、不调用渠道。
func TestSubscriptionChangeDowngradeExtendsWithoutPayment(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	newPlanID := insertSubscriptionPlan(t, db, 4900, 400, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-10 * 24 * time.Hour)
	oldEnd := now.Add(20 * 24 * time.Hour)
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		9900, 1000, 250, 30, start, oldEnd)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	beforeWallet := walletTxCount(t, db, userID)
	result, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: fmt.Sprintf("change-down-%d", topupTestSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("create downgrade: %v", err)
	}
	// 剩余 20 天 × 9900 / 30 = 6600；6600 / 4900 个周期 = 40.408 天。
	if result.PriceDelta != 0 || result.EventKind != store.SubscriptionEventDowngraded {
		t.Fatalf("降级 = 应补 %d / kind %s，期望 0 / downgraded", result.PriceDelta, result.EventKind)
	}
	if result.ClientSecret != "" || result.QRCode != "" || result.PayURL != "" {
		t.Fatalf("0 元差价不得调用渠道: %+v", result)
	}
	if result.Order.Status != string(OrderSucceeded) || result.Order.Amount != 0 {
		t.Fatalf("0 元升降级订单 = %+v，期望已 succeeded / amount 0", result.Order)
	}

	after := readRenewalSubscription(t, db, subID)
	if after.PlanID != newPlanID || after.AmountTotal != 400 || after.AmountUsed != 250 ||
		after.PricePaidAmount != 4900 {
		t.Fatalf("降级后实例 = %+v", after)
	}
	if !after.StartAt.UTC().Equal(start) {
		t.Fatalf("降级不得改写 start_at：%s → %s", start, after.StartAt)
	}
	// end_at 从 now 起算 40.408 天 > 旧的 20 天剩余 ⇒ 周期被延长。
	if !after.EndAt.After(oldEnd) {
		t.Fatalf("降级必须延长 end_at：旧 %s → 新 %s", oldEnd, after.EndAt)
	}
	if delta := after.EndAt.UTC().Sub(now); delta < 40*24*time.Hour || delta > 41*24*time.Hour {
		t.Fatalf("降级后剩余时长 = %s，期望约 40.4 天", delta)
	}
	if got := walletTxCount(t, db, userID); got != beforeWallet {
		t.Fatalf("降级不得退款（不得写钱包流水）：%d → %d", beforeWallet, got)
	}

	events := readSubscriptionEvents(t, db, subID)
	if len(events) != 1 || events[0].Kind != store.SubscriptionEventDowngraded {
		t.Fatalf("降级后事件 = %+v，期望恰一条 downgraded", events)
	}
	detail := readChangeEventDetail(t, db, subID, store.SubscriptionEventDowngraded)
	if detail["from_plan_id"] != float64(oldPlanID) || detail["to_plan_id"] != float64(newPlanID) ||
		detail["price_delta"] != float64(0) || detail["credits_delta"] != float64(-600) {
		t.Fatalf("降级事件 detail = %v", detail)
	}
}

// TestSubscriptionChangeClampsUsedWhenRemainNegative 覆盖 P-12 的「remain 为负
// 时置 0」：新套餐总额度小于已用量时，amount_used 钳到新总额度而不是记账负数。
func TestSubscriptionChangeClampsUsedWhenRemainNegative(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	// 新套餐额度 200 < 已用 400。
	newPlanID := insertSubscriptionPlan(t, db, 4900, 200, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		9900, 1000, 400, 30, now.Add(-24*time.Hour), now.Add(29*24*time.Hour))

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	if _, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: fmt.Sprintf("change-clamp-%d", topupTestSeq.Add(1)),
	}); err != nil {
		t.Fatalf("create change: %v", err)
	}
	after := readRenewalSubscription(t, db, subID)
	if after.AmountTotal != 200 || after.AmountUsed != 200 {
		t.Fatalf("额度钳制后 = total:%d used:%d，期望 200/200", after.AmountTotal, after.AmountUsed)
	}
}

// TestSubscriptionChangeRejectsCycleMismatchAndGuards 覆盖 Step 4 与各类守卫：
// 跨周期（P-10）→ ErrSubscriptionCycleMismatch；同套餐、他人实例、终态、
// 停用套餐各有明确哨兵，且失败时不留订单、不改实例。
func TestSubscriptionChangeRejectsCycleMismatchAndGuards(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	otherID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	deleteTestOrders(t, db, otherID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	monthlyPlanID := insertSubscriptionPlan(t, db, 19900, 5000, 30, true, "enabled", "")
	quarterlyPlanID := insertSubscriptionPlan(t, db, 29900, 8000, 90, true, "enabled", "")
	now := time.Now().UTC()
	svc := newStripeTopupService(t, gormDB, stripe.URL)

	newSub := func(user int64, status string) int64 {
		return insertChangeSubscription(t, db, user, oldPlanID, status,
			9900, 1000, 0, 30, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	}

	t.Run("跨周期单位 422", func(t *testing.T) {
		subID := newSub(userID, "active")
		key := fmt.Sprintf("change-cycle-%d", topupTestSeq.Add(1))
		_, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: quarterlyPlanID,
			Provider: ProviderStripe, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrSubscriptionCycleMismatch) {
			t.Fatalf("跨周期 err = %v，期望 ErrSubscriptionCycleMismatch", err)
		}
		assertNoOrderWithKey(t, db, key)
		if sub := readRenewalSubscription(t, db, subID); sub.PlanID != oldPlanID {
			t.Fatalf("失败请求改动了实例: %+v", sub)
		}
	})

	t.Run("目标即当前套餐 409", func(t *testing.T) {
		subID := newSub(userID, "active")
		key := fmt.Sprintf("change-same-%d", topupTestSeq.Add(1))
		_, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: oldPlanID,
			Provider: ProviderStripe, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrChangePlanUnchanged) {
			t.Fatalf("同套餐 err = %v，期望 ErrChangePlanUnchanged", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("他人实例 404 语义", func(t *testing.T) {
		subID := newSub(otherID, "active")
		key := fmt.Sprintf("change-foreign-%d", topupTestSeq.Add(1))
		_, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: monthlyPlanID,
			Provider: ProviderStripe, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrChangeSubscriptionNotFound) {
			t.Fatalf("他人实例 err = %v，期望 ErrChangeSubscriptionNotFound", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("cancelled 终态 409", func(t *testing.T) {
		subID := newSub(userID, "cancelled")
		key := fmt.Sprintf("change-cancelled-%d", topupTestSeq.Add(1))
		_, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: monthlyPlanID,
			Provider: ProviderStripe, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrSubscriptionNotChangeable) {
			t.Fatalf("cancelled err = %v，期望 ErrSubscriptionNotChangeable", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("停用套餐 409", func(t *testing.T) {
		subID := newSub(userID, "active")
		disabledID := insertSubscriptionPlan(t, db, 19900, 5000, 30, true, "disabled", "")
		key := fmt.Sprintf("change-disabled-%d", topupTestSeq.Add(1))
		_, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: disabledID,
			Provider: ProviderStripe, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrSubscriptionPlanUnavailable) {
			t.Fatalf("停用套餐 err = %v，期望 ErrSubscriptionPlanUnavailable", err)
		}
		assertNoOrderWithKey(t, db, key)
	})

	t.Run("同 key 改口到另一套餐 409", func(t *testing.T) {
		subID := newSub(userID, "active")
		key := fmt.Sprintf("change-key-%d", topupTestSeq.Add(1))
		if _, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: monthlyPlanID,
			Provider: ProviderStripe, IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("首次升降级: %v", err)
		}
		otherPlanID := insertSubscriptionPlan(t, db, 29900, 6000, 30, true, "enabled", "")
		if _, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: otherPlanID,
			Provider: ProviderStripe, IdempotencyKey: key,
		}); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("同 key 换套餐 err = %v，期望 ErrIdempotencyConflict", err)
		}
	})

	t.Run("wallet 渠道 400 语义", func(t *testing.T) {
		subID := newSub(userID, "active")
		key := fmt.Sprintf("change-wallet-%d", topupTestSeq.Add(1))
		if _, err := svc.ChangePlan(context.Background(), ChangePlanInput{
			UserID: userID, SubscriptionID: subID, NewPlanID: monthlyPlanID,
			Provider: ProviderWallet, IdempotencyKey: key,
		}); !errors.Is(err, ErrInvalidProvider) {
			t.Fatalf("wallet err = %v，期望 ErrInvalidProvider", err)
		}
		assertNoOrderWithKey(t, db, key)
	})
}

// TestSubscriptionChangeZeroPriceTargetIsImmediate 覆盖「换成免费套餐」：
// 0 元差价即时生效、不调用渠道、不缩短已有剩余时间。
func TestSubscriptionChangeZeroPriceTargetIsImmediate(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	freePlanID := insertSubscriptionPlan(t, db, 0, 300, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(45 * 24 * time.Hour)
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		9900, 1000, 10, 30, now.Add(-24*time.Hour), oldEnd)

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	result, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: freePlanID,
		Provider: ProviderAlipay, IdempotencyKey: fmt.Sprintf("change-free-%d", topupTestSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("create free change: %v", err)
	}
	if result.PriceDelta != 0 || result.Order.Status != string(OrderSucceeded) ||
		result.QRCode != "" || result.ClientSecret != "" {
		t.Fatalf("免费套餐升降级 = %+v", result)
	}
	after := readRenewalSubscription(t, db, subID)
	if after.PlanID != freePlanID || after.AmountTotal != 300 || after.PricePaidAmount != 0 {
		t.Fatalf("换成免费套餐后实例 = %+v", after)
	}
	// 剩余 45 天 > 免费套餐的 30 天：GREATEST(end_at, …) 保住剩余时间。
	if !after.EndAt.UTC().Equal(oldEnd) {
		t.Fatalf("换免费套餐不得缩短周期：%s → %s", oldEnd, after.EndAt)
	}
}

// TestSubscriptionChangeSettlementFailureLeavesOrderPending 覆盖「失败零扩展」：
// 下单后实例被并发取消，webhook 结算必须失败并回滚订单状态，实例保持不动。
func TestSubscriptionChangeSettlementFailureLeavesOrderPending(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 9900, 1000, 30, true, "enabled", "")
	newPlanID := insertSubscriptionPlan(t, db, 19900, 5000, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		9900, 1000, 50, 30, now.Add(-time.Hour), now.Add(29*24*time.Hour))

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	result, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: fmt.Sprintf("change-race-%d", topupTestSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("create change order: %v", err)
	}
	if _, err := db.Exec(`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, subID); err != nil {
		t.Fatalf("cancel subscription concurrently: %v", err)
	}
	if err := changeViaStripe(t, svc, db, result.Order.ID, result.Order.Amount); err == nil {
		t.Fatal("实例已 cancelled 时结算必须失败")
	}
	assertOrderStatus(t, db, result.Order.ID, string(OrderPending))
	sub := readRenewalSubscription(t, db, subID)
	if sub.PlanID != oldPlanID || sub.Status != "cancelled" || sub.AmountTotal != 1000 {
		t.Fatalf("失败结算不得改写实例: %+v", sub)
	}
	if got := readSubscriptionEvents(t, db, subID); len(got) != 0 {
		t.Fatalf("失败结算不得追加事件，得到 %+v", got)
	}
}

// TestSubscriptionChangePercentAndRounding 用一笔精确的周期中点样本核对
// 「应补四舍五入到积分」在数据库路径上的落点（Step 1 的取整要求）。
//
// 旧价 10000 / 30 天、剩余 10 天 ⇒ 剩余价值 = round(3333.33) = 3333；
// 新价 20000 ⇒ 应补 = 16667（不是 16666.67，也不是 16667.33 截断出的 16666）。
func TestSubscriptionChangePercentAndRounding(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	stripe := newStripeTestServer(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)

	oldPlanID := insertSubscriptionPlan(t, db, 10000, 1000, 30, true, "enabled", "")
	newPlanID := insertSubscriptionPlan(t, db, 20000, 4000, 30, true, "enabled", "")
	now := time.Now().UTC().Truncate(time.Second)
	subID := insertChangeSubscription(t, db, userID, oldPlanID, "active",
		10000, 1000, 0, 30, now.Add(-20*24*time.Hour), now.Add(10*24*time.Hour))

	svc := newStripeTopupService(t, gormDB, stripe.URL)
	result, err := svc.ChangePlan(context.Background(), ChangePlanInput{
		UserID: userID, SubscriptionID: subID, NewPlanID: newPlanID,
		Provider: ProviderStripe, IdempotencyKey: fmt.Sprintf("change-round-%d", topupTestSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("create change: %v", err)
	}
	// 剩余秒向下取整只会让剩余价值变小或不变，因此应补落在 16667..16668。
	if result.PriceDelta < 16667 || result.PriceDelta > 16668 {
		t.Fatalf("应补差价 = %d，期望 16667..16668", result.PriceDelta)
	}
}
