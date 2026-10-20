//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// 本文件是 D5a 订阅套餐数据层的集成验收（billing-08 §2/§3/§5/§7、
// platform-04 §4.5 V10、R-D5-01…09）。
//
// 覆盖：
//   - 套餐 CRUD / 唯一键 / 启停 / 删除引用保护 / 批量重置的游标推进；
//   - 实例创建（order_id 唯一、快照列、end_at > start_at）；
//   - 条件更新终态（到期/取消/周期末取消，RowsAffected 语义）；
//   - 重置推进 next_reset_at，重复 pass 不双倍重置；
//   - 预扣记录幂等、已退款再预扣报错、单实例约束；
//   - amount_total=0 的无限额度语义（上界只在应用层）。
//
// 运行（隔离库，先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d5' \
//	GOPROXY=https://goproxy.cn,direct go test -tags=integration ./internal/store/ -run TestSubscription -v

func subscriptionCtx() context.Context { return context.Background() }

// subscriptionPlanKey 返回本次运行独占的套餐 key（plan_key 是 UNIQUE）。
func subscriptionPlanKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// subscriptionRequestID 生成 RFC 4122 v4 形态的 uuid 字符串（request_id 是
// PG uuid 列，非法格式会被 22P02 拒绝；这里直接产出合法值）。
func subscriptionRequestID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newTestPlan 创建套餐并登记清理：先删引用它的订阅，再删套餐（无 FK，
// 但删除保护会拒绝直接删被引用套餐）。
func newTestPlan(t *testing.T, repo *SQLSubscriptionRepo, db *sql.DB, prefix string) SubscriptionPlan {
	t.Helper()
	plan, err := repo.CreatePlan(subscriptionCtx(), SubscriptionPlanInput{
		PlanKey:      subscriptionPlanKey(prefix),
		Title:        "Test Plan",
		PeriodDays:   30,
		PriceAmount:  9900,
		Currency:     "CNY",
		GrantCredits: 1000,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id IN
			(SELECT id FROM subscriptions WHERE plan_id = $1)`, plan.ID)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE plan_id = $1`, plan.ID)
		_, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, plan.ID)
	})
	return plan
}

// createTestSubscription 创建实例并登记清理。
func createTestSubscription(t *testing.T, repo *SQLSubscriptionRepo, plan SubscriptionPlan,
	userID int64, start, end time.Time, orderID *int64) Subscription {
	t.Helper()
	sub, err := repo.CreateSubscription(subscriptionCtx(), SubscriptionCreateInput{
		UserID:              userID,
		PlanID:              plan.ID,
		PlanTitleSnapshot:   plan.Title,
		StartAt:             start,
		EndAt:               end,
		AmountTotal:         plan.GrantCredits,
		AmountUsed:          0,
		AllowWalletOverflow: plan.AllowWalletOverflow,
		OrderID:             orderID,
		Currency:            plan.Currency,
		PricePaidAmount:     plan.PriceAmount,
		PeriodDays:          plan.PeriodDays,
		ResetOnRenew:        plan.ResetOnRenew,
		GraceDays:           plan.GraceDays,
		ModelScope:          plan.ModelScope,
		ConcurrencyLimit:    plan.ConcurrencyLimit,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return sub
}

func TestSubscriptionPlanCRUD(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-crud")

	// 默认值（DB 列默认）必须按设计生效：reset_on_renew=true、
	// allow_wallet_overflow=true、status=enabled。
	if !plan.ResetOnRenew || !plan.AllowWalletOverflow || plan.Status != "enabled" {
		t.Fatalf("默认值不符：%+v", plan)
	}
	if plan.Currency != "CNY" || plan.GrantCredits != 1000 || plan.PeriodDays != 30 {
		t.Fatalf("套餐字段不符：%+v", plan)
	}

	// plan_key 唯一 → 409 哨兵。
	if _, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey: plan.PlanKey, Title: "dup", PeriodDays: 30, Currency: "CNY",
	}); !errors.Is(err, ErrPlanDuplicate) {
		t.Fatalf("重复 plan_key err = %v，want ErrPlanDuplicate", err)
	}
	// 应用层字段校验先于 DB CHECK。
	if _, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey: subscriptionPlanKey("d5a-bad"), Title: "x", PeriodDays: 0, Currency: "CNY",
	}); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("周期 0 err = %v，want ErrPlanInvalid", err)
	}
	if _, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey: subscriptionPlanKey("d5a-bad"), Title: "x", PeriodDays: 30,
		Currency: "USD",
	}); err == nil {
		t.Fatalf("非 CNY 币种应被 DB CHECK 拒绝")
	}

	// 编辑不影响已购实例：这里只验证套餐行本身被更新。
	updated, err := repo.UpdatePlan(ctx, plan.ID, SubscriptionPlanUpdate{
		PlanKey: plan.PlanKey, Title: "Renamed Plan", PeriodDays: 60,
		PriceAmount: 19900, Currency: "CNY", GrantCredits: 2000,
		ResetOnRenew: false, AllowWalletOverflow: false, Status: "disabled",
		SortOrder: 7,
	})
	if err != nil {
		t.Fatalf("update plan: %v", err)
	}
	if updated.Title != "Renamed Plan" || updated.PeriodDays != 60 ||
		updated.GrantCredits != 2000 || updated.ResetOnRenew ||
		updated.AllowWalletOverflow || updated.Status != "disabled" {
		t.Fatalf("更新结果不符：%+v", updated)
	}
	if _, err := repo.UpdatePlan(ctx, plan.ID, SubscriptionPlanUpdate{
		PlanKey: plan.PlanKey, Title: "x", PeriodDays: 30, Currency: "CNY", Status: "bogus",
	}); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("非法 status err = %v，want ErrPlanInvalid", err)
	}
	if _, err := repo.UpdatePlan(ctx, 1<<60, SubscriptionPlanUpdate{
		PlanKey: "nope", Title: "x", PeriodDays: 30, Currency: "CNY", Status: "enabled",
	}); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("更新不存在套餐 err = %v，want ErrPlanNotFound", err)
	}

	// 启停：disable 后用户侧 enabled 列表不再包含；全量列表仍包含。
	if err := repo.EnablePlan(ctx, plan.ID); err != nil {
		t.Fatalf("enable plan: %v", err)
	}
	enabled, err := repo.ListPlans(ctx, "enabled")
	if err != nil {
		t.Fatalf("list enabled plans: %v", err)
	}
	if !containsPlan(enabled, plan.ID) {
		t.Fatalf("enable 后 enabled 列表缺少套餐 %d", plan.ID)
	}
	if err := repo.DisablePlan(ctx, plan.ID); err != nil {
		t.Fatalf("disable plan: %v", err)
	}
	if err := repo.DisablePlan(ctx, plan.ID); err != nil {
		t.Fatalf("重复 disable 应幂等：%v", err)
	}
	enabled, err = repo.ListPlans(ctx, "enabled")
	if err != nil {
		t.Fatalf("list enabled plans: %v", err)
	}
	if containsPlan(enabled, plan.ID) {
		t.Fatalf("disable 后 enabled 列表仍包含套餐 %d", plan.ID)
	}
	all, err := repo.ListPlans(ctx, "")
	if err != nil {
		t.Fatalf("list all plans: %v", err)
	}
	if !containsPlan(all, plan.ID) {
		t.Fatalf("全量列表缺少套餐 %d", plan.ID)
	}
	if err := repo.EnablePlan(ctx, 1<<60); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("启用不存在套餐 err = %v，want ErrPlanNotFound", err)
	}
	if _, err := repo.GetPlan(ctx, 1<<60); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("读取不存在套餐 err = %v，want ErrPlanNotFound", err)
	}
}

func containsPlan(plans []SubscriptionPlan, id int64) bool {
	for _, p := range plans {
		if p.ID == id {
			return true
		}
	}
	return false
}

func TestSubscriptionPlanDeleteProtection(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-del")
	now := time.Now().UTC()

	sub := createTestSubscription(t, repo, plan, 900001, now.Add(-time.Hour), now.Add(time.Hour), nil)
	if err := repo.DeletePlan(ctx, plan.ID); !errors.Is(err, ErrPlanReferenced) {
		t.Fatalf("有实例引用时删除 err = %v，want ErrPlanReferenced", err)
	}
	// 终态实例仍是逻辑引用（历史可追溯），不能因取消而放行删除。
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}
	if err := repo.DeletePlan(ctx, plan.ID); !errors.Is(err, ErrPlanReferenced) {
		t.Fatalf("终态实例引用时删除 err = %v，want ErrPlanReferenced", err)
	}

	if _, err := db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("cleanup subscription: %v", err)
	}
	if err := repo.DeletePlan(ctx, plan.ID); err != nil {
		t.Fatalf("无引用删除: %v", err)
	}
	if err := repo.DeletePlan(ctx, plan.ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("重复删除 err = %v，want ErrPlanNotFound", err)
	}
}

func TestSubscriptionCreateAndRead(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-create")
	now := time.Now().UTC()

	orderID := now.UnixNano()
	scope := []byte(`["gpt-4o"]`)
	limit := int32(3)
	in := SubscriptionCreateInput{
		UserID:              900002,
		PlanID:              plan.ID,
		PlanTitleSnapshot:   "Snapshot Title",
		StartAt:             now.Add(-time.Minute),
		EndAt:               now.Add(29 * 24 * time.Hour),
		NextResetAt:         timePtr(now.Add(30 * 24 * time.Hour)),
		AmountTotal:         500,
		AmountUsed:          10,
		AllowWalletOverflow: true,
		OrderID:             &orderID,
		Currency:            "CNY",
		PricePaidAmount:     5000,
		PeriodDays:          30,
		ResetOnRenew:        true,
		GraceDays:           2,
		ModelScope:          scope,
		ConcurrencyLimit:    &limit,
	}
	sub, err := repo.CreateSubscription(ctx, in)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID)
	})
	// 快照列必须原样落库（R-D5-03），而不是回读当前套餐。
	if sub.PlanTitleSnapshot != "Snapshot Title" || sub.PeriodDays != 30 ||
		sub.GraceDays != 2 || sub.AmountTotal != 500 || sub.AmountUsed != 10 ||
		string(sub.ModelScope) != `["gpt-4o"]` ||
		sub.ConcurrencyLimit == nil || *sub.ConcurrencyLimit != 3 {
		t.Fatalf("快照列不符：%+v", sub)
	}
	if sub.Status != "active" {
		t.Fatalf("新实例 status = %q，want active", sub.Status)
	}

	// 同一 order_id 只能建一条实例（回调重复投递的最后防线）。
	dup := in
	if _, err := repo.CreateSubscription(ctx, dup); !errors.Is(err, ErrSubscriptionDuplicate) {
		t.Fatalf("重复 order_id err = %v，want ErrSubscriptionDuplicate", err)
	}

	// end_at 必须晚于 start_at（应用层先拒绝，DB CHECK 兜底）。
	bad := in
	bad.OrderID = nil
	bad.EndAt = bad.StartAt
	if _, err := repo.CreateSubscription(ctx, bad); !errors.Is(err, ErrSubscriptionInvalid) {
		t.Fatalf("end_at<=start_at err = %v，want ErrSubscriptionInvalid", err)
	}
	// 套餐不存在 / 已删除 → ErrPlanNotFound（FOR SHARE 锁行校验）。
	bad = in
	bad.OrderID = nil
	bad.PlanID = 1 << 60
	if _, err := repo.CreateSubscription(ctx, bad); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("不存在套餐 err = %v，want ErrPlanNotFound", err)
	}

	got, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if got.ID != sub.ID || got.UserID != 900002 {
		t.Fatalf("get 结果不符：%+v", got)
	}
	list, err := repo.ListSubscriptionsByUser(ctx, 900002)
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if len(list) != 1 || list[0].ID != sub.ID {
		t.Fatalf("list by user = %+v，want 仅实例 %d", list, sub.ID)
	}
	if _, err := repo.GetSubscription(ctx, 1<<60); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("读取不存在实例 err = %v，want ErrSubscriptionNotFound", err)
	}

	// 读侧资金分派契约（billing-08 §4.1）。
	active, err := repo.HasActiveUserSubscription(ctx, 900002, now)
	if err != nil || !active {
		t.Fatalf("HasActiveUserSubscription = %v/%v，want true/nil", active, err)
	}
	active, err = repo.HasActiveUserSubscription(ctx, 900002, now.Add(31*24*time.Hour))
	if err != nil || active {
		t.Fatalf("过了 end_at+grace 后仍 active = %v/%v，want false/nil", active, err)
	}
	overflow, err := repo.UserActiveSubscriptionsAllowWalletOverflow(ctx, 900002, now)
	if err != nil || !overflow {
		t.Fatalf("UserActiveSubscriptionsAllowWalletOverflow = %v/%v，want true/nil", overflow, err)
	}
	actives, err := repo.ListActiveSubscriptions(ctx, 900002, now)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(actives) != 1 || actives[0].ID != sub.ID {
		t.Fatalf("list active = %+v，want 实例 %d", actives, sub.ID)
	}
}

func TestSubscriptionConditionalTransitions(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-state")
	now := time.Now().UTC()

	// 周期末取消：active 可置位且幂等；取消后实例仍 active。
	sub := createTestSubscription(t, repo, plan, 900003, now.Add(-time.Hour), now.Add(time.Hour), nil)
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); err != nil {
		t.Fatalf("cancel at period end: %v", err)
	}
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); err != nil {
		t.Fatalf("重复 cancel at period end 应幂等：%v", err)
	}
	got, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil || !got.CancelAtPeriodEnd || got.Status != "active" {
		t.Fatalf("周期末取消后 = %+v/%v，want active + flag", got, err)
	}

	// 立即取消：active → cancelled；重复调用幂等；之后周期末取消不再适用。
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("重复 cancel immediately 应幂等：%v", err)
	}
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); !errors.Is(err, ErrSubscriptionNotActive) {
		t.Fatalf("终态 period-end cancel err = %v，want ErrSubscriptionNotActive", err)
	}
	if err := repo.CancelImmediately(ctx, 1<<60); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("不存在实例 err = %v，want ErrSubscriptionNotFound", err)
	}

	// 到期：错过的实例被条件更新置 expired；重复 pass 幂等。
	expired := createTestSubscription(t, repo, plan, 900004,
		now.Add(-2*time.Hour), now.Add(-time.Hour), nil)
	// 宽限期内的实例不能被误杀：end_at 已过，但 end_at+grace_days 未到。
	graceSub := createTestSubscription(t, repo, plan, 900005,
		now.Add(-2*time.Hour), now.Add(-time.Hour), nil)
	if _, err := db.Exec(`UPDATE subscriptions SET grace_days = 5 WHERE id = $1`, graceSub.ID); err != nil {
		t.Fatalf("set grace: %v", err)
	}
	// 未来实例不受影响。
	future := createTestSubscription(t, repo, plan, 900006, now.Add(-time.Hour), now.Add(time.Hour), nil)

	affected, err := repo.ExpireDue(ctx, now)
	if err != nil {
		t.Fatalf("expire due: %v", err)
	}
	if affected < 1 {
		t.Fatalf("expire due affected = %d，want >= 1（应命中无宽限的过期实例）", affected)
	}
	got, err = repo.GetSubscription(ctx, expired.ID)
	if err != nil || got.Status != "expired" {
		t.Fatalf("过期实例 status = %q/%v，want expired", got.Status, err)
	}
	got, err = repo.GetSubscription(ctx, graceSub.ID)
	if err != nil || got.Status != "active" {
		t.Fatalf("宽限期内实例 status = %q/%v，want active", got.Status, err)
	}
	got, err = repo.GetSubscription(ctx, future.ID)
	if err != nil || got.Status != "active" {
		t.Fatalf("未来实例 status = %q/%v，want active", got.Status, err)
	}
	affected, err = repo.ExpireDue(ctx, now)
	if err != nil || affected != 0 {
		t.Fatalf("重复到期 pass affected = %d/%v，want 0/nil", affected, err)
	}
}

func TestSubscriptionQuotaReset(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-reset")
	// PG timestamptz 是微秒精度；断言 next_reset_at 精确推进时必须用同一
	// 精度的时间，否则 Go 的纳秒尾数会在落库时被截断，产生假失败。
	now := time.Now().UTC().Truncate(time.Microsecond)

	oldReset := now.Add(-time.Minute)
	sub, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID: 900007, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-31 * 24 * time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: &oldReset, AmountTotal: 1000, AmountUsed: 400,
		Currency: "CNY", PricePaidAmount: 9900, PeriodDays: 30, ResetOnRenew: true,
	})
	if err != nil {
		t.Fatalf("create reset subscription: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID) })

	due, err := repo.DueForReset(ctx, now)
	if err != nil {
		t.Fatalf("due for reset: %v", err)
	}
	if !containsSubscription(due, sub.ID) {
		t.Fatalf("DueForReset 未包含到期实例 %d", sub.ID)
	}

	affected, err := repo.ResetSubscriptionQuota(ctx, sub.ID, now)
	if err != nil || affected != 1 {
		t.Fatalf("reset quota = %d/%v，want 1/nil", affected, err)
	}
	got, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if got.AmountUsed != 0 {
		t.Fatalf("重置后 amount_used = %d，want 0", got.AmountUsed)
	}
	if got.NextResetAt == nil || !got.NextResetAt.Equal(oldReset.Add(30*24*time.Hour)) {
		t.Fatalf("next_reset_at = %v，want %v（推进一个周期）", got.NextResetAt, oldReset.Add(30*24*time.Hour))
	}

	// 同一 now 再跑一次：游标已前移，不能再重置（否则双倍赠额）。
	affected, err = repo.ResetSubscriptionQuota(ctx, sub.ID, now)
	if err != nil || affected != 0 {
		t.Fatalf("重复 reset affected = %d/%v，want 0/nil", affected, err)
	}

	// amount_total = 0 表示无限额度：DB 不做上界守卫（R-D5-07），
	// 应用层（会话）才在 amount_total > 0 时拒绝超总额。
	unlimited, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID: 900008, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: &oldReset, AmountTotal: 0, AmountUsed: 0,
		Currency: "CNY", PricePaidAmount: 0, PeriodDays: 30, ResetOnRenew: true,
	})
	if err != nil {
		t.Fatalf("create unlimited subscription: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, unlimited.ID) })
	if _, err := db.Exec(`UPDATE subscriptions SET amount_used = 500 WHERE id = $1`, unlimited.ID); err != nil {
		t.Fatalf("无限额度实例应允许 amount_used > amount_total：%v", err)
	}
	if affected, err := repo.ResetSubscriptionQuota(ctx, unlimited.ID, now); err != nil || affected != 1 {
		t.Fatalf("无限额度 reset = %d/%v，want 1/nil", affected, err)
	}

	// 按套餐批量重置：advanceNextReset=true 同步推进游标。
	batch, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID: 900009, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: timePtr(now.Add(10 * 24 * time.Hour)), AmountTotal: 100,
		AmountUsed: 70, Currency: "CNY", PricePaidAmount: 1000, PeriodDays: 30,
		ResetOnRenew: true,
	})
	if err != nil {
		t.Fatalf("create batch subscription: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, batch.ID) })
	if _, err := db.Exec(`UPDATE subscriptions SET amount_used = 70 WHERE id = $1`, batch.ID); err != nil {
		t.Fatalf("seed batch used: %v", err)
	}
	affected, err = repo.ResetPlanQuota(ctx, plan.ID, true)
	if err != nil {
		t.Fatalf("reset plan quota: %v", err)
	}
	if affected < 1 {
		t.Fatalf("ResetPlanQuota affected = %d，want >= 1", affected)
	}
	got, err = repo.GetSubscription(ctx, batch.ID)
	if err != nil {
		t.Fatalf("get batch: %v", err)
	}
	if got.AmountUsed != 0 {
		t.Fatalf("批量重置后 amount_used = %d，want 0", got.AmountUsed)
	}
	wantNext := now.Add(10 * 24 * time.Hour).Add(30 * 24 * time.Hour)
	if got.NextResetAt == nil || !got.NextResetAt.Equal(wantNext) {
		t.Fatalf("批量重置 next_reset_at = %v，want %v", got.NextResetAt, wantNext)
	}
	if _, err := repo.ResetPlanQuota(ctx, 1<<60, true); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("不存在套餐 reset err = %v，want ErrPlanNotFound", err)
	}
}

func containsSubscription(subs []Subscription, id int64) bool {
	for _, s := range subs {
		if s.ID == id {
			return true
		}
	}
	return false
}

func TestSubscriptionPreConsumeIdempotency(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-pre")
	now := time.Now().UTC()

	sub := createTestSubscription(t, repo, plan, 900010, now.Add(-time.Hour), now.Add(time.Hour), nil)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
	})

	requestID := subscriptionRequestID(t)
	rec, err := repo.InsertPreConsume(ctx, requestID, sub.ID, 120)
	if err != nil {
		t.Fatalf("insert pre-consume: %v", err)
	}
	if rec.Status != "consumed" || rec.PreConsumed != 120 || rec.SubscriptionID != sub.ID {
		t.Fatalf("预扣记录不符：%+v", rec)
	}

	// 同 requestId 再预扣：幂等返回既有值（金额不被第二次调用改写）。
	again, err := repo.InsertPreConsume(ctx, requestID, sub.ID, 999)
	if err != nil {
		t.Fatalf("幂等预扣: %v", err)
	}
	if again.PreConsumed != 120 {
		t.Fatalf("幂等预扣 pre_consumed = %d，want 120（既有值）", again.PreConsumed)
	}
	// 一次预扣只能落一个实例：行上只有一个 subscription_id。
	other := createTestSubscription(t, repo, plan, 900011, now.Add(-time.Hour), now.Add(time.Hour), nil)
	third, err := repo.InsertPreConsume(ctx, requestID, other.ID, 50)
	if err != nil {
		t.Fatalf("同 requestId 不同实例应按幂等返回既有行：%v", err)
	}
	if third.SubscriptionID != sub.ID {
		t.Fatalf("同一 requestId 落到实例 %d，want 首个实例 %d（禁止跨实例拆分）",
			third.SubscriptionID, sub.ID)
	}

	// 退款幂等；退款后再预扣必须报错（防双花）。
	if err := repo.MarkPreConsumeRefunded(ctx, requestID); err != nil {
		t.Fatalf("mark refunded: %v", err)
	}
	if err := repo.MarkPreConsumeRefunded(ctx, requestID); err != nil {
		t.Fatalf("重复退款应幂等：%v", err)
	}
	got, err := repo.GetPreConsume(ctx, requestID)
	if err != nil || got.Status != "refunded" {
		t.Fatalf("退款后状态 = %q/%v，want refunded/nil", got.Status, err)
	}
	if _, err := repo.InsertPreConsume(ctx, requestID, sub.ID, 120); !errors.Is(err, ErrPreConsumeRefunded) {
		t.Fatalf("已退款再预扣 err = %v，want ErrPreConsumeRefunded", err)
	}

	// 参数校验与不存在记录。
	if _, err := repo.InsertPreConsume(ctx, subscriptionRequestID(t), sub.ID, 0); !errors.Is(err, ErrPreConsumeInvalid) {
		t.Fatalf("amount=0 err = %v，want ErrPreConsumeInvalid", err)
	}
	if _, err := repo.InsertPreConsume(ctx, "not-a-uuid", sub.ID, 1); !errors.Is(err, ErrPreConsumeInvalid) {
		t.Fatalf("非法 uuid err = %v，want ErrPreConsumeInvalid", err)
	}
	missing := subscriptionRequestID(t)
	if _, err := repo.GetPreConsume(ctx, missing); !errors.Is(err, ErrPreConsumeNotFound) {
		t.Fatalf("读取不存在预扣 err = %v，want ErrPreConsumeNotFound", err)
	}
	if err := repo.MarkPreConsumeRefunded(ctx, missing); !errors.Is(err, ErrPreConsumeNotFound) {
		t.Fatalf("退款不存在预扣 err = %v，want ErrPreConsumeNotFound", err)
	}
}

func TestSubscriptionPreConsumeConcurrentSameRequest(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-pre-conc")
	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 900012, now.Add(-time.Hour), now.Add(time.Hour), nil)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
	})

	requestID := subscriptionRequestID(t)
	const workers = 4
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, err := repo.InsertPreConsume(ctx, requestID, sub.ID, 10)
			errs <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发预扣 err = %v，want nil", err)
		}
	}
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM subscription_pre_consume_records WHERE request_id = $1`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("count pre-consume: %v", err)
	}
	if n != 1 {
		t.Fatalf("并发同 requestId 记录数 = %d，want 1（唯一键收敛）", n)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
