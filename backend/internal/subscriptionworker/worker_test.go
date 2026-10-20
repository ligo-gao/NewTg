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

	expireAffected  int64
	expireErr       error
	due             []store.Subscription
	dueErr          error
	resetAffected   map[int64]int64
	advanceAffected map[int64]int64
	resetErr        error

	expireCalls  int
	resetCalls   []int64
	advanceCalls []int64

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
	if r.resetErr != nil {
		return 0, r.resetErr
	}
	if affected, ok := r.resetAffected[id]; ok {
		return affected, nil
	}
	return 1, nil
}

func (r *fakeRepo) AdvanceNextResetAt(_ context.Context, id int64, _ time.Time) (int64, error) {
	r.mu.Lock()
	r.advanceCalls = append(r.advanceCalls, id)
	r.mu.Unlock()
	if affected, ok := r.advanceAffected[id]; ok {
		return affected, nil
	}
	return 1, nil
}

func (r *fakeRepo) callCounts() (expire int, reset, advance int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expireCalls, len(r.resetCalls), len(r.advanceCalls)
}

// fakeCleaner 按脚本返回每批删除行数。
type fakeCleaner struct {
	mu      sync.Mutex
	batches []int64
	err     error
	calls   int
	cutoffs []time.Time
	limits  []int
}

func (c *fakeCleaner) DeletePreConsumeBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.cutoffs = append(c.cutoffs, cutoff)
	c.limits = append(c.limits, limit)
	if c.err != nil {
		return 0, c.err
	}
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

// TestSweepResetsAndAdvancesCursor 验证两类重置分流：
//   - 有额度（amount_total>0）→ ResetSubscriptionQuota；
//   - 无限额度（amount_total=0）→ 只推进游标，不归零 amount_used。
//
// 第二轮的 affected=0 模拟条件 UPDATE 的幂等：重复 pass 不再计数。
func TestSweepResetsAndAdvancesCursor(t *testing.T) {
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
	if res.Expired != 3 || res.Reset != 1 || res.CursorAdvanced != 1 {
		t.Fatalf("result = %+v，want expired=3 reset=1 advanced=1", res)
	}
	if len(repo.resetCalls) != 1 || repo.resetCalls[0] != 11 {
		t.Fatalf("reset calls = %v，want [11]", repo.resetCalls)
	}
	if len(repo.advanceCalls) != 1 || repo.advanceCalls[0] != 12 {
		t.Fatalf("advance calls = %v，want [12]", repo.advanceCalls)
	}

	// 同一 now 的第二轮：游标前移后条件 UPDATE 返回 0，不重复计数。
	repo.resetAffected = map[int64]int64{11: 0}
	repo.advanceAffected = map[int64]int64{12: 0}
	res, ran, err = w.sweep(context.Background())
	if err != nil || !ran {
		t.Fatalf("second sweep = %v/%v，want ran/nil", ran, err)
	}
	if res.Reset != 0 || res.CursorAdvanced != 0 {
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
	if expire, reset, advance := repo.callCounts(); expire != 1 || reset != 0 || advance != 0 {
		t.Fatalf("repo calls = %d/%d/%d，want only the lock holder to run", expire, reset, advance)
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
		if expire, _, _ := repo.callCounts(); expire > 0 {
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
	if expire, reset, advance := repo.callCounts(); expire != 0 || reset != 0 || advance != 0 {
		t.Fatalf("pre-cancelled RunLoop called repo: %d/%d/%d", expire, reset, advance)
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
