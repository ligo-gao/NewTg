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
		ExpectedEndAt:     sub.EndAt,
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

// TestSubscriptionEventsConcurrentTransitionsKeepSeqContiguous 并发追加事件
// 不得产生重复或跳号 seq。
//
// 直接并发调用 AppendSubscriptionEvent（等价于 5 条并发迁移）：每个事务在
// 订阅行锁内取 max+1，是验证 R-17「唯一键冲突整体重试而非跳号」的最直接
// 场景。R-43 之后续费本身不允许同周期并发成功，故不通过 RenewSubscription
// 制造并发。
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
			// R-43 之后同一周期的 RenewSubscription 只允许一笔成功，因此
			// 这里直接并发追加 renewed 事件来验证 seq 机制本身（行锁内取
			// max+1、唯一键冲突整体重试）。事件写入路径与续费共用同一个
			// AppendSubscriptionEvent，覆盖目标不变。
			if err := AppendSubscriptionEvent(ctx, tx, sub.ID,
				SubscriptionEventRenewed, map[string]any{
					"plan_id":           plan.ID,
					"amount_total":      plan.GrantCredits,
					"price_paid_amount": int64(n),
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

// TestResetPlanQuotaWritesAdminEvents 覆盖 I1/R-21：管理侧批量重置与 worker 的
// 周期重置语义相同，必须写 reset 事件，并用 detail.actor="admin" 区分来源。
func TestResetPlanQuotaWritesAdminEvents(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-admin-reset")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC().Truncate(time.Second)
	subs := []Subscription{
		createTestSubscription(t, repo, plan, 900401, now.Add(-time.Hour), now.Add(30*24*time.Hour), nil),
		createTestSubscription(t, repo, plan, 900402, now.Add(-time.Hour), now.Add(30*24*time.Hour), nil),
	}
	// 两项在批量重置前都有 created（seq 1）。
	for _, sub := range subs {
		if events := listEvents(t, repo, sub.ID, 0); len(events) != 1 {
			t.Fatalf("前置事件 = %+v", events)
		}
	}

	affected, err := repo.ResetPlanQuota(ctx, plan.ID, true)
	if err != nil || affected < 2 {
		t.Fatalf("reset plan quota = %d / %v", affected, err)
	}
	for _, sub := range subs {
		events := listEvents(t, repo, sub.ID, 0)
		if len(events) != 2 || events[1].Kind != SubscriptionEventReset {
			t.Fatalf("管理员重置后事件 = %+v", events)
		}
		if detail := eventDetail(t, events[1]); detail["actor"] != "admin" {
			t.Fatalf("reset detail.actor = %v，期望 admin", detail["actor"])
		}
	}
}

// TestExpireDueProcessesBatches 覆盖 I2/R-22：到期扫描单次 ≤ ExpireScanLimit，
// 多轮处理完积压；每行恰一条 expired 事件（重复扫描不重复追加）。
func TestExpireDueProcessesBatches(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-expire-batch")
	cleanupSubscriptionEvents(t, db, plan.ID)

	const total = ExpireScanLimit + 5
	rows, err := db.Query(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 amount_total, amount_used, allow_wallet_overflow, currency,
			 price_paid_amount, period_days, reset_on_renew, grace_days)
		SELECT 900500 + g, $1, 'batch', 'active',
		       now() - interval '40 days', now() - interval '1 day',
		       0, 0, true, 'CNY', 0, 30, false, 0
		  FROM generate_series(1, $2) AS g
		RETURNING id`, plan.ID, total)
	if err != nil {
		t.Fatalf("insert due subscriptions: %v", err)
	}
	ids := make([]int64, 0, total)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatalf("scan due subscription id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate due ids: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close due ids: %v", err)
	}

	now := time.Now().UTC()
	var processed int64
	first := true
	for round := 0; ; round++ {
		affected, err := repo.ExpireDue(ctx, now)
		if err != nil {
			t.Fatalf("expire due round %d: %v", round, err)
		}
		if first {
			if affected > ExpireScanLimit {
				t.Fatalf("首批 = %d，超过上界 %d", affected, ExpireScanLimit)
			}
			first = false
		}
		processed += affected
		if affected < ExpireScanLimit {
			break
		}
		if round > 10 {
			t.Fatal("分批循环未收敛")
		}
	}
	if processed < total {
		t.Fatalf("累计到期 = %d，期望至少 %d", processed, total)
	}

	// 每行恰一条 expired 事件；重复扫描（已无 due 行）不追加。
	for _, id := range ids {
		events := listEvents(t, repo, id, 0)
		if len(events) != 1 || events[0].Seq != 1 || events[0].Kind != SubscriptionEventExpired {
			t.Fatalf("订阅 %d 事件 = %+v，期望恰一条 expired", id, events)
		}
	}
	if again, err := repo.ExpireDue(ctx, now); err != nil || again != 0 {
		t.Fatalf("重复扫描 = %d/%v，期望 0/nil", again, err)
	}
}

// TestAppendSubscriptionEventConcurrentWithoutCallerLock 覆盖 I3：并发事务
// **直接**调用 AppendSubscriptionEvent（调用方不做任何行写）。去掉函数内的
// `SELECT ... FOR UPDATE` 后，两个事务会算出同一个 seq，其中一个撞唯一键并
// 报 ErrSubscriptionEventSeqConflict —— 本用例必须因此变红。
func TestAppendSubscriptionEventConcurrentWithoutCallerLock(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-direct-append")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC().Truncate(time.Second)
	sub := createTestSubscription(t, repo, plan, 900601, now.Add(-time.Hour), now.Add(24*time.Hour), nil)

	const writers = 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if err := AppendSubscriptionEvent(ctx, tx, sub.ID, SubscriptionEventReset,
				map[string]any{"actor": "worker"}); err != nil {
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
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发直接追加失败（去掉行锁时这里会因 seq 冲突报错）：%v", errs[0])
	}
	events := listEvents(t, repo, sub.ID, 0)
	if len(events) != writers+1 {
		t.Fatalf("事件数 = %d，期望 %d", len(events), writers+1)
	}
	requireContiguousSeq(t, events)
}

// TestWithEventTxRetriesSeqConflict 覆盖 I3：强制一次真实的 seq 唯一键冲突
// （一个未提交的旁路事务先占了 next seq），断言 withEventTx 整体重试后
// seq 连续、事件数正确、不跳号。
func TestWithEventTxRetriesSeqConflict(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-retry")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC().Truncate(time.Second)
	sub := createTestSubscription(t, repo, plan, 900701, now.Add(-time.Hour), now.Add(24*time.Hour), nil)

	// 旁路事务：不经 AppendSubscriptionEvent（不加订阅行锁）占住 seq 2，
	// 先不提交。CancelImmediately 的首轮会算出 max+1=2 并在唯一键上等待。
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(`INSERT INTO subscription_events
		(subscription_id, seq, kind, detail) VALUES ($1, 2, 'reset', '{}'::jsonb)`, sub.ID); err != nil {
		_ = blocker.Rollback()
		t.Fatalf("blocker insert: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- repo.CancelImmediately(ctx, sub.ID) }()
	waitForBlockedStatement(t, db)
	if err := blocker.Commit(); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("CancelImmediately 未在冲突后重试成功: %v", err)
	}

	events := listEvents(t, repo, sub.ID, 0)
	if len(events) != 3 {
		t.Fatalf("事件数 = %d，期望 3（created + 占位 + 重试后的 cancelled）: %+v", len(events), events)
	}
	requireContiguousSeq(t, events)
	if events[1].Kind != SubscriptionEventReset || events[2].Kind != SubscriptionEventCancelled {
		t.Fatalf("事件顺序 = %+v", events)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM subscriptions WHERE id = $1`, sub.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	// R-48：立即取消不再把 status 置 cancelled，而是保留 active 并把 end_at
	// 提前（宽限期语义）；本用例只关心事件重试，因此断言「迁移已落地」。
	if status != "active" {
		t.Fatalf("重试后 status = %s，期望 active（立即取消进入宽限）", status)
	}
}

// waitForBlockedStatement 轮询 pg_locks，直到出现未授予的锁（目标事务已经
// 走到唯一键/行锁等待）。用于把并发测试的时序变成确定事件，而不是 sleep。
//
// 【为什么只数「未授予」】唯一索引冲突的等待在 pg_locks 里是事务级锁
// （locktype=transactionid），不带 relation，按表/索引名过滤会永远数不到；
// 隔离库里此时只有目标语句在等锁，数总未授予数即可。
func waitForBlockedStatement(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRow(`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待阻塞锁超时（目标事务可能没有走到冲突点）")
}

// TestResetPlanQuotaRetriesSeqConflict 覆盖复核 Important：批量重置在事件 seq
// 冲突、withEventTx 整体重试时，不得重复计行或重复写事件。
//
// 冲突由旁路事务制造（不经 AppendSubscriptionEvent、不占订阅行锁，先占住
// 第一个实例的 next seq，kind 用 created 以免与待验证的 reset 混淆）。若
// ids/totals/batch 声明在闭包外，重试会把同一批 id 再 append 一遍：affected
// 翻倍、每个实例出现两条 admin reset。本用例必须因此变红。
func TestResetPlanQuotaRetriesSeqConflict(t *testing.T) {
	db := openTestDB(t)
	repo := NewSubscriptionRepo(db)
	ctx := subscriptionCtx()
	plan := newTestPlan(t, repo, db, "s1-plan-reset-retry")
	cleanupSubscriptionEvents(t, db, plan.ID)

	now := time.Now().UTC().Truncate(time.Second)
	// 直接插入实例（不经过 CreateSubscription）：事件流从空白开始，占位事件
	// 不会与 created 计数纠缠，断言「恰一条 reset」更直接。
	subs := make([]int64, 0, 2)
	for i := 0; i < 2; i++ {
		var id int64
		err := db.QueryRow(`
			INSERT INTO subscriptions
				(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
				 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
				 currency, price_paid_amount, period_days, reset_on_renew, grace_days)
			VALUES ($1, $2, 'x', 'active', $3, $4, $4, 1000, 55, true,
			        'CNY', 0, 30, true, 0)
			RETURNING id`, 900800+i, plan.ID, now.Add(-time.Hour), now.Add(30*24*time.Hour)).Scan(&id)
		if err != nil {
			t.Fatalf("insert subscription %d: %v", i, err)
		}
		subs = append(subs, id)
	}

	// 旁路事务占住第一个实例的 seq 1，暂不提交。
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(`INSERT INTO subscription_events
		(subscription_id, seq, kind, detail) VALUES ($1, 1, 'created', '{}'::jsonb)`, subs[0]); err != nil {
		_ = blocker.Rollback()
		t.Fatalf("blocker insert: %v", err)
	}

	type resetResult struct {
		affected int64
		err      error
	}
	done := make(chan resetResult, 1)
	go func() {
		affected, resetErr := repo.ResetPlanQuota(ctx, plan.ID, true)
		done <- resetResult{affected: affected, err: resetErr}
	}()
	waitForBlockedStatement(t, db)
	if err := blocker.Commit(); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	result := <-done
	if result.err != nil {
		t.Fatalf("ResetPlanQuota 未在 seq 冲突后重试成功: %v", result.err)
	}
	// 返回值必须等于实际订阅数，不能因重试把同一批重复计数。
	if result.affected != int64(len(subs)) {
		t.Fatalf("affected = %d，期望 %d（重试不得重复计行）", result.affected, len(subs))
	}

	for _, id := range subs {
		events := listEvents(t, repo, id, 0)
		requireContiguousSeq(t, events)
		var quotaResets int
		for _, event := range events {
			if event.Kind != SubscriptionEventReset {
				continue
			}
			quotaResets++
			if detail := eventDetail(t, event); detail["actor"] != "admin" {
				t.Fatalf("订阅 %d reset detail.actor = %v", id, detail["actor"])
			}
		}
		if quotaResets != 1 {
			t.Fatalf("订阅 %d reset 事件数 = %d，期望 1（重试不得重复写事件）: %+v",
				id, quotaResets, events)
		}
	}
}
