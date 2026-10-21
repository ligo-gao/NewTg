//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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

// requirePlanReferenced 断言删除被引用保护拒绝，并核对两类计数。
// wantInstances 是 subscriptions 行数，wantOrders 是未终结订阅订单数。
func requirePlanReferenced(t *testing.T, err error, wantInstances, wantOrders int64) {
	t.Helper()
	var ref ErrPlanReferenced
	if !errors.As(err, &ref) {
		t.Fatalf("err = %v，want ErrPlanReferenced", err)
	}
	if ref.OrderReferenceCount != wantOrders {
		t.Fatalf("OrderReferenceCount = %d，want %d", ref.OrderReferenceCount, wantOrders)
	}
	if ref.ReferenceCount != wantInstances+wantOrders {
		t.Fatalf("ReferenceCount = %d，want %d（实例 %d + 订单 %d）",
			ref.ReferenceCount, wantInstances+wantOrders, wantInstances, wantOrders)
	}
}

func TestSubscriptionPlanDeleteProtection(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-del")
	now := time.Now().UTC()

	sub := createTestSubscription(t, repo, plan, 900001, now.Add(-time.Hour), now.Add(time.Hour), nil)
	requirePlanReferenced(t, repo.DeletePlan(ctx, plan.ID), 1, 0)
	// 终态实例仍是逻辑引用（历史可追溯），不能因取消而放行删除。
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}
	requirePlanReferenced(t, repo.DeletePlan(ctx, plan.ID), 1, 0)

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

// insertSubscribeOrder 插入一条订阅购买订单（biz_ref_id 指向套餐）。
func insertSubscribeOrder(t *testing.T, db *sql.DB, planID int64, status string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status,
			idempotency_key, biz_kind, biz_ref_id)
		VALUES (900090, 'stripe', 9900, 'CNY', $1, $2, 'subscribe', $3)
		RETURNING id`,
		status, fmt.Sprintf("d5a-order-%d-%s", time.Now().UnixNano(), status), planID).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscribe order: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM orders WHERE id = $1`, id) })
	return id
}

// TestSubscriptionPlanDeleteProtectionByOrders：没有实例、但存在未终结的
// 订阅订单时，套餐同样不能被删除（MEDIUM-1）。
//
// 场景：用户已下单（orders pending），支付回调尚未创建实例。此时套餐仍是
// 履约依据，删除会让回调撞 ErrPlanNotFound、订单成为孤儿。pending 与
// succeeded 都算引用；cancelled/expired/failed 是终态，不再阻塞。
func TestSubscriptionPlanDeleteProtectionByOrders(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()

	plan := newTestPlan(t, repo, db, "d5a-del-order")
	orderID := insertSubscribeOrder(t, db, plan.ID, "pending")

	// 无实例、仅一条 pending 订单 → 409，且套餐保留。
	requirePlanReferenced(t, repo.DeletePlan(ctx, plan.ID), 0, 1)
	if _, err := repo.GetPlan(ctx, plan.ID); err != nil {
		t.Fatalf("被订单引用时套餐应保留：GetPlan err = %v", err)
	}

	// 已支付但回调尚未落成实例同样是引用。
	if _, err := db.Exec(`UPDATE orders SET status = 'succeeded' WHERE id = $1`, orderID); err != nil {
		t.Fatalf("set order succeeded: %v", err)
	}
	requirePlanReferenced(t, repo.DeletePlan(ctx, plan.ID), 0, 1)

	// 取消后不再阻塞：用户放弃支付不应让套餐永远删不掉。
	if _, err := db.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, orderID); err != nil {
		t.Fatalf("set order cancelled: %v", err)
	}
	if err := repo.DeletePlan(ctx, plan.ID); err != nil {
		t.Fatalf("订单取消后删除套餐: %v", err)
	}

	// 过期订单同样不阻塞（终态口径与 cancelled 一致）。
	expiredPlan := newTestPlan(t, repo, db, "d5a-del-order-expired")
	insertSubscribeOrder(t, db, expiredPlan.ID, "expired")
	if err := repo.DeletePlan(ctx, expiredPlan.ID); err != nil {
		t.Fatalf("订单过期后删除套餐: %v", err)
	}
}

// TestSubscriptionModelScopeNullVsEmpty：model_scope 的 NULL（全部模型）与
// '[]'（没有任何可用模型）必须可区分地落库与读回（LOW-6）。
//
// 二者都合法，但语义相反。nullableJSON 把 nil 写成 SQL NULL、把非空字节写
// 成 jsonb 原文；若任一侧被「空值即 NULL」的归一吞掉，套餐/实例的准入范围
// 会从「无模型可用」静默变成「全部模型」，属于授权扩大。
func TestSubscriptionModelScopeNullVsEmpty(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	now := time.Now().UTC()

	createPlan := func(prefix string, scope []byte) SubscriptionPlan {
		t.Helper()
		p, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
			PlanKey: subscriptionPlanKey(prefix), Title: "Scope Plan",
			PeriodDays: 30, PriceAmount: 100, Currency: "CNY", GrantCredits: 10,
			ModelScope: scope,
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", prefix, err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM subscriptions WHERE plan_id = $1`, p.ID)
			_, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, p.ID)
		})
		return p
	}

	planAll := createPlan("d5a-scope-null", nil)
	planNone := createPlan("d5a-scope-empty", []byte(`[]`))

	// 套餐侧：NULL 读回 nil；'[]' 读回字面量（不是 nil）。
	gotAll, err := repo.GetPlan(ctx, planAll.ID)
	if err != nil {
		t.Fatalf("get null-scope plan: %v", err)
	}
	if gotAll.ModelScope != nil {
		t.Fatalf("NULL model_scope 读回 %q，want nil（全部模型）", gotAll.ModelScope)
	}
	gotNone, err := repo.GetPlan(ctx, planNone.ID)
	if err != nil {
		t.Fatalf("get empty-scope plan: %v", err)
	}
	if string(gotNone.ModelScope) != "[]" {
		t.Fatalf("'[]' model_scope 读回 %q，want []（无可用模型）", gotNone.ModelScope)
	}

	// DB 侧直接确认两种存储形态不同。
	var jsonIsNull bool
	if err := db.QueryRow(`SELECT model_scope IS NULL FROM subscription_plans WHERE id = $1`,
		planAll.ID).Scan(&jsonIsNull); err != nil || !jsonIsNull {
		t.Fatalf("NULL scope 落库形态 = %v/%v，want jsonb IS NULL", jsonIsNull, err)
	}
	var typeof string
	if err := db.QueryRow(`SELECT jsonb_typeof(model_scope) FROM subscription_plans WHERE id = $1`,
		planNone.ID).Scan(&typeof); err != nil || typeof != "array" {
		t.Fatalf("'[]' scope 落库形态 = %q/%v，want jsonb array", typeof, err)
	}

	// 实例侧同样区分（快照列不得把 '[]' 折成 NULL）。
	createSub := func(plan SubscriptionPlan, userID int64) Subscription {
		t.Helper()
		s, createErr := repo.CreateSubscription(ctx, SubscriptionCreateInput{
			UserID: userID, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
			StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
			AmountTotal: plan.GrantCredits, Currency: "CNY", PeriodDays: 30,
			ResetOnRenew: true, ModelScope: plan.ModelScope,
		})
		if createErr != nil {
			t.Fatalf("create subscription: %v", createErr)
		}
		return s
	}
	subAll := createSub(planAll, 900030)
	subNone := createSub(planNone, 900031)
	if subAll.ModelScope != nil {
		t.Fatalf("实例 NULL model_scope 读回 %q，want nil", subAll.ModelScope)
	}
	if string(subNone.ModelScope) != "[]" {
		t.Fatalf("实例 '[]' model_scope 读回 %q，want []", subNone.ModelScope)
	}
}

// TestSubscriptionTextOverflowMapping：char(3) 超长（SQLSTATE 22001）必须
// 映射为可读的输入错误，而不是原始数据库错误（LOW-12）。
//
// currency 是 char(3)；传四位币种时 PG 在求值 CHECK 前先报
// string_data_right_truncation（22001），不会走 23514 分支。漏映射会让
// handler 把用户输入错误当作 500。
func TestSubscriptionTextOverflowMapping(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	now := time.Now().UTC()

	if _, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey: subscriptionPlanKey("d5a-overflow"), Title: "Overflow",
		PeriodDays: 30, Currency: "USDX",
	}); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("四位币种创建套餐 err = %v，want ErrPlanInvalid", err)
	}

	plan := newTestPlan(t, repo, db, "d5a-overflow-sub")
	if _, err := repo.UpdatePlan(ctx, plan.ID, SubscriptionPlanUpdate{
		PlanKey: plan.PlanKey, Title: "Overflow", PeriodDays: 30,
		Currency: "USDX", Status: "enabled",
	}); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("四位币种更新套餐 err = %v，want ErrPlanInvalid", err)
	}

	if _, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID: 900040, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		AmountTotal: 10, Currency: "USDX", PricePaidAmount: 0,
		PeriodDays: 30, ResetOnRenew: true,
	}); !errors.Is(err, ErrSubscriptionInvalid) {
		t.Fatalf("四位币种创建实例 err = %v，want ErrSubscriptionInvalid", err)
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

	// 立即取消（R-48）：active 仍是 active，end_at 提前到「现在」并清掉
	// 周期末标记，进入快照宽限期；重复调用幂等（不重复写事件）。
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("重复 cancel immediately 应幂等：%v", err)
	}
	got, err = repo.GetSubscription(ctx, sub.ID)
	if err != nil || got.Status != "active" || got.CancelAtPeriodEnd {
		t.Fatalf("立即取消后 = %+v/%v，want active 且无周期末标记", got, err)
	}
	if got.EndAt.After(time.Now().UTC()) {
		t.Fatalf("立即取消后 end_at = %v，期望不晚于 now", got.EndAt)
	}
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); err != nil {
		t.Fatalf("立即取消后仍可置周期末取消（宽限期内）：%v", err)
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

// TestSubscriptionResetOnRenewContract：reset_on_renew=false 的「不周期重置」
// 合同必须在数据层两端自洽（LOW-3）。
//
//  1. 创建时把调用方误传的 next_reset_at 归一为 NULL——否则 DueForReset 会
//     永远扫到它，而重置条件又因 reset_on_renew=false 拒绝，形成僵尸行；
//  2. ResetSubscriptionQuota 的 WHERE 必须带 reset_on_renew——历史行/人工
//     UPDATE 残留的游标也不能绕过快照列的合同。
func TestSubscriptionResetOnRenewContract(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-reset-contract")
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 1. 创建时归一：调用方传了游标，但 reset_on_renew=false。
	strayReset := now.Add(-time.Minute)
	sub, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID: 900020, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: &strayReset, AmountTotal: 1000, AmountUsed: 100,
		Currency: "CNY", PricePaidAmount: 9900, PeriodDays: 30, ResetOnRenew: false,
	})
	if err != nil {
		t.Fatalf("create non-reset subscription: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID) })
	if sub.NextResetAt != nil {
		t.Fatalf("reset_on_renew=false 时 next_reset_at = %v，want NULL", sub.NextResetAt)
	}
	got, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil || got.NextResetAt != nil {
		t.Fatalf("落库后 next_reset_at = %v/%v，want NULL/nil", got.NextResetAt, err)
	}
	due, err := repo.DueForReset(ctx, now)
	if err != nil {
		t.Fatalf("due for reset: %v", err)
	}
	if containsSubscription(due, sub.ID) {
		t.Fatalf("reset_on_renew=false 的实例不应进入 B12 重置扫描")
	}

	// 2. 重置条件带 reset_on_renew：模拟历史行残留的过期游标。
	legacy, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID: 900021, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: &strayReset, AmountTotal: 1000, AmountUsed: 400,
		Currency: "CNY", PricePaidAmount: 9900, PeriodDays: 30, ResetOnRenew: true,
	})
	if err != nil {
		t.Fatalf("create legacy subscription: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, legacy.ID) })
	if _, err := db.Exec(`UPDATE subscriptions
		SET reset_on_renew = false, next_reset_at = $2 WHERE id = $1`, legacy.ID, strayReset); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	affected, err := repo.ResetSubscriptionQuota(ctx, legacy.ID, now)
	if err != nil || affected != 0 {
		t.Fatalf("reset_on_renew=false 的 reset = %d/%v，want 0/nil", affected, err)
	}
	got, err = repo.GetSubscription(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("get legacy: %v", err)
	}
	if got.AmountUsed != 400 || got.NextResetAt == nil || !got.NextResetAt.Equal(strayReset) {
		t.Fatalf("被拒绝的重置改动了实例：amount_used=%d next_reset_at=%v", got.AmountUsed, got.NextResetAt)
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

func TestSubscriptionPreConsumeUserSubscriptionAtomic(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4a-pre-atomic")
	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 900020, now.Add(-time.Hour), now.Add(time.Hour), nil)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
	})

	readUsed := func() int64 {
		t.Helper()
		var used int64
		if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
			t.Fatalf("read amount_used: %v", err)
		}
		return used
	}

	requestID := subscriptionRequestID(t)
	rec, created, err := repo.PreConsumeUserSubscription(ctx, requestID, sub.ID, 120)
	if err != nil || !created {
		t.Fatalf("首次预扣 = %+v/%v/%v，want created true/nil", rec, created, err)
	}
	if rec.SubscriptionID != sub.ID || rec.PreConsumed != 120 || rec.Status != "consumed" {
		t.Fatalf("预扣记录不符：%+v", rec)
	}
	if got := readUsed(); got != 120 {
		t.Fatalf("amount_used = %d，want 120", got)
	}

	// R-W9B-02：预扣成功必须在同一事务内留下 in_flight 租约（含 user_id
	// 与预扣额），否则「预扣已落、租约缺失」的崩溃窗口会让兜底扫描把仍在
	// 运行的长任务误退。
	var (
		leaseState string
		leaseUser  int64
		leasePre   int64
		leaseFresh bool
	)
	if err := db.QueryRow(`
		SELECT state, user_id, pre_deducted, heartbeat_at > now() - interval '30s'
		  FROM request_registry WHERE request_id = $1`, requestID).
		Scan(&leaseState, &leaseUser, &leasePre, &leaseFresh); err != nil {
		t.Fatalf("read inflight lease: %v", err)
	}
	if leaseState != "in_flight" || leaseUser != 900020 || leasePre != 120 || !leaseFresh {
		t.Fatalf("in_flight 租约 = state %q user %d pre %d fresh %v，want in_flight/900020/120/true",
			leaseState, leaseUser, leasePre, leaseFresh)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_registry WHERE user_id = 900020`)
	})

	// 同 requestId 再预扣：幂等返回既有值，amount_used 不再增加。
	again, created, err := repo.PreConsumeUserSubscription(ctx, requestID, sub.ID, 999)
	if err != nil || created {
		t.Fatalf("幂等预扣 = %+v created=%v err=%v，want created false/nil", again, created, err)
	}
	if again.PreConsumed != 120 {
		t.Fatalf("幂等预扣 pre_consumed = %d，want 120", again.PreConsumed)
	}
	if got := readUsed(); got != 120 {
		t.Fatalf("幂等预扣后 amount_used = %d，want 120（不双扣）", got)
	}

	// 超上界：失败尝试必须整体回滚（不留下幂等记录、不改 used）。
	overID := subscriptionRequestID(t)
	if _, _, err := repo.PreConsumeUserSubscription(ctx, overID, sub.ID, 900); !errors.Is(err, ErrSubscriptionQuotaInsufficient) {
		t.Fatalf("超上界 err = %v，want ErrSubscriptionQuotaInsufficient", err)
	}
	if got := readUsed(); got != 120 {
		t.Fatalf("失败预扣后 amount_used = %d，want 120", got)
	}
	if _, err := repo.GetPreConsume(ctx, overID); !errors.Is(err, ErrPreConsumeNotFound) {
		t.Fatalf("失败预扣留下幂等记录：err = %v，want ErrPreConsumeNotFound", err)
	}

	// 退款：状态置 refunded 且 amount_used 回退；重复退款幂等。
	if err := repo.MarkPreConsumeRefunded(ctx, requestID); err != nil {
		t.Fatalf("退款: %v", err)
	}
	if got := readUsed(); got != 0 {
		t.Fatalf("退款后 amount_used = %d，want 0", got)
	}
	if err := repo.MarkPreConsumeRefunded(ctx, requestID); err != nil {
		t.Fatalf("重复退款应幂等：%v", err)
	}
	if got := readUsed(); got != 0 {
		t.Fatalf("重复退款后 amount_used = %d，want 0（不重复回退）", got)
	}

	// 已退款再预扣：硬报错，且不得再改额度。
	if _, _, err := repo.PreConsumeUserSubscription(ctx, requestID, sub.ID, 120); !errors.Is(err, ErrPreConsumeRefunded) {
		t.Fatalf("已退款再预扣 err = %v，want ErrPreConsumeRefunded", err)
	}
	if got := readUsed(); got != 0 {
		t.Fatalf("已退款再预扣后 amount_used = %d，want 0", got)
	}

	// 新 requestId 在退款后可以重新使用该实例。
	freshID := subscriptionRequestID(t)
	if _, created, err := repo.PreConsumeUserSubscription(ctx, freshID, sub.ID, 900); err != nil || !created {
		t.Fatalf("退款后新预扣 = created %v err %v，want true/nil", created, err)
	}
	if got := readUsed(); got != 900 {
		t.Fatalf("新预扣后 amount_used = %d，want 900", got)
	}
}

func TestSubscriptionPreConsumeUserSubscriptionUnlimitedAndConcurrent(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()

	// amount_total = 0（无限额度）：任意多次预扣都成功，上界守卫不生效。
	unlimitedPlan, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey: subscriptionPlanKey("s4a-unlimited"), Title: "Unlimited",
		PeriodDays: 30, PriceAmount: 0, Currency: "CNY", GrantCredits: 0,
	})
	if err != nil {
		t.Fatalf("create unlimited plan: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, unlimitedPlan.ID) })
	now := time.Now().UTC()
	unlimited := createTestSubscription(t, repo, unlimitedPlan, 900021, now.Add(-time.Hour), now.Add(time.Hour), nil)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, unlimited.ID)
	})

	requestID := subscriptionRequestID(t)
	const workers = 4
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, _, err := repo.PreConsumeUserSubscription(ctx, requestID, unlimited.ID, 10)
			errs <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发同 requestId 预扣 err = %v，want nil", err)
		}
	}
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, unlimited.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	if used != 10 {
		t.Fatalf("并发预扣 amount_used = %d，want 10（唯一键收敛为一次扣减）", used)
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

// TestSubscriptionRenew 覆盖 P2a 的条件扩展：同一实例推进 end_at、清零
// amount_used、重置游标、恢复 active、清 cancel_at_period_end、按续费快照
// 更新派生列；cancelled 不可续费；归属/套餐不符统一 NotFound。
func TestSubscriptionRenew(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "p2a-renew")
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("active 续费推进并更新快照列", func(t *testing.T) {
		oldEnd := now.Add(10 * 24 * time.Hour)
		sub := createTestSubscription(t, repo, plan, 900003, now.Add(-20*24*time.Hour), oldEnd, nil)
		if _, err := db.Exec(`UPDATE subscriptions
			SET amount_used = 7, cancel_at_period_end = true WHERE id = $1`, sub.ID); err != nil {
			t.Fatalf("seed used/cancel flag: %v", err)
		}
		scope := []byte(`["gpt-4o"]`)
		limit := int32(2)
		got, err := RenewSubscription(ctx, db, RenewalExtendInput{
			SubscriptionID: sub.ID, UserID: 900003, PlanID: plan.ID,
			PlanTitleSnapshot: "Renewed Plan", PeriodDays: 30,
			GrantCredits: 4242, ResetOnRenew: true, AllowWalletOverflow: false,
			ModelScope: scope, ConcurrencyLimit: &limit, GraceDays: 3,
			Currency: "CNY", PricePaidAmount: 19900, Now: now, ExpectedEndAt: oldEnd,
		})
		if err != nil {
			t.Fatalf("renew active: %v", err)
		}
		if !got.EndAt.UTC().Equal(oldEnd.Add(30 * 24 * time.Hour)) {
			t.Fatalf("end_at = %s，期望 %s", got.EndAt, oldEnd.Add(30*24*time.Hour))
		}
		if got.AmountUsed != 0 || got.AmountTotal != 4242 || got.Status != "active" ||
			got.CancelAtPeriodEnd {
			t.Fatalf("续费后状态 = %+v", got)
		}
		if got.NextResetAt == nil || !got.NextResetAt.UTC().Equal(got.EndAt.UTC()) {
			t.Fatalf("next_reset_at = %v，期望等于 end_at %s", got.NextResetAt, got.EndAt)
		}
		if got.PlanTitleSnapshot != "Renewed Plan" || got.PeriodDays != 30 ||
			got.GraceDays != 3 || got.AllowWalletOverflow || got.PricePaidAmount != 19900 {
			t.Fatalf("快照列未更新: %+v", got)
		}
		if string(got.ModelScope) != `["gpt-4o"]` ||
			got.ConcurrencyLimit == nil || *got.ConcurrencyLimit != 2 {
			t.Fatalf("scope/limit = %s / %v", got.ModelScope, got.ConcurrencyLimit)
		}
	})

	t.Run("expired 从 now 起算并恢复 active", func(t *testing.T) {
		oldEnd := now.Add(-15 * 24 * time.Hour)
		sub := createTestSubscription(t, repo, plan, 900003, oldEnd.Add(-30*24*time.Hour), oldEnd, nil)
		if _, err := db.Exec(`UPDATE subscriptions
			SET status = 'expired', amount_used = 99 WHERE id = $1`, sub.ID); err != nil {
			t.Fatalf("seed expired: %v", err)
		}
		got, err := RenewSubscription(ctx, db, RenewalExtendInput{
			SubscriptionID: sub.ID, UserID: 900003, PlanID: plan.ID,
			PlanTitleSnapshot: plan.Title, PeriodDays: 30, GrantCredits: 1000,
			ResetOnRenew: false, AllowWalletOverflow: true, GraceDays: 0,
			Currency: "CNY", PricePaidAmount: 9900, Now: now, ExpectedEndAt: oldEnd,
		})
		if err != nil {
			t.Fatalf("renew expired: %v", err)
		}
		if got.Status != "active" || got.AmountUsed != 0 {
			t.Fatalf("expired 续费后 = status:%s used:%d", got.Status, got.AmountUsed)
		}
		if !got.EndAt.UTC().Equal(now.Add(30 * 24 * time.Hour)) {
			t.Fatalf("expired end_at = %s，期望 now+30d %s", got.EndAt, now.Add(30*24*time.Hour))
		}
		if got.NextResetAt != nil {
			t.Fatalf("reset_on_renew=false 时 next_reset_at 必须为 NULL，得到 %v", got.NextResetAt)
		}
	})

	t.Run("cancelled 与归属不符", func(t *testing.T) {
		sub := createTestSubscription(t, repo, plan, 900003, now.Add(-time.Hour), now.Add(24*time.Hour), nil)
		if _, err := db.Exec(`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, sub.ID); err != nil {
			t.Fatalf("seed cancelled: %v", err)
		}
		in := RenewalExtendInput{
			SubscriptionID: sub.ID, UserID: 900003, PlanID: plan.ID,
			PlanTitleSnapshot: plan.Title, PeriodDays: 30, GrantCredits: 1000,
			ResetOnRenew: true, AllowWalletOverflow: true, Currency: "CNY",
			PricePaidAmount: 9900, Now: now, ExpectedEndAt: now.Add(24 * time.Hour),
		}
		if _, err := RenewSubscription(ctx, db, in); !errors.Is(err, ErrSubscriptionNotActive) {
			t.Fatalf("cancelled err = %v，期望 ErrSubscriptionNotActive", err)
		}
		in.UserID = 900004
		if _, err := RenewSubscription(ctx, db, in); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("他人实例 err = %v，期望 ErrSubscriptionNotFound", err)
		}
		in.UserID = 900003
		in.PlanID = plan.ID + 999999
		if _, err := RenewSubscription(ctx, db, in); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("套餐不符 err = %v，期望 ErrSubscriptionNotFound", err)
		}
	})
}

// TestSubscriptionCancelAtPeriodEndBoundary 覆盖 M2 / G-DOM-30 ②：周期末取消
// 的实例不享宽限。宽限期内（end_at < now < end_at+grace）未取消实例仍可
// 消费；一旦置 cancel_at_period_end，三条读侧契约与预扣写入都必须拒绝。
func TestSubscriptionCancelAtPeriodEndBoundary(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	now := time.Now().UTC()

	graceDays := int32(7)
	plan, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey:      subscriptionPlanKey("d5a-m2"),
		Title:        "M2 Boundary",
		PeriodDays:   30,
		PriceAmount:  100,
		Currency:     "CNY",
		GrantCredits: 1000,
		GraceDays:    graceDays,
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

	userID := int64(910001)
	// end_at 已过 1 小时，但仍在 7 天宽限内。
	sub := createTestSubscription(t, repo, plan, userID,
		now.Add(-48*time.Hour), now.Add(-time.Hour), nil)

	// 未取消：宽限期内的实例仍是活跃订阅。
	actives, err := repo.ListActiveSubscriptions(ctx, userID, now)
	if err != nil || len(actives) != 1 || actives[0].ID != sub.ID {
		t.Fatalf("宽限期内 ListActiveSubscriptions = %+v/%v，want 实例 %d", actives, err, sub.ID)
	}
	if ok, err := repo.HasActiveUserSubscription(ctx, userID, now); err != nil || !ok {
		t.Fatalf("宽限期内 HasActiveUserSubscription = %v/%v，want true", ok, err)
	}
	if ok, err := repo.UserActiveSubscriptionsAllowWalletOverflow(ctx, userID, now); err != nil || !ok {
		t.Fatalf("宽限期内 AllowWalletOverflow = %v/%v，want true", ok, err)
	}

	// 置周期末取消：end_at 已过 → 不享宽限。
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); err != nil {
		t.Fatalf("CancelAtPeriodEnd: %v", err)
	}
	if actives, err := repo.ListActiveSubscriptions(ctx, userID, now); err != nil || len(actives) != 0 {
		t.Fatalf("取消后 ListActiveSubscriptions = %+v/%v，want 空", actives, err)
	}
	if ok, err := repo.HasActiveUserSubscription(ctx, userID, now); err != nil || ok {
		t.Fatalf("取消后 HasActiveUserSubscription = %v/%v，want false", ok, err)
	}
	if ok, err := repo.UserActiveSubscriptionsAllowWalletOverflow(ctx, userID, now); err != nil || ok {
		t.Fatalf("取消后 AllowWalletOverflow = %v/%v，want false", ok, err)
	}

	// 预扣写入同样按「不享宽限」拒绝：条件 UPDATE 命中 0 行 → 额度不足，
	// 且失败事务不留下幂等记录或 amount_used 增量。
	req := subscriptionRequestID(t)
	if _, _, err := repo.PreConsumeUserSubscription(ctx, req, sub.ID, 100); !errors.Is(err, ErrSubscriptionQuotaInsufficient) {
		t.Fatalf("取消实例预扣 err = %v，want ErrSubscriptionQuotaInsufficient", err)
	}
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	if used != 0 {
		t.Fatalf("amount_used = %d，want 0", used)
	}
	if _, err := repo.GetPreConsume(ctx, req); !errors.Is(err, ErrPreConsumeNotFound) {
		t.Fatalf("失败预扣留下记录: %v", err)
	}
}

// TestSubscriptionCancelImmediatelyGrace 覆盖 C3 / R-48：管理侧立即取消把
// end_at 提前到「现在」，实例仍是 active 并在快照宽限期内可消费；宽限期
// 结束后 ExpireDue 迁移为 expired，额度随之不可用。宽限期内不发放新周期
// 额度（H3 与立即取消正交：DueForReset 只认 end_at > now）。
func TestSubscriptionCancelImmediatelyGrace(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	now := time.Now().UTC()

	plan, err := repo.CreatePlan(ctx, SubscriptionPlanInput{
		PlanKey:      subscriptionPlanKey("c3-immediate"),
		Title:        "Immediate Grace",
		PeriodDays:   30,
		PriceAmount:  100,
		Currency:     "CNY",
		GrantCredits: 1000,
		GraceDays:    3,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id IN
			(SELECT id FROM subscriptions WHERE plan_id = $1)`, plan.ID)
		_, _ = db.Exec(`DELETE FROM subscription_events WHERE subscription_id IN
			(SELECT id FROM subscriptions WHERE plan_id = $1)`, plan.ID)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE plan_id = $1`, plan.ID)
		_, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, plan.ID)
	})

	userID := int64(920001)
	// 原合同还剩 5 天；立即取消后宽限期从「现在」起算 3 天。
	sub := createTestSubscription(t, repo, plan, userID,
		now.Add(-time.Hour), now.Add(5*24*time.Hour), nil)
	// 打开自动续费并挂一个令牌（逻辑引用，无 FK）：这正是 C3 复核的
	// Critical——若立即取消不关 auto_renew，S4 worker 会在下一 tick 命中
	// 这条「已被管理员取消」的订阅并继续向用户扣款。
	if _, err := db.Exec(`UPDATE subscriptions
		SET grace_days = 3, auto_renew = true,
		    auto_renew_payment_method_id = gen_random_uuid()
		WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("set grace/auto-renew: %v", err)
	}

	// 取消前：该实例确实在续费窗口内（否则下面的「取消后不在候选中」
	// 无法证明是取消造成的）。
	dueBefore, err := repo.ListDueForAutoRenew(ctx, now, 6*24*time.Hour, 6*time.Hour, 50)
	if err != nil {
		t.Fatalf("ListDueForAutoRenew before cancel: %v", err)
	}
	if !containsSubscription(dueBefore, sub.ID) {
		t.Fatalf("取消前应进入续费候选，实际 %+v", dueBefore)
	}

	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}

	got, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if got.Status != "active" || got.CancelAtPeriodEnd {
		t.Fatalf("立即取消后 = status %q / cancel_at_period_end %v，want active/false",
			got.Status, got.CancelAtPeriodEnd)
	}
	if got.AutoRenew {
		t.Fatal("立即取消后 auto_renew 仍为 true：S4 worker 会对已取消订阅继续扣款")
	}
	if got.EndAt.After(time.Now().UTC()) {
		t.Fatalf("立即取消后 end_at = %v，期望提前到 now", got.EndAt)
	}

	// 取消后：不再进入续费候选（不会产生续费订单/重置额度）。
	dueAfter, err := repo.ListDueForAutoRenew(ctx, now, 6*24*time.Hour, 6*time.Hour, 50)
	if err != nil {
		t.Fatalf("ListDueForAutoRenew after cancel: %v", err)
	}
	if containsSubscription(dueAfter, sub.ID) {
		t.Fatalf("立即取消后仍进入续费候选：%+v", dueAfter)
	}

	// 宽限期内可消费：读侧列表与预扣写入都仍接受该实例。
	actives, err := repo.ListActiveSubscriptions(ctx, userID, now)
	if err != nil || len(actives) != 1 || actives[0].ID != sub.ID {
		t.Fatalf("宽限期内 ListActiveSubscriptions = %+v/%v，want 实例 %d", actives, err, sub.ID)
	}
	req := subscriptionRequestID(t)
	if _, created, err := repo.PreConsumeUserSubscription(ctx, req, sub.ID, 100); err != nil || !created {
		t.Fatalf("宽限期内预扣 = created %v / err %v，want 成功", created, err)
	}

	// 事件：created + cancelled(immediate=true)，重复调用不追加。
	events, err := repo.ListSubscriptionEvents(ctx, sub.ID, 0, 100)
	if err != nil || len(events) != 2 || events[1].Kind != SubscriptionEventCancelled {
		t.Fatalf("立即取消事件 = %+v/%v", events, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(events[1].Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["immediate"] != true {
		t.Fatalf("detail.immediate = %v，want true", detail["immediate"])
	}
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("重复 cancel immediately: %v", err)
	}
	if events, err = repo.ListSubscriptionEvents(ctx, sub.ID, 0, 100); err != nil || len(events) != 2 {
		t.Fatalf("重复取消后事件数 = %d/%v，want 2（幂等不重写）", len(events), err)
	}

	// 幂等判定把 auto_renew 纳入（R-54 的配套）：用户在宽限期里重新打开
	// 自动续费后再取消，必须再次关掉它并追加事件——否则下一 tick 就被
	// worker 扣款。
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew = true WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("re-enable auto-renew: %v", err)
	}
	if err := repo.CancelImmediately(ctx, sub.ID); err != nil {
		t.Fatalf("重新开启后再次取消: %v", err)
	}
	if got, err = repo.GetSubscription(ctx, sub.ID); err != nil || got.AutoRenew {
		t.Fatalf("再次取消后 auto_renew = %v/%v，want false", got.AutoRenew, err)
	}
	if events, err = repo.ListSubscriptionEvents(ctx, sub.ID, 0, 100); err != nil || len(events) != 3 ||
		events[2].Kind != SubscriptionEventCancelled {
		t.Fatalf("再次取消后事件 = %+v/%v，want 追加一条 cancelled", events, err)
	}

	// 宽限期结束后：ExpireDue 迁移为 expired，额度不再可用。
	afterGrace := now.Add(4 * 24 * time.Hour)
	if n, err := repo.ExpireDue(ctx, afterGrace); err != nil || n < 1 {
		t.Fatalf("ExpireDue = %d/%v，want ≥1", n, err)
	}
	req2 := subscriptionRequestID(t)
	if _, _, err := repo.PreConsumeUserSubscription(ctx, req2, sub.ID, 1); !errors.Is(err, ErrSubscriptionQuotaInsufficient) {
		t.Fatalf("宽限结束后预扣 err = %v，want ErrSubscriptionQuotaInsufficient", err)
	}
}

// TestSubscriptionRefundRaceWithResetIsNoop 覆盖 M3：周期重置已把
// amount_used 归零（或归到小于预扣额）后，退款回退必须变成 no-op，不能把
// 新周期的已用量扣成负数/少算。
func TestSubscriptionRefundRaceWithResetIsNoop(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "d5a-m3")
	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 910002, now.Add(-time.Hour), now.Add(time.Hour), nil)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
	})

	requestID := subscriptionRequestID(t)
	if _, created, err := repo.PreConsumeUserSubscription(ctx, requestID, sub.ID, 120); err != nil || !created {
		t.Fatalf("PreConsumeUserSubscription = %v/%v，want created", created, err)
	}

	// 模拟竞态：B12 重置把 amount_used 归零。
	if _, err := db.Exec(`UPDATE subscriptions SET amount_used = 0 WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("simulate reset: %v", err)
	}
	if err := repo.MarkPreConsumeRefunded(ctx, requestID); err != nil {
		t.Fatalf("MarkPreConsumeRefunded: %v", err)
	}
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	if used != 0 {
		t.Fatalf("归零后退款 amount_used = %d，want 0（no-op）", used)
	}
	rec, err := repo.GetPreConsume(ctx, requestID)
	if err != nil || rec.Status != "refunded" {
		t.Fatalf("退款记录 = %+v/%v，want refunded", rec, err)
	}

	// 第二个请求：重置后新周期已有 50 已用，退款不得从新周期扣走。
	req2 := subscriptionRequestID(t)
	if _, created, err := repo.PreConsumeUserSubscription(ctx, req2, sub.ID, 120); err != nil || !created {
		t.Fatalf("second PreConsume = %v/%v", created, err)
	}
	if _, err := db.Exec(`UPDATE subscriptions SET amount_used = 50 WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("simulate partial reset: %v", err)
	}
	if err := repo.MarkPreConsumeRefunded(ctx, req2); err != nil {
		t.Fatalf("second MarkPreConsumeRefunded: %v", err)
	}
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used after second refund: %v", err)
	}
	if used != 50 {
		t.Fatalf("部分竞态后退款 amount_used = %d，want 50（no-op）", used)
	}
}

// TestSubscriptionPostConsumeDelta 覆盖 S4b 结算增量的数据层语义
// （billing-04 §3.5 / billing-08 §5.1）：正追加、负回退、下界 0、上界拒绝、
// amount_total=0 无限额度、delta=0 返回快照但不改写。
func TestSubscriptionPostConsumeDelta(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4b-delta")
	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 900101, now.Add(-time.Hour), now.Add(time.Hour), nil)

	got, err := repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, 120)
	if err != nil {
		t.Fatalf("delta +120: %v", err)
	}
	if got.AmountUsed != 120 || got.AmountTotal != 1000 || got.PlanID != plan.ID ||
		got.PlanTitleSnapshot != plan.Title {
		t.Fatalf("delta +120 返回快照 = %+v，字段不符", got)
	}

	got, err = repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, -40)
	if err != nil || got.AmountUsed != 80 {
		t.Fatalf("delta -40 = %d/%v，want 80/nil", got.AmountUsed, err)
	}

	// 下界 0：负增量不得把已用推到负数。
	got, err = repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, -1000)
	if err != nil || got.AmountUsed != 0 {
		t.Fatalf("delta -1000 = %d/%v，want 0/nil", got.AmountUsed, err)
	}

	// delta=0：返回快照、不加更新。
	got, err = repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, 0)
	if err != nil || got.AmountUsed != 0 {
		t.Fatalf("delta 0 = %d/%v，want 0/nil", got.AmountUsed, err)
	}

	// 上界拒绝：结果 1001 > amount_total 1000，事务不落地。
	if _, err := repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, 1001); !errors.Is(err, ErrSubscriptionSettleExceeded) {
		t.Fatalf("越上界 err = %v，want ErrSubscriptionSettleExceeded", err)
	}
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	if used != 0 {
		t.Fatalf("越界拒绝后 amount_used = %d，want 0（不做部分结算）", used)
	}

	// 恰好到上界允许。
	if got, err = repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, 1000); err != nil || got.AmountUsed != 1000 {
		t.Fatalf("delta 到上界 = %d/%v，want 1000/nil", got.AmountUsed, err)
	}

	if _, err := repo.PostConsumeUserSubscriptionDelta(ctx, 1<<60, 1); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("不存在实例 err = %v，want ErrSubscriptionNotFound", err)
	}

	// amount_total=0 = 无限额度：只守下界，不设上界（R-D5-07）。
	unlimited := SubscriptionCreateInput{
		UserID: 900102, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		AmountTotal: 0, Currency: plan.Currency, PricePaidAmount: 0,
		PeriodDays: plan.PeriodDays, ResetOnRenew: true,
	}
	inf, err := repo.CreateSubscription(ctx, unlimited)
	if err != nil {
		t.Fatalf("create unlimited: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, inf.ID) })
	if got, err = repo.PostConsumeUserSubscriptionDelta(ctx, inf.ID, 1<<40); err != nil || got.AmountUsed != 1<<40 {
		t.Fatalf("无限额度 delta = %d/%v，want 1<<40/nil", got.AmountUsed, err)
	}
}

// TestSubscriptionRefundIncrementGuard 覆盖 M3 的增量回退守卫：
// amount_used >= amount 才回退；周期重置后旧周期增量回退不得侵蚀新周期用量。
func TestSubscriptionRefundIncrementGuard(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4b-inc-refund")
	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 900104, now.Add(-time.Hour), now.Add(time.Hour), nil)

	if _, err := repo.PostConsumeUserSubscriptionDelta(ctx, sub.ID, 200); err != nil {
		t.Fatalf("seed amount_used: %v", err)
	}
	applied, err := repo.RefundSubscriptionIncrement(ctx, sub.ID, 30)
	if err != nil || !applied {
		t.Fatalf("RefundSubscriptionIncrement = %v/%v，want true/nil", applied, err)
	}
	if got := subscriptionAmountUsed(t, db, sub.ID); got != 170 {
		t.Fatalf("amount_used = %d，want 170", got)
	}

	// 模拟周期重置（归零）后新周期已用 20：旧增量的 30 不得再回退。
	if _, err := db.Exec(`UPDATE subscriptions SET amount_used = 20 WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("simulate reset: %v", err)
	}
	applied, err = repo.RefundSubscriptionIncrement(ctx, sub.ID, 30)
	if err != nil || applied {
		t.Fatalf("守卫未命中时 = %v/%v，want false/nil", applied, err)
	}
	if got := subscriptionAmountUsed(t, db, sub.ID); got != 20 {
		t.Fatalf("amount_used = %d，want 20（不得侵蚀新周期用量）", got)
	}

	if _, err := repo.RefundSubscriptionIncrement(ctx, sub.ID, 0); !errors.Is(err, ErrSubscriptionInvalid) {
		t.Fatalf("amount=0 err = %v，want ErrSubscriptionInvalid", err)
	}
}

// subscriptionAmountUsed 读取实例已用额度（断言用）。
func subscriptionAmountUsed(t *testing.T, db *sql.DB, id int64) int64 {
	t.Helper()
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, id).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	return used
}

// TestSubscriptionListStalePreConsumes 覆盖崩溃兜底扫描的候选集
// （billing-06 §5.2；R-W9B-02 的在飞租约谓词）：旧 consumed 且租约缺失/
// 过期 → 命中；refunded、未到窗口、registry 已 settled/refunded、以及
// in_flight 且心跳新鲜（仍在飞）的记录被排除；failed 不排除。
func TestSubscriptionListStalePreConsumes(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4b-stale")
	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 900103, now.Add(-time.Hour), now.Add(time.Hour), nil)

	insert := func(pre int64) string {
		req := subscriptionRequestID(t)
		if _, created, err := repo.PreConsumeUserSubscription(ctx, req, sub.ID, pre); err != nil || !created {
			t.Fatalf("preconsume %s = %v/%v", req, created, err)
		}
		return req
	}
	backdate := func(req string, at time.Time) {
		if _, err := db.Exec(`UPDATE subscription_pre_consume_records SET created_at = $2 WHERE request_id = $1`, req, at); err != nil {
			t.Fatalf("backdate %s: %v", req, err)
		}
	}
	setRegistry := func(req, state string) {
		// PreConsumeUserSubscription 现在会与预扣同事务写 in_flight 租约
		// （R-W9B-02），因此这里必须 UPSERT 而不是 INSERT。
		if _, err := db.Exec(`
			INSERT INTO request_registry (request_id, user_id, state, heartbeat_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (request_id) DO UPDATE
			   SET state = EXCLUDED.state, heartbeat_at = now()`,
			req, 900103, state); err != nil {
			t.Fatalf("insert request_registry %s: %v", req, err)
		}
	}
	backdateLease := func(req string, at time.Time) {
		if _, err := db.Exec(`UPDATE request_registry SET heartbeat_at = $2 WHERE request_id = $1`, req, at); err != nil {
			t.Fatalf("backdate lease %s: %v", req, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_registry WHERE user_id = 900103`)
	})

	oldConsumed := insert(50)
	backdate(oldConsumed, now.Add(-20*time.Minute))
	// 预扣事务写入的租约心跳也是 now；把过期请求的租约一起回拨，模拟
	// 「进程已崩溃」。
	backdateLease(oldConsumed, now.Add(-20*time.Minute))

	refunded := insert(40)
	backdate(refunded, now.Add(-20*time.Minute))
	if err := repo.MarkPreConsumeRefunded(ctx, refunded); err != nil {
		t.Fatalf("refund: %v", err)
	}

	fresh := insert(30) // created_at = now，未到窗口
	_ = fresh

	settled := insert(20)
	backdate(settled, now.Add(-20*time.Minute))
	setRegistry(settled, "settled")

	registryRefunded := insert(20)
	backdate(registryRefunded, now.Add(-20*time.Minute))
	setRegistry(registryRefunded, "refunded")

	failedState := insert(10)
	backdate(failedState, now.Add(-20*time.Minute))
	setRegistry(failedState, "failed")

	// liveLease：预扣很老，但 in_flight 心跳新鲜 —— 长任务/长流仍在跑，
	// 扫描不得把它当崩溃（G-DOM-35 的核心回归）。
	liveLease := insert(15)
	backdate(liveLease, now.Add(-20*time.Minute))

	leaseCutoff := now.Add(-time.Minute) // TTL=60s 的等价过期界
	list, err := repo.ListStalePreConsumes(ctx, now.Add(-15*time.Minute), leaseCutoff, 100)
	if err != nil {
		t.Fatalf("ListStalePreConsumes: %v", err)
	}
	got := map[string]bool{}
	for _, rec := range list {
		got[rec.RequestID] = true
	}
	if len(got) != 2 || !got[oldConsumed] || !got[failedState] {
		t.Fatalf("stale 候选 = %v，want 仅 %s 与 %s", got, oldConsumed, failedState)
	}
	for _, req := range []string{refunded, settled, registryRefunded, fresh, liveLease} {
		if got[req] {
			t.Fatalf("不应纳入 %s（已退款/已结案/未到窗口/仍在飞）", req)
		}
	}

	// 心跳超过租约过期界后，同一条记录重新变成候选（崩溃才会被回收）。
	backdateLease(liveLease, now.Add(-2*time.Minute))
	list, err = repo.ListStalePreConsumes(ctx, now.Add(-15*time.Minute), leaseCutoff, 100)
	if err != nil {
		t.Fatalf("ListStalePreConsumes after lease expiry: %v", err)
	}
	got = map[string]bool{}
	for _, rec := range list {
		got[rec.RequestID] = true
	}
	if !got[liveLease] {
		t.Fatalf("租约过期后 %s 应重新纳入候选", liveLease)
	}

	// LIMIT 生效（有界扫描）。
	limited, err := repo.ListStalePreConsumes(ctx, now.Add(-15*time.Minute), leaseCutoff, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit=1 返回 %d 条/%v，want 1/nil", len(limited), err)
	}
}
