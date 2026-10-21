//go:build integration

package store

// S1（W2）订阅状态迁移事件流的数据库级验收（R-17 / R-18）。
//
// 覆盖单元测试无法证明的真相：
//   - 每条状态迁移（创建/续费/周期末取消/立即取消/到期/重置）与事件
//     **同事务**落地；
//   - seq 在同一订阅内从 1 开始连续无洞，after_seq 增量读取可用；
//   - 并发两条迁移不产生重复 seq（订阅行锁 + 唯一键兜底）；
//   - 幂等重放不追加第二条事件（事件流是迁移账本，不是调用日志）。
//
// 运行（隔离库，先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_s1?sslmode=disable' \
//	GOPROXY=https://goproxy.cn,direct \
//	go test -tags=integration ./internal/store/ -run TestSubscriptionEvents -v

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// cleanupSubscriptionEvents 在实例被删除前清掉其事件。
//
// 事件表刻意不设 FK（历史必须独立于订阅行存在），因此数据库不会级联删除；
// 测试自己维护卫生，避免同一隔离库反复运行时事件越积越多。
func cleanupSubscriptionEvents(t *testing.T, db *sql.DB, planID int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_events WHERE subscription_id IN
			(SELECT id FROM subscriptions WHERE plan_id = $1)`, planID)
	})
}

func listEvents(t *testing.T, repo *SQLSubscriptionRepo, subscriptionID, afterSeq int64) []SubscriptionEvent {
	t.Helper()
	events, err := repo.ListSubscriptionEvents(subscriptionCtx(), subscriptionID, afterSeq, 100)
	if err != nil {
		t.Fatalf("list events(sub=%d after=%d): %v", subscriptionID, afterSeq, err)
	}
	return events
}

func requireContiguousSeq(t *testing.T, events []SubscriptionEvent) {
	t.Helper()
	for i, event := range events {
		if want := int64(i + 1); event.Seq != want {
			t.Fatalf("第 %d 条事件 seq = %d，期望 %d（事件流必须连续无洞）", i, event.Seq, want)
		}
	}
}

func eventDetail(t *testing.T, event SubscriptionEvent) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		t.Fatalf("decode detail of seq %d: %v（%s）", event.Seq, err, event.Detail)
	}
	return detail
}

func TestSubscriptionEventsLifecycle(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-events")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC().Truncate(time.Second)

	// ---- 创建 → created ----
	sub := createTestSubscription(t, repo, plan, 900101, now.Add(-time.Hour), now.Add(30*24*time.Hour), nil)
	events := listEvents(t, repo, sub.ID, 0)
	if len(events) != 1 || events[0].Kind != SubscriptionEventCreated {
		t.Fatalf("创建后事件 = %+v，期望恰一条 created", events)
	}
	requireContiguousSeq(t, events)
	detail := eventDetail(t, events[0])
	if detail["plan_id"] != float64(plan.ID) ||
		detail["amount_total"] != float64(plan.GrantCredits) ||
		detail["price_paid_amount"] != float64(plan.PriceAmount) {
		t.Fatalf("created detail = %v", detail)
	}

	// ---- 续费 → renewed（调用方事务，与 billing 同构） ----
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin renewal tx: %v", err)
	}
	if _, err := RenewSubscription(ctx, tx, RenewalExtendInput{
		SubscriptionID:    sub.ID,
		UserID:            sub.UserID,
		PlanID:            plan.ID,
		PlanTitleSnapshot: plan.Title,
		PeriodDays:        plan.PeriodDays,
		GrantCredits:      plan.GrantCredits,
		ResetOnRenew:      plan.ResetOnRenew,
		GraceDays:         plan.GraceDays,
		Currency:          plan.Currency,
		PricePaidAmount:   plan.PriceAmount,
		Now:               now,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("renew: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit renewal: %v", err)
	}
	events = listEvents(t, repo, sub.ID, 0)
	if len(events) != 2 || events[1].Kind != SubscriptionEventRenewed {
		t.Fatalf("续费后事件 = %+v", events)
	}
	requireContiguousSeq(t, events)

	// ---- 周期末取消 → cancelled(immediate=false)，幂等重放不再追加 ----
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); err != nil {
		t.Fatalf("cancel at period end: %v", err)
	}
	if err := repo.CancelAtPeriodEnd(ctx, sub.ID); err != nil {
		t.Fatalf("repeat cancel at period end: %v", err)
	}
	events = listEvents(t, repo, sub.ID, 0)
	if len(events) != 3 || events[2].Kind != SubscriptionEventCancelled {
		t.Fatalf("取消后事件 = %+v（幂等重放不得重复追加）", events)
	}
	if got := eventDetail(t, events[2])["immediate"]; got != false {
		t.Fatalf("周期末取消 detail.immediate = %v，期望 false", got)
	}

	// ---- 立即取消 → cancelled(immediate=true) ----
	immediateSub := createTestSubscription(t, repo, plan, 900102,
		now.Add(-time.Hour), now.Add(30*24*time.Hour), nil)
	if err := repo.CancelImmediately(ctx, immediateSub.ID); err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}
	if err := repo.CancelImmediately(ctx, immediateSub.ID); err != nil {
		t.Fatalf("repeat cancel immediately: %v（已终态应幂等）", err)
	}
	immediateEvents := listEvents(t, repo, immediateSub.ID, 0)
	if len(immediateEvents) != 2 || immediateEvents[1].Kind != SubscriptionEventCancelled {
		t.Fatalf("立即取消事件 = %+v", immediateEvents)
	}
	if got := eventDetail(t, immediateEvents[1])["immediate"]; got != true {
		t.Fatalf("立即取消 detail.immediate = %v，期望 true", got)
	}

	// ---- 重置 → reset（本次真正命中条件 UPDATE 才写事件） ----
	resetSub, err := repo.CreateSubscription(ctx, SubscriptionCreateInput{
		UserID:            900103,
		PlanID:            plan.ID,
		PlanTitleSnapshot: plan.Title,
		StartAt:           now.Add(-48 * time.Hour),
		EndAt:             now.Add(28 * 24 * time.Hour),
		NextResetAt:       timePtr(now.Add(-time.Hour)),
		AmountTotal:       plan.GrantCredits,
		AmountUsed:        42,
		Currency:          plan.Currency,
		PricePaidAmount:   plan.PriceAmount,
		PeriodDays:        plan.PeriodDays,
		ResetOnRenew:      true,
		GraceDays:         plan.GraceDays,
	})
	if err != nil {
		t.Fatalf("create reset subscription: %v", err)
	}
	affected, err := repo.ResetSubscriptionQuota(ctx, resetSub.ID, now)
	if err != nil || affected != 1 {
		t.Fatalf("reset quota = %d / %v", affected, err)
	}
	// 重复 pass：游标已推进，条件 UPDATE 不再命中 → 不得追加第二条 reset。
	if affected, err := repo.ResetSubscriptionQuota(ctx, resetSub.ID, now); err != nil || affected != 0 {
		t.Fatalf("repeat reset = %d / %v，期望 0（幂等）", affected, err)
	}
	resetEvents := listEvents(t, repo, resetSub.ID, 0)
	if len(resetEvents) != 2 || resetEvents[1].Kind != SubscriptionEventReset {
		t.Fatalf("重置事件 = %+v", resetEvents)
	}
	if got := eventDetail(t, resetEvents[1])["amount_total"]; got != float64(plan.GrantCredits) {
		t.Fatalf("reset detail.amount_total = %v", got)
	}

	// ---- 到期 → expired ----
	expireSub := createTestSubscription(t, repo, plan, 900104,
		now.Add(-31*24*time.Hour), now.Add(-time.Hour), nil)
	expired, err := repo.ExpireDue(ctx, now)
	if err != nil {
		t.Fatalf("expire due: %v", err)
	}
	if expired < 1 {
		t.Fatalf("expire due affected = %d，期望至少 1", expired)
	}
	expireEvents := listEvents(t, repo, expireSub.ID, 0)
	if len(expireEvents) != 2 || expireEvents[1].Kind != SubscriptionEventExpired {
		t.Fatalf("到期事件 = %+v", expireEvents)
	}
	// 重复扫描不重复写事件。
	if _, err := repo.ExpireDue(ctx, now); err != nil {
		t.Fatalf("repeat expire due: %v", err)
	}
	if got := listEvents(t, repo, expireSub.ID, 0); len(got) != 2 {
		t.Fatalf("重复到期扫描后事件数 = %d，期望 2", len(got))
	}

	// ---- after_seq 增量读取 ----
	after := listEvents(t, repo, sub.ID, 1)
	if len(after) != 2 || after[0].Seq != 2 || after[1].Seq != 3 {
		t.Fatalf("after_seq=1 返回 = %+v", after)
	}
	if got := listEvents(t, repo, sub.ID, 3); len(got) != 0 {
		t.Fatalf("after_seq=3 应返回空，实际 %+v", got)
	}
}

// TestSubscriptionEventsConcurrentTransitionsKeepSeqContiguous 并发两条迁移
// （这里是 5 条并发续费）不得产生重复或跳号 seq。
//
// 续费路径允许 active 实例反复扩展，因此每个事务都会成功并各写一条事件，
// 是验证 R-17「行锁内取 max+1 / 唯一键冲突整体重试而非跳号」的最直接场景。
func TestSubscriptionEventsConcurrentTransitionsKeepSeqContiguous(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-concurrent")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC().Truncate(time.Second)
	sub := createTestSubscription(t, repo, plan, 900201,
		now.Add(-time.Hour), now.Add(30*24*time.Hour), nil)

	const renewals = 5
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < renewals; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if _, err := RenewSubscription(ctx, tx, RenewalExtendInput{
				SubscriptionID:    sub.ID,
				UserID:            sub.UserID,
				PlanID:            plan.ID,
				PlanTitleSnapshot: plan.Title,
				PeriodDays:        plan.PeriodDays,
				GrantCredits:      plan.GrantCredits,
				ResetOnRenew:      plan.ResetOnRenew,
				GraceDays:         plan.GraceDays,
				Currency:          plan.Currency,
				PricePaidAmount:   int64(n),
				Now:               now,
			}); err != nil {
				_ = tx.Rollback()
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if err := tx.Commit(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发续费失败：%v", errs[0])
	}

	events := listEvents(t, repo, sub.ID, 0)
	if len(events) != renewals+1 {
		t.Fatalf("事件数 = %d，期望 %d（created + %d renewed）", len(events), renewals+1, renewals)
	}
	requireContiguousSeq(t, events)
	if events[0].Kind != SubscriptionEventCreated {
		t.Fatalf("第一条事件 kind = %s，期望 created", events[0].Kind)
	}
	renewedSeqs := make(map[int64]bool, renewals)
	for _, event := range events[1:] {
		if event.Kind != SubscriptionEventRenewed {
			t.Fatalf("seq %d kind = %s，期望 renewed", event.Seq, event.Kind)
		}
		if renewedSeqs[event.Seq] {
			t.Fatalf("seq %d 重复", event.Seq)
		}
		renewedSeqs[event.Seq] = true
	}

	// 唯一键兜底：直接插入已存在的 (subscription_id, seq) 必须报冲突类错误，
	// 而不是静默覆盖或跳号。AppendSubscriptionEvent 会把 23505 翻译成
	// ErrSubscriptionEventSeqConflict（调用方据此整体重试）。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin duplicate probe: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO subscription_events (subscription_id, seq, kind)
		VALUES ($1, 1, 'created')`, sub.ID); err == nil {
		t.Fatal("重复 (subscription_id, seq) 插入应被唯一键拒绝")
	} else if !isUniqueViolation(err) {
		t.Fatalf("重复插入错误 = %v，期望 unique violation", err)
	}
}

// TestAppendSubscriptionEventRejectsUnknownKind 冻结白名单之外的 kind 必须在
// 应用层拒绝（DB CHECK 只是最后防线；绕过白名单的拼写会污染事件流）。
func TestAppendSubscriptionEventRejectsUnknownKind(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-kind")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC()
	sub := createTestSubscription(t, repo, plan, 900301, now.Add(-time.Hour), now.Add(24*time.Hour), nil)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = AppendSubscriptionEvent(ctx, tx, sub.ID, "migrated", nil)
	if !errors.Is(err, ErrSubscriptionEventInvalid) {
		t.Fatalf("错误 = %v，期望 ErrSubscriptionEventInvalid", err)
	}
	if _, err := tx.Exec(`SELECT 1`); err != nil {
		t.Fatalf("入库前拒绝不得让事务进入 aborted 状态: %v", err)
	}
}
