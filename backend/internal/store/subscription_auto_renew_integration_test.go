//go:build integration

package store

// S4 自动续费的 SQL 不变量（R-40 / P-16 / R-42 / R-43）。
//
// 纯 fake 单测会自带阈值/窗口逻辑，无法发现 store 里的真实 SQL 被改坏
// （例如把 `disabled := failures >= threshold` 改成 false、去掉 end_at
// 窗口条件）。这里全部走真实 PostgreSQL：
//   - 窗口过滤与失败退避（ListDueForAutoRenew）；
//   - 连续失败阈值 → auto_renew=false + 事件（RecordAutoRenewFailure）；
//   - 并发失败计数必须由行锁串行化（FOR UPDATE）；
//   - 用户侧偏好写入的归属/令牌校验（SetSubscriptionAutoRenew）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_s4b?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestAutoRenewStore -v

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func createAutoRenewTestMethod(t *testing.T, db *sql.DB, userID int64) PaymentMethod {
	t.Helper()
	repo := NewPaymentMethodRepo(db)
	method, err := repo.Create(context.Background(), PaymentMethodInput{
		UserID: userID, Provider: "stripe",
		TokenCipher: "sealed:pm_store_test_padding", Brand: "visa",
		Last4: "4242", ExpMonth: 12, ExpYear: 2030,
	})
	if err != nil {
		t.Fatalf("create payment method: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM payment_methods WHERE id = $1`, method.ID) })
	return method
}

// TestAutoRenewStoreWindowAndBackoff 覆盖窗口过滤与重试退避。
func TestAutoRenewStoreWindowAndBackoff(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4-window")
	now := time.Now().UTC().Truncate(time.Second)
	method := createAutoRenewTestMethod(t, db, 900501)

	makeSub := func(label string, endAt time.Time) Subscription {
		sub := createTestSubscription(t, repo, plan, 900501,
			now.Add(-29*24*time.Hour), endAt, nil)
		enabled, err := repo.SetSubscriptionAutoRenew(ctx, 900501, sub.ID, true, &method.ID)
		if err != nil {
			t.Fatalf("%s: enable auto-renew: %v", label, err)
		}
		return enabled
	}

	inside := makeSub("inside", now.Add(time.Hour))
	outside := makeSub("outside", now.Add(30*time.Hour))
	cancelled := makeSub("cancel", now.Add(time.Hour))
	// cancel_at_period_end=true 的实例必须被窗口查询排除（R-40）。
	if err := repo.CancelAtPeriodEnd(ctx, cancelled.ID); err != nil {
		t.Fatalf("cancel at period end: %v", err)
	}
	disabled := makeSub("disabled", now.Add(time.Hour))
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew = false WHERE id = $1`, disabled.ID); err != nil {
		t.Fatalf("disable auto-renew: %v", err)
	}
	noMethod := makeSub("no-method", now.Add(time.Hour))
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew_payment_method_id = NULL WHERE id = $1`, noMethod.ID); err != nil {
		t.Fatalf("clear payment method: %v", err)
	}

	due, err := repo.ListDueForAutoRenew(ctx, now, 24*time.Hour, 6*time.Hour, 50)
	if err != nil {
		t.Fatalf("ListDueForAutoRenew: %v", err)
	}
	ids := map[int64]bool{}
	for _, sub := range due {
		ids[sub.ID] = true
	}
	if !ids[inside.ID] {
		t.Fatal("subscription inside the 24h window must be due")
	}
	for label, id := range map[string]int64{
		"outside window": outside.ID, "cancel_at_period_end": cancelled.ID,
		"auto_renew=false": disabled.ID, "no payment method": noMethod.ID,
	} {
		if ids[id] {
			t.Fatalf("%s subscription must not be due", label)
		}
	}

	// 退避：1 小时前尝试过（retry=6h）→ 不候选；7 小时前 → 再次候选。
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew_attempted_at = $2
		WHERE id = $1`, inside.ID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed recent attempt: %v", err)
	}
	due, err = repo.ListDueForAutoRenew(ctx, now, 24*time.Hour, 6*time.Hour, 50)
	if err != nil {
		t.Fatalf("ListDueForAutoRenew (backoff): %v", err)
	}
	for _, sub := range due {
		if sub.ID == inside.ID {
			t.Fatal("subscription with a recent attempt must respect the retry backoff")
		}
	}
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew_attempted_at = $2
		WHERE id = $1`, inside.ID, now.Add(-7*time.Hour)); err != nil {
		t.Fatalf("seed old attempt: %v", err)
	}
	due, err = repo.ListDueForAutoRenew(ctx, now, 24*time.Hour, 6*time.Hour, 50)
	if err != nil {
		t.Fatalf("ListDueForAutoRenew (after backoff): %v", err)
	}
	found := false
	for _, sub := range due {
		if sub.ID == inside.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("subscription must be due again after the retry backoff")
	}
}

// TestAutoRenewStoreFailureThreshold 覆盖 P-16：连续 3 次失败关闭开关并逐次
// 写 renewal_failed 事件。变异把阈值比较改成恒 false 时，本用例必须红。
func TestAutoRenewStoreFailureThreshold(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4-threshold")
	now := time.Now().UTC().Truncate(time.Second)
	method := createAutoRenewTestMethod(t, db, 900502)
	sub := createTestSubscription(t, repo, plan, 900502,
		now.Add(-29*24*time.Hour), now.Add(time.Hour), nil)
	if _, err := repo.SetSubscriptionAutoRenew(ctx, 900502, sub.ID, true, &method.ID); err != nil {
		t.Fatalf("enable auto-renew: %v", err)
	}

	for i := int32(1); i <= 3; i++ {
		state, err := repo.RecordAutoRenewFailure(ctx, sub.ID, now,
			AutoRenewFailureThreshold, map[string]any{"reason": "card_declined"})
		if err != nil {
			t.Fatalf("RecordAutoRenewFailure #%d: %v", i, err)
		}
		if state.Failures != i {
			t.Fatalf("failures = %d, want %d", state.Failures, i)
		}
		if state.Disabled != (i == 3) {
			t.Fatalf("attempt %d disabled = %v, want %v", i, state.Disabled, i == 3)
		}
	}

	current, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if current.AutoRenew {
		t.Fatal("auto_renew must be false after the third consecutive failure (P-16)")
	}
	if current.AutoRenewFailures != 3 {
		t.Fatalf("auto_renew_failures = %d, want 3", current.AutoRenewFailures)
	}

	var events int
	if err := db.QueryRow(`SELECT count(*) FROM subscription_events
		WHERE subscription_id = $1 AND kind = 'renewal_failed'`, sub.ID).Scan(&events); err != nil {
		t.Fatalf("count renewal_failed events: %v", err)
	}
	if events != 3 {
		t.Fatalf("renewal_failed events = %d, want 3", events)
	}
	var disabledFlag bool
	if err := db.QueryRow(`SELECT (detail->>'auto_renew_disabled')::boolean
		FROM subscription_events WHERE subscription_id = $1 AND kind = 'renewal_failed'
		ORDER BY seq DESC LIMIT 1`, sub.ID).Scan(&disabledFlag); err != nil {
		t.Fatalf("read third event detail: %v", err)
	}
	if !disabledFlag {
		t.Fatal("the third renewal_failed event must record auto_renew_disabled=true")
	}
}

// TestAutoRenewStoreConcurrentFailuresSerialize 证明失败计数在订阅行锁内
// 串行化：两个并发失败必须得到 2 次计数与 2 条事件，而不是都从 0 读到 1。
func TestAutoRenewStoreConcurrentFailuresSerialize(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4-concurrent-fail")
	now := time.Now().UTC().Truncate(time.Second)
	method := createAutoRenewTestMethod(t, db, 900503)
	sub := createTestSubscription(t, repo, plan, 900503,
		now.Add(-29*24*time.Hour), now.Add(time.Hour), nil)
	if _, err := repo.SetSubscriptionAutoRenew(ctx, 900503, sub.ID, true, &method.ID); err != nil {
		t.Fatalf("enable auto-renew: %v", err)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := repo.RecordAutoRenewFailure(ctx, sub.ID, now,
				AutoRenewFailureThreshold, map[string]any{"reason": "card_declined"}); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("concurrent failures: %v", errs[0])
	}
	current, err := repo.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if current.AutoRenewFailures != 2 {
		t.Fatalf("failures = %d, want 2 (row lock must serialize the increments)",
			current.AutoRenewFailures)
	}
	var events int
	if err := db.QueryRow(`SELECT count(*) FROM subscription_events
		WHERE subscription_id = $1 AND kind = 'renewal_failed'`, sub.ID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 2 {
		t.Fatalf("renewal_failed events = %d, want 2", events)
	}
}

// TestAutoRenewStorePreferenceValidation 覆盖归属/终态/令牌有效性与清零语义。
func TestAutoRenewStorePreferenceValidation(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4-pref")
	now := time.Now().UTC().Truncate(time.Second)
	method := createAutoRenewTestMethod(t, db, 900504)
	foreignMethod := createAutoRenewTestMethod(t, db, 900505)

	sub := createTestSubscription(t, repo, plan, 900504,
		now.Add(-29*24*time.Hour), now.Add(time.Hour), nil)
	if _, err := repo.SetSubscriptionAutoRenew(ctx, 900599, sub.ID, true, &method.ID); err == nil {
		t.Fatal("foreign user must not be able to change auto-renew")
	}
	if _, err := repo.SetSubscriptionAutoRenew(ctx, 900504, sub.ID, true, nil); err == nil {
		t.Fatal("enabling without a payment method must fail")
	}
	if _, err := repo.SetSubscriptionAutoRenew(ctx, 900504, sub.ID, true, &foreignMethod.ID); err == nil {
		t.Fatal("enabling with another user's payment method must fail")
	}
	enabled, err := repo.SetSubscriptionAutoRenew(ctx, 900504, sub.ID, true, &method.ID)
	if err != nil {
		t.Fatalf("enable auto-renew: %v", err)
	}
	if !enabled.AutoRenew || enabled.AutoRenewPaymentMethodID == nil ||
		*enabled.AutoRenewPaymentMethodID != method.ID {
		t.Fatalf("enabled subscription = %+v", enabled)
	}

	// 显式关闭清零失败计数与尝试时刻。
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew_failures = 2,
		auto_renew_attempted_at = now() WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("seed failures: %v", err)
	}
	disabled, err := repo.SetSubscriptionAutoRenew(ctx, 900504, sub.ID, false, nil)
	if err != nil {
		t.Fatalf("disable auto-renew: %v", err)
	}
	if disabled.AutoRenew || disabled.AutoRenewFailures != 0 || disabled.AutoRenewAttemptedAt != nil {
		t.Fatalf("disabled subscription = %+v, want cleared state", disabled)
	}

	cancelled := createTestSubscription(t, repo, plan, 900504,
		now.Add(-29*24*time.Hour), now.Add(time.Hour), nil)
	if _, err := db.Exec(`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, cancelled.ID); err != nil {
		t.Fatalf("cancel subscription: %v", err)
	}
	if _, err := repo.SetSubscriptionAutoRenew(ctx, 900504, cancelled.ID, true, &method.ID); err == nil {
		t.Fatal("cancelled subscriptions must not enable auto-renew")
	}
}

// TestSetSubscriptionAutoRenewRejectsEnded 覆盖 R-54：立即取消后实例仍是
// active 但 end_at <= now，开启自动续费必须被拒绝（ErrSubscriptionEnded），
// 否则用户自己就能在宽限期里把管理员的立即取消撤销掉（下一 tick 扣款）。
// 未结束实例仍可正常开启；关闭操作对已结束实例仍允许（只减不增风险）。
func TestSetSubscriptionAutoRenewRejectsEnded(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4-r54")
	now := time.Now().UTC().Truncate(time.Second)
	const userID = int64(900510)
	method := createAutoRenewTestMethod(t, db, userID)

	// end_at 已过但仍在 grace 窗口内（与立即取消后的形态一致）。
	ended := createTestSubscription(t, repo, plan, userID,
		now.Add(-48*time.Hour), now.Add(-time.Hour), nil)
	if _, err := db.Exec(`UPDATE subscriptions SET grace_days = 7 WHERE id = $1`, ended.ID); err != nil {
		t.Fatalf("set grace: %v", err)
	}
	if _, err := repo.SetSubscriptionAutoRenew(ctx, userID, ended.ID, true, &method.ID); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("已结束实例开启自动续费 err = %v，want ErrSubscriptionEnded", err)
	}
	got, err := repo.GetSubscription(ctx, ended.ID)
	if err != nil || got.AutoRenew {
		t.Fatalf("被拒绝后 auto_renew = %v/%v，want false", got.AutoRenew, err)
	}
	// 关闭允许（幂等、不产生扣款风险）。
	if _, err := repo.SetSubscriptionAutoRenew(ctx, userID, ended.ID, false, nil); err != nil {
		t.Fatalf("已结束实例关闭自动续费应允许：%v", err)
	}

	// 未结束实例不受影响。
	active := createTestSubscription(t, repo, plan, userID,
		now.Add(-time.Hour), now.Add(5*24*time.Hour), nil)
	enabled, err := repo.SetSubscriptionAutoRenew(ctx, userID, active.ID, true, &method.ID)
	if err != nil || !enabled.AutoRenew {
		t.Fatalf("未结束实例开启自动续费 = %+v/%v，want 成功", enabled, err)
	}
}

// TestRenewSubscriptionRejectsStaleExpectedEndAt 覆盖 R-43 的 store 级周期锚：
// 下单快照的 end_at 与当前值不一致时返回 ErrSubscriptionCycleChanged，且
// 不推进 end_at；匹配时才推进一次。
func TestRenewSubscriptionRejectsStaleExpectedEndAt(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s4-cycle-anchor")
	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(10 * 24 * time.Hour)
	sub := createTestSubscription(t, repo, plan, 900506,
		now.Add(-20*24*time.Hour), oldEnd, nil)

	input := RenewalExtendInput{
		SubscriptionID: sub.ID, UserID: 900506, PlanID: plan.ID,
		PlanTitleSnapshot: plan.Title, PeriodDays: 30, GrantCredits: 1000,
		ResetOnRenew: true, AllowWalletOverflow: true, Currency: "CNY",
		PricePaidAmount: 9900, Now: now,
		ExpectedEndAt: oldEnd.Add(time.Second), // 陈旧快照（另一笔已续费）
	}
	if _, err := RenewSubscription(ctx, db, input); !errors.Is(err, ErrSubscriptionCycleChanged) {
		t.Fatalf("stale anchor err = %v, want ErrSubscriptionCycleChanged", err)
	}
	var currentEnd time.Time
	if err := db.QueryRow(`SELECT end_at FROM subscriptions WHERE id = $1`, sub.ID).
		Scan(&currentEnd); err != nil {
		t.Fatalf("read end_at: %v", err)
	}
	if !currentEnd.UTC().Equal(oldEnd) {
		t.Fatalf("end_at = %s, want unchanged %s", currentEnd, oldEnd)
	}

	input.ExpectedEndAt = oldEnd
	got, err := RenewSubscription(ctx, db, input)
	if err != nil {
		t.Fatalf("matching anchor renewal: %v", err)
	}
	if !got.EndAt.UTC().Equal(oldEnd.Add(30 * 24 * time.Hour)) {
		t.Fatalf("end_at = %s, want oldEnd+30d", got.EndAt)
	}
}
