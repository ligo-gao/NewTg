package subscriptionworker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/store"
)

// 本文件是不依赖真实 PG 的编排单测：锁竞争、重复扫描的调用协议、
// 分批清理上限与停机退出。SQL 的条件更新语义由 integration 测试用真 PG
// 证明（worker_integration_test.go）。

// fakeRepo 记录调用并提供可配置的 affected/错误。
type fakeRepo struct {
	mu sync.Mutex

	expireAffected int64
	expireErr      error
	due            []store.Subscription
	dueErr         error
	resetAffected  map[int64]int64
	resetErr       error
	// resetHook 在每次 ResetSubscriptionQuota 内执行（pass 中取消测试用）。
	resetHook func()

	expireCalls int
	resetCalls  []int64

	// sweepEntered/sweepRelease 让锁竞争测试把第一轮阻塞在 ExpireDue 内。
	sweepEntered chan struct{}
	sweepRelease chan struct{}
}

func (r *fakeRepo) ExpireDue(_ context.Context, _ time.Time) (int64, error) {
	r.mu.Lock()
	r.expireCalls++
	r.mu.Unlock()
	if r.sweepEntered != nil {
		select {
		case r.sweepEntered <- struct{}{}:
		default:
		}
		<-r.sweepRelease
	}
	return r.expireAffected, r.expireErr
}

func (r *fakeRepo) DueForReset(_ context.Context, _ time.Time) ([]store.Subscription, error) {
	if r.dueErr != nil {
		return nil, r.dueErr
	}
	return r.due, nil
}

func (r *fakeRepo) ResetSubscriptionQuota(_ context.Context, id int64, _ time.Time) (int64, error) {
	r.mu.Lock()
	r.resetCalls = append(r.resetCalls, id)
	r.mu.Unlock()
	if r.resetHook != nil {
		r.resetHook()
	}
	if r.resetErr != nil {
		return 0, r.resetErr
	}
	if affected, ok := r.resetAffected[id]; ok {
		return affected, nil
	}
	return 1, nil
}

func (r *fakeRepo) callCounts() (expire int, reset int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expireCalls, len(r.resetCalls)
}

// fakeCleaner 按脚本返回每批删除行数。
type fakeCleaner struct {
	mu      sync.Mutex
	batches []int64
	err     error
	// errOnCall 指定错误只在第 N 次调用（1 起）发生；0 表示每次都错。
	errOnCall int
	calls     int
	cutoffs   []time.Time
	limits    []int
	// onCall 在每批删除内执行（pass 中取消测试用）。
	onCall func()
}

func (c *fakeCleaner) DeletePreConsumeBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.cutoffs = append(c.cutoffs, cutoff)
	c.limits = append(c.limits, limit)
	c.mu.Unlock()
	if c.onCall != nil {
		c.onCall()
	}
	if c.err != nil && (c.errOnCall == 0 || call == c.errOnCall) {
		return 0, c.err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.batches) == 0 {
		return 0, nil
	}
	n := c.batches[0]
	c.batches = c.batches[1:]
	return n, nil
}

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func fixedNow() time.Time {
	return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
}

func newTestWorker(t *testing.T, repo Repository, cleaner Cleaner) *worker {
	t.Helper()
	w, err := newWorker(Deps{
		Repo:    repo,
		Cleaner: cleaner,
		Redis:   newTestRedis(t),
		Now:     fixedNow,
	})
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}
	return w
}

// TestSweepResetsAllInstances 验证所有到期实例统一归零 amount_used：
// amount_total>0 与 amount_total=0（无限额度）走同一条条件 UPDATE，
// 不再有无限额度的「只推进游标」特判（H1 裁决，§7 表格口径）。
//
// 第二轮的 affected=0 模拟条件 UPDATE 的幂等：重复 pass 不再计数。
func TestSweepResetsAllInstances(t *testing.T) {
	repo := &fakeRepo{
		expireAffected: 3,
		due: []store.Subscription{
			{ID: 11, AmountTotal: 1000, AmountUsed: 400},
			{ID: 12, AmountTotal: 0, AmountUsed: 777},
		},
	}
	cleaner := &fakeCleaner{}
	w := newTestWorker(t, repo, cleaner)

	res, ran, err := w.sweep(context.Background())
	if err != nil || !ran {
		t.Fatalf("sweep = %+v/%v/%v，want ran/nil", res, ran, err)
	}
	if res.Expired != 3 || res.Reset != 2 {
		t.Fatalf("result = %+v，want expired=3 reset=2", res)
	}
	if len(repo.resetCalls) != 2 || repo.resetCalls[0] != 11 || repo.resetCalls[1] != 12 {
		t.Fatalf("reset calls = %v，want [11 12]", repo.resetCalls)
	}

	// 同一 now 的第二轮：游标前移后条件 UPDATE 返回 0，不重复计数。
	repo.resetAffected = map[int64]int64{11: 0, 12: 0}
	res, ran, err = w.sweep(context.Background())
	if err != nil || !ran {
		t.Fatalf("second sweep = %v/%v，want ran/nil", ran, err)
	}
	if res.Reset != 0 {
		t.Fatalf("second sweep result = %+v，want no double reset", res)
	}
}

// TestSweepLockContention：第一副本持锁期间，第二副本整轮跳过，
// 不产生任何仓储调用（多副本下的预期路径，不是错误）。
func TestSweepLockContention(t *testing.T) {
	repo := &fakeRepo{
		expireAffected: 1,
		sweepEntered:   make(chan struct{}, 1),
		sweepRelease:   make(chan struct{}),
	}
	cleaner := &fakeCleaner{}
	w1 := newTestWorker(t, repo, cleaner)
	w2, err := newWorker(Deps{
		Repo:    repo,
		Cleaner: cleaner,
		Redis:   w1.redis,
		Now:     fixedNow,
	})
	if err != nil {
		t.Fatalf("newWorker w2: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, sweepErr := w1.sweep(context.Background())
		done <- sweepErr
	}()
	<-repo.sweepEntered

	res, ran, err := w2.sweep(context.Background())
	if err != nil {
		t.Fatalf("contended sweep err = %v，want nil", err)
	}
	if ran || res != (SweepResult{}) {
		t.Fatalf("contended sweep = %+v/%v，want skipped", res, ran)
	}
	if expire, reset := repo.callCounts(); expire != 1 || reset != 0 {
		t.Fatalf("repo calls = %d/%d，want only the lock holder to run", expire, reset)
	}

	close(repo.sweepRelease)
	if err := <-done; err != nil {
		t.Fatalf("lock holder sweep err = %v", err)
	}
}

// TestCleanupBatchesUntilShort：按 LIMIT 分批，直到返回不足一批；
// cutoff 必须是 now-7d（保留期边界由 SQL 侧执行）。
func TestCleanupBatchesUntilShort(t *testing.T) {
	repo := &fakeRepo{}
	cleaner := &fakeCleaner{batches: []int64{500, 500, 17}}
	w := newTestWorker(t, repo, cleaner)

	deleted, ran, err := w.cleanup(context.Background())
	if err != nil || !ran {
		t.Fatalf("cleanup = %d/%v/%v，want ran/nil", deleted, ran, err)
	}
	if deleted != 1017 {
		t.Fatalf("deleted = %d，want 1017", deleted)
	}
	if cleaner.calls != 3 {
		t.Fatalf("cleanup calls = %d，want 3", cleaner.calls)
	}
	wantCutoff := fixedNow().Add(-PreConsumeRetention)
	for i, cutoff := range cleaner.cutoffs {
		if !cutoff.Equal(wantCutoff) {
			t.Fatalf("batch %d cutoff = %v，want %v", i, cutoff, wantCutoff)
		}
		if cleaner.limits[i] != CleanupBatchSize {
			t.Fatalf("batch %d limit = %d，want %d", i, cleaner.limits[i], CleanupBatchSize)
		}
	}
}

// TestCleanupStopsAtPerPassBudget：持续满批时单轮有界，剩余积压留给下一轮，
// 防止清理长期占住扫描 goroutine。
func TestCleanupStopsAtPerPassBudget(t *testing.T) {
	repo := &fakeRepo{}
	batches := make([]int64, CleanupMaxBatchesPerPass)
	for i := range batches {
		batches[i] = CleanupBatchSize
	}
	cleaner := &fakeCleaner{batches: batches}
	w := newTestWorker(t, repo, cleaner)

	deleted, _, err := w.cleanup(context.Background())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if cleaner.calls != CleanupMaxBatchesPerPass {
		t.Fatalf("cleanup calls = %d，want %d", cleaner.calls, CleanupMaxBatchesPerPass)
	}
	if deleted != int64(CleanupBatchSize*CleanupMaxBatchesPerPass) {
		t.Fatalf("deleted = %d，want %d", deleted, CleanupBatchSize*CleanupMaxBatchesPerPass)
	}
}

// TestCleanupFailureStopsBatches：单批失败立即返回已删除数，不继续放大错误。
func TestCleanupFailureStopsBatches(t *testing.T) {
	repo := &fakeRepo{}
	cleaner := &fakeCleaner{batches: []int64{500}, err: errors.New("db down")}
	w := newTestWorker(t, repo, cleaner)

	deleted, ran, err := w.cleanup(context.Background())
	if err == nil || !ran || deleted != 0 {
		t.Fatalf("cleanup = %d/%v/%v，want error after batch", deleted, ran, err)
	}
}

// TestRunLoopStopsOnCancel：ticker 循环在 ctx 取消后立即退出，不残留
// goroutine 或继续调用仓储（platform 的 WaitGroup 依赖这一性质）。
func TestRunLoopStopsOnCancel(t *testing.T) {
	repo := &fakeRepo{}
	cleaner := &fakeCleaner{}
	w, err := newWorker(Deps{
		Repo:            repo,
		Cleaner:         cleaner,
		Redis:           newTestRedis(t),
		Now:             fixedNow,
		SweepInterval:   5 * time.Millisecond,
		CleanupInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunLoop(ctx, Deps{
			Repo:            repo,
			Cleaner:         cleaner,
			Redis:           w.redis,
			Now:             fixedNow,
			SweepInterval:   5 * time.Millisecond,
			CleanupInterval: 5 * time.Millisecond,
		})
	}()

	deadline := time.After(2 * time.Second)
	for {
		if expire, _ := repo.callCounts(); expire > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("RunLoop did not sweep after start")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLoop returned %v，want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not stop on context cancellation")
	}
}

// TestRunLoopPreCancelled：已取消上下文不启动任何 DB/Redis 负载。
func TestRunLoopPreCancelled(t *testing.T) {
	repo := &fakeRepo{}
	cleaner := &fakeCleaner{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunLoop(ctx, Deps{
		Repo:    repo,
		Cleaner: cleaner,
		Redis:   newTestRedis(t),
		Now:     fixedNow,
	}); err != nil {
		t.Fatalf("RunLoop = %v，want nil", err)
	}
	if expire, reset := repo.callCounts(); expire != 0 || reset != 0 {
		t.Fatalf("pre-cancelled RunLoop called repo: %d/%d", expire, reset)
	}
	if cleaner.calls != 0 {
		t.Fatalf("pre-cancelled RunLoop called cleaner %d times", cleaner.calls)
	}
}

// TestNewWorkerRequiresDeps：缺依赖必须启动失败，而不是带着 nil 指针运行。
func TestNewWorkerRequiresDeps(t *testing.T) {
	redisClient := newTestRedis(t)
	cases := []struct {
		name string
		deps Deps
	}{
		{name: "repo", deps: Deps{Cleaner: &fakeCleaner{}, Redis: redisClient}},
		{name: "cleaner", deps: Deps{Repo: &fakeRepo{}, Redis: redisClient}},
		{name: "redis", deps: Deps{Repo: &fakeRepo{}, Cleaner: &fakeCleaner{}}},
	}
	for _, tc := range cases {
		if _, err := newWorker(tc.deps); err == nil {
			t.Fatalf("newWorker(%s) = nil error，want failure", tc.name)
		}
	}
}

// TestSweepRedisFailureSkips：Redis 不可用时抢锁失败，整轮跳过且不碰
// 仓储。锁是正确性的必要条件（多副本互斥），不能在无锁状态下「尽力执行」。
func TestSweepRedisFailureSkips(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	mr.Close() // 模拟 Redis 运行时故障。

	repo := &fakeRepo{expireAffected: 1}
	w, err := newWorker(Deps{Repo: repo, Cleaner: &fakeCleaner{}, Redis: client, Now: fixedNow})
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}

	res, ran, err := w.sweep(context.Background())
	if err == nil || ran || res != (SweepResult{}) {
		t.Fatalf("sweep = %+v/%v/%v，want skipped with error", res, ran, err)
	}
	if expire, reset := repo.callCounts(); expire != 0 || reset != 0 {
		t.Fatalf("repo calls = %d/%d，want none when Redis is down", expire, reset)
	}
}

// TestCleanupRedisFailureSkips：清理同样在抢锁失败时跳过。
func TestCleanupRedisFailureSkips(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	mr.Close()

	cleaner := &fakeCleaner{batches: []int64{1}}
	w, err := newWorker(Deps{Repo: &fakeRepo{}, Cleaner: cleaner, Redis: client, Now: fixedNow})
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}

	deleted, ran, err := w.cleanup(context.Background())
	if err == nil || ran || deleted != 0 {
		t.Fatalf("cleanup = %d/%v/%v，want skipped with error", deleted, ran, err)
	}
	if cleaner.calls != 0 {
		t.Fatalf("cleaner calls = %d，want 0", cleaner.calls)
	}
}

// TestReleaseLockCompareDelete：只有持有者令牌能删除锁。TTL 恰在操作结束
// 前过期、另一副本已经抢到锁时，无条件 DEL 会误删别人的锁，导致真正的
// 互斥失效（两个副本同时扫描）。
func TestReleaseLockCompareDelete(t *testing.T) {
	repo := &fakeRepo{}
	cleaner := &fakeCleaner{}
	w := newTestWorker(t, repo, cleaner)
	ctx := context.Background()

	if err := w.redis.Set(ctx, SweepLockKey, "other-replica", time.Minute).Err(); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	w.releaseLock(SweepLockKey, "my-stale-token")
	got, err := w.redis.Get(ctx, SweepLockKey).Result()
	if err != nil {
		t.Fatalf("get lock after non-owner release: %v", err)
	}
	if got != "other-replica" {
		t.Fatalf("lock holder = %q，want other-replica（非持有者不得删除）", got)
	}

	// 持有者删除：令牌匹配才删除。
	w.releaseLock(SweepLockKey, "other-replica")
	if _, err := w.redis.Get(ctx, SweepLockKey).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("lock still present after owner release: %v", err)
	}
}

// TestCleanupLockContentionSkips：另一副本持有清理锁时，本轮不产生任何
// 删除调用（多副本下的预期路径，不是错误）。
func TestCleanupLockContentionSkips(t *testing.T) {
	repo := &fakeRepo{}
	cleaner := &fakeCleaner{batches: []int64{1}}
	w := newTestWorker(t, repo, cleaner)
	if err := w.redis.Set(context.Background(), CleanupLockKey, "other-replica", time.Minute).Err(); err != nil {
		t.Fatalf("seed cleanup lock: %v", err)
	}

	deleted, ran, err := w.cleanup(context.Background())
	if err != nil || ran || deleted != 0 {
		t.Fatalf("cleanup = %d/%v/%v，want skipped", deleted, ran, err)
	}
	if cleaner.calls != 0 {
		t.Fatalf("cleaner calls = %d，want 0 while another replica holds the lock", cleaner.calls)
	}
}

// TestSweepStopsMidPassOnCancel：一轮扫描处理到一半时 ctx 取消，必须停止
// 后续实例的重置（停机时不再发起新的资金写）。已完成的实例保留其效果。
func TestSweepStopsMidPassOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &fakeRepo{
		due: []store.Subscription{{ID: 21}, {ID: 22}, {ID: 23}},
	}
	repo.resetHook = cancel
	w := newTestWorker(t, repo, &fakeCleaner{})

	res, ran, err := w.sweep(ctx)
	if err != nil || !ran {
		t.Fatalf("sweep = %+v/%v/%v，want ran/nil", res, ran, err)
	}
	if res.Reset != 1 || len(repo.resetCalls) != 1 || repo.resetCalls[0] != 21 {
		t.Fatalf("reset = %d calls=%v，want only the first instance before cancel", res.Reset, repo.resetCalls)
	}
}

// TestCleanupStopsMidPassOnCancel：分批清理在 ctx 取消后不再发起下一批，
// 返回已删除数而不是继续加大停机时的 DB 负载。
func TestCleanupStopsMidPassOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleaner := &fakeCleaner{batches: []int64{500, 500, 500}}
	cleaner.onCall = cancel
	w := newTestWorker(t, &fakeRepo{}, cleaner)

	deleted, ran, err := w.cleanup(ctx)
	if err != nil || !ran || deleted != 500 {
		t.Fatalf("cleanup = %d/%v/%v，want 500/true/nil after cancel", deleted, ran, err)
	}
	if cleaner.calls != 1 {
		t.Fatalf("cleaner calls = %d，want 1 after cancel", cleaner.calls)
	}
}

// TestCleanupPartialFailureKeepsDeleted：第二批失败时，第一批已删除的行数
// 必须如实返回（日志/指标不能把已发生的删除报成 0），并停止后续批次。
func TestCleanupPartialFailureKeepsDeleted(t *testing.T) {
	cleaner := &fakeCleaner{
		batches:   []int64{500},
		err:       errors.New("db down"),
		errOnCall: 2,
	}
	w := newTestWorker(t, &fakeRepo{}, cleaner)

	deleted, ran, err := w.cleanup(context.Background())
	if err == nil || !ran || deleted != 500 {
		t.Fatalf("cleanup = %d/%v/%v，want 500/true/error", deleted, ran, err)
	}
	if cleaner.calls != 2 {
		t.Fatalf("cleaner calls = %d，want 2（第二批失败后停止）", cleaner.calls)
	}
}
