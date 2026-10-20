//go:build integration

package subscriptionworker

// B12 的真实 PG 验收（billing-06 §9.3、billing-08 §7）。
//
// 覆盖单测（fake）无法证明的 SQL 语义：
//   - 到期条件：宽限期只适用于未取消续费的实例；cancel_at_period_end=true
//     到期即终止、不享宽限（M4）；
//   - 重置原子推进 next_reset_at，第二次 pass 不双倍重置；
//   - 宽限期内（end_at < now < end_at+grace）不发放新周期额度（H3），
//     包括 end_at = now 的等值边界；
//   - 停机多轮后游标一次推进到第一个严格晚于 now 的边界（M2）；
//   - amount_total=0 的无限额度实例与其他实例一样统一归零（H1）；
//   - 预扣幂等记录按 created_at 的 7 天边界删除，且分批执行。
//
// 运行（隔离库，先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_b12?sslmode=disable' \
//	  go test -tags=integration ./internal/subscriptionworker/ -count=1 -v
//
// 用例只使用带本次运行唯一后缀的 plan_key / request_id，并在结束时精确
// 删除自己的行；不 DROP/TRUNCATE（共享隔离库上可能有并行包）。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/store"
)

func openIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func integrationRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// integrationPlan 创建套餐并登记清理（先删实例/幂等记录，再删套餐）。
func integrationPlan(t *testing.T, db *sql.DB, repo *store.SQLSubscriptionRepo) store.SubscriptionPlan {
	t.Helper()
	plan, err := repo.CreatePlan(context.Background(), store.SubscriptionPlanInput{
		PlanKey:      fmt.Sprintf("b12-%d", time.Now().UnixNano()),
		Title:        "B12 Test Plan",
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

func integrationSub(t *testing.T, repo *store.SQLSubscriptionRepo, plan store.SubscriptionPlan,
	in store.SubscriptionCreateInput) store.Subscription {
	t.Helper()
	in.PlanID = plan.ID
	if in.PlanTitleSnapshot == "" {
		in.PlanTitleSnapshot = plan.Title
	}
	if in.Currency == "" {
		in.Currency = "CNY"
	}
	if in.PeriodDays == 0 {
		in.PeriodDays = plan.PeriodDays
	}
	sub, err := repo.CreateSubscription(context.Background(), in)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return sub
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestIntegrationExpireDueGraceAndCancelAtPeriodEnd 验证到期边界。
func TestIntegrationExpireDueGraceAndCancelAtPeriodEnd(t *testing.T) {
	db := openIntegrationDB(t)
	repo := store.NewSubscriptionRepo(db)
	ctx := context.Background()
	plan := integrationPlan(t, db, repo)
	// PG timestamptz 是微秒精度；边界断言使用同一精度。
	now := time.Now().UTC().Truncate(time.Microsecond)

	expired := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910001, StartAt: now.Add(-2 * time.Hour), EndAt: now.Add(-time.Hour),
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30,
	})
	// 宽限期快照在实例上：end_at 已过，但 end_at+5d 未到。
	grace := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910002, StartAt: now.Add(-2 * time.Hour), EndAt: now.Add(-time.Hour),
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30, GraceDays: 5,
	})
	// 周期末取消仍是 active，到期后必须迁移为 expired。
	cancelling := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910003, StartAt: now.Add(-2 * time.Hour), EndAt: now.Add(-time.Hour),
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30,
		CancelAtPeriodEnd: true,
	})
	// M4：已取消续费的实例即使配置了宽限期，也不得在宽限期内继续存活。
	cancellingGrace := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910005, StartAt: now.Add(-2 * time.Hour), EndAt: now.Add(-time.Hour),
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30, GraceDays: 5,
		CancelAtPeriodEnd: true,
	})
	// 等值边界：end_at = now 且 grace=0 应到期（<=）。
	boundary := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910006, StartAt: now.Add(-2 * time.Hour), EndAt: now,
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30,
	})
	future := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910004, StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30,
	})
	// 未到期的取消实例保持 active（周期末才终止）。
	cancelFuture := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910007, StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		AmountTotal: 1000, Currency: "CNY", PeriodDays: 30, GraceDays: 5,
		CancelAtPeriodEnd: true,
	})

	if _, err := repo.ExpireDue(ctx, now); err != nil {
		t.Fatalf("expire due: %v", err)
	}
	assertStatus(t, repo, expired.ID, "expired")
	assertStatus(t, repo, cancelling.ID, "expired")
	assertStatus(t, repo, cancellingGrace.ID, "expired")
	assertStatus(t, repo, boundary.ID, "expired")
	assertStatus(t, repo, grace.ID, "active")
	assertStatus(t, repo, future.ID, "active")
	assertStatus(t, repo, cancelFuture.ID, "active")

	// 重复 pass：已迁移行不再命中（条件 UPDATE 幂等）。
	if affected, err := repo.ExpireDue(ctx, now); err != nil || affected != 0 {
		t.Fatalf("repeated expire = %d/%v，want 0/nil", affected, err)
	}

	// 宽限期结束后到期。
	if _, err := repo.ExpireDue(ctx, now.Add(5*24*time.Hour)); err != nil {
		t.Fatalf("expire after grace: %v", err)
	}
	assertStatus(t, repo, grace.ID, "expired")
	// 未来实例此刻也早已过了 end_at（+1h），同样到期；仅确认终态单向。
	assertStatus(t, repo, future.ID, "expired")
	assertStatus(t, repo, cancelFuture.ID, "expired")
}

// TestIntegrationResetCursorAndUnlimited 验证额度重置、H1/H3/M2 边界。
func TestIntegrationResetCursorAndUnlimited(t *testing.T) {
	db := openIntegrationDB(t)
	repo := store.NewSubscriptionRepo(db)
	plan := integrationPlan(t, db, repo)
	now := time.Now().UTC().Truncate(time.Microsecond)
	oldReset := now.Add(-time.Minute)

	regular := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910011, StartAt: now.Add(-31 * 24 * time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: ptrTime(oldReset), AmountTotal: 1000, AmountUsed: 400,
		Currency: "CNY", PeriodDays: 30, ResetOnRenew: true,
	})
	// H1：无限额度实例与其他实例统一归零，不再有「只推进游标」特判。
	unlimited := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910012, StartAt: now.Add(-31 * 24 * time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: ptrTime(oldReset), AmountTotal: 0, AmountUsed: 777,
		Currency: "CNY", PeriodDays: 30, ResetOnRenew: true,
	})
	// H3：宽限期内（end_at < now < end_at+grace）不得发放新周期额度。
	inGrace := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910013, StartAt: now.Add(-32 * 24 * time.Hour), EndAt: now.Add(-time.Hour),
		NextResetAt: ptrTime(oldReset), AmountTotal: 1000, AmountUsed: 250,
		Currency: "CNY", PeriodDays: 30, GraceDays: 5, ResetOnRenew: true,
	})
	// H3 等值边界：end_at = now 同样不重置（守卫是 end_at > now）。
	endBoundary := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910014, StartAt: now.Add(-31 * 24 * time.Hour), EndAt: now,
		NextResetAt: ptrTime(oldReset), AmountTotal: 1000, AmountUsed: 300,
		Currency: "CNY", PeriodDays: 30, GraceDays: 5, ResetOnRenew: true,
	})
	// M2：停机 62 天、周期 30 天，游标一次推进到第一个严格晚于 now 的边界。
	downtimeReset := now.Add(-62 * 24 * time.Hour)
	afterDowntime := integrationSub(t, repo, plan, store.SubscriptionCreateInput{
		UserID: 910015, StartAt: now.Add(-93 * 24 * time.Hour), EndAt: now.Add(time.Hour),
		NextResetAt: ptrTime(downtimeReset), AmountTotal: 1000, AmountUsed: 500,
		Currency: "CNY", PeriodDays: 30, ResetOnRenew: true,
	})

	w, err := newWorker(Deps{
		Repo:    repo,
		Cleaner: repo,
		Redis:   integrationRedis(t),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}

	res, ran, err := w.sweep(context.Background())
	if err != nil || !ran {
		t.Fatalf("sweep = %+v/%v/%v，want ran/nil", res, ran, err)
	}
	if res.Reset < 3 {
		t.Fatalf("sweep result = %+v，want reset >= 3（regular/unlimited/downtime）", res)
	}

	regularGot, err := repo.GetSubscription(context.Background(), regular.ID)
	if err != nil {
		t.Fatalf("get regular: %v", err)
	}
	if regularGot.AmountUsed != 0 {
		t.Fatalf("regular amount_used = %d，want 0", regularGot.AmountUsed)
	}
	if regularGot.NextResetAt == nil || !regularGot.NextResetAt.Equal(oldReset.Add(30*24*time.Hour)) {
		t.Fatalf("regular next_reset_at = %v，want %v", regularGot.NextResetAt, oldReset.Add(30*24*time.Hour))
	}

	unlimitedGot, err := repo.GetSubscription(context.Background(), unlimited.ID)
	if err != nil {
		t.Fatalf("get unlimited: %v", err)
	}
	if unlimitedGot.AmountUsed != 0 {
		t.Fatalf("unlimited amount_used = %d，want 0（H1：统一归零）", unlimitedGot.AmountUsed)
	}
	if unlimitedGot.NextResetAt == nil || !unlimitedGot.NextResetAt.Equal(oldReset.Add(30*24*time.Hour)) {
		t.Fatalf("unlimited next_reset_at = %v，want %v", unlimitedGot.NextResetAt, oldReset.Add(30*24*time.Hour))
	}

	// H3：宽限期实例保持 active、额度与游标都不动。
	graceGot, err := repo.GetSubscription(context.Background(), inGrace.ID)
	if err != nil {
		t.Fatalf("get in-grace: %v", err)
	}
	if graceGot.Status != "active" {
		t.Fatalf("in-grace status = %q，want active", graceGot.Status)
	}
	if graceGot.AmountUsed != 250 {
		t.Fatalf("in-grace amount_used = %d，want 250（宽限期不得发放新周期额度）", graceGot.AmountUsed)
	}
	if graceGot.NextResetAt == nil || !graceGot.NextResetAt.Equal(oldReset) {
		t.Fatalf("in-grace next_reset_at = %v，want %v（不得推进）", graceGot.NextResetAt, oldReset)
	}
	// 即使绕过扫描直接调用重置，SQL 守卫也必须拒绝（affected=0）。
	if affected, err := repo.ResetSubscriptionQuota(context.Background(), inGrace.ID, now); err != nil || affected != 0 {
		t.Fatalf("in-grace direct reset = %d/%v，want 0/nil", affected, err)
	}

	// H3 等值边界：end_at = now 且处于宽限期内，重置不得命中。
	if affected, err := repo.ResetSubscriptionQuota(context.Background(), endBoundary.ID, now); err != nil || affected != 0 {
		t.Fatalf("end-boundary reset = %d/%v，want 0/nil", affected, err)
	}

	// M2：62 天停机 → floor(62/30)+1 = 3 个周期 = 90 天。
	downGot, err := repo.GetSubscription(context.Background(), afterDowntime.ID)
	if err != nil {
		t.Fatalf("get downtime: %v", err)
	}
	wantDown := downtimeReset.Add(90 * 24 * time.Hour)
	if downGot.NextResetAt == nil || !downGot.NextResetAt.Equal(wantDown) {
		t.Fatalf("downtime next_reset_at = %v，want %v", downGot.NextResetAt, wantDown)
	}
	if !downGot.NextResetAt.After(now) {
		t.Fatalf("downtime next_reset_at = %v，必须严格晚于 now=%v", downGot.NextResetAt, now)
	}
	if downGot.AmountUsed != 0 {
		t.Fatalf("downtime amount_used = %d，want 0", downGot.AmountUsed)
	}

	// 同一 now 的重复 pass：游标都已前移，affected 全为 0。
	res, ran, err = w.sweep(context.Background())
	if err != nil || !ran {
		t.Fatalf("second sweep = %v/%v，want ran/nil", ran, err)
	}
	if res.Reset != 0 {
		t.Fatalf("second sweep result = %+v，want no double reset", res)
	}
}

// TestIntegrationCleanupSevenDayBoundary 验证按 created_at 的保留期边界，
// 并确认分批上限不会漏删。
func TestIntegrationCleanupSevenDayBoundary(t *testing.T) {
	db := openIntegrationDB(t)
	repo := store.NewSubscriptionRepo(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-PreConsumeRetention)

	// 本用例使用本次运行独有的 subscription_id 范围，清理语句只删这些行；
	// 共享隔离库上可能有并行包，固定 ID 会误删别人的数据。
	baseSubID := time.Now().UnixNano() % 1_000_000_000
	subA, subB := baseSubID, baseSubID+1

	insert := func(createdAt time.Time, subscriptionID int64) string {
		requestID := uuid.NewString()
		if _, err := db.Exec(`INSERT INTO subscription_pre_consume_records
			(request_id, subscription_id, status, pre_consumed, created_at)
			VALUES ($1, $2, 'consumed', 10, $3)`, requestID, subscriptionID, createdAt); err != nil {
			t.Fatalf("insert pre-consume record: %v", err)
		}
		return requestID
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records
			WHERE subscription_id IN ($1, $2)`, subA, subB)
	})

	// 边界：恰在 cutoff 之前删除；cutoff 之后保留（严格 <）。
	oldID := insert(cutoff.Add(-time.Second), subA)
	justOldID := insert(cutoff.Add(-time.Microsecond), subA)
	justNewID := insert(cutoff.Add(time.Microsecond), subB)
	freshID := insert(now, subB)
	// 额外 5 条旧行：BatchSize=2 时必须循环多批才能全部清掉。
	for i := 0; i < 5; i++ {
		insert(cutoff.Add(-time.Duration(i+2)*time.Minute), subA)
	}

	w, err := newWorker(Deps{
		Repo:       repo,
		Cleaner:    repo,
		Redis:      integrationRedis(t),
		Now:        func() time.Time { return now },
		BatchSize:  2,
		MaxBatches: 10,
	})
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}
	_, ran, err := w.cleanup(context.Background())
	if err != nil || !ran {
		t.Fatalf("cleanup ran=%v err=%v，want ran/nil", ran, err)
	}
	// 不断言全局 deleted 计数：共享库上并行包的行也会被同一轮清理带走，
	// 计数是噪声。断言只针对本次 subscription_id 范围内的具体行。

	if exists(t, db, oldID) || exists(t, db, justOldID) {
		t.Fatal("cutoff 之前的记录应被删除")
	}
	if !exists(t, db, justNewID) || !exists(t, db, freshID) {
		t.Fatal("cutoff 之后（含边界时刻）的记录必须保留")
	}
}

func assertStatus(t *testing.T, repo *store.SQLSubscriptionRepo, id int64, want string) {
	t.Helper()
	got, err := repo.GetSubscription(context.Background(), id)
	if err != nil {
		t.Fatalf("get subscription(%d): %v", id, err)
	}
	if got.Status != want {
		t.Fatalf("subscription(%d) status = %q，want %q", id, got.Status, want)
	}
}

func exists(t *testing.T, db *sql.DB, requestID string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM subscription_pre_consume_records WHERE request_id = $1`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("count pre-consume record: %v", err)
	}
	return n > 0
}
