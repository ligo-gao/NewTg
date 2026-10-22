//go:build integration

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/perfmetrics"
)

// G-M6C-13 采集器 + 独立指标库仓储的端到端集成测试：
// 批量 flush、停机 flush、3× 停摆、并发与错误路径。
//
// 运行：
//
//	NEWTG_TEST_METRICS_DSN='postgres://newtg:newtg@localhost:55432/newtg_metrics?sslmode=disable' \
//	go test -tags=integration ./internal/store/ -run TestCollectorIntegration -p 1 -v

type collectorClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *collectorClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *collectorClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newIntegrationCollector(t *testing.T, store perfmetrics.Store, clock *collectorClock,
	interval time.Duration) *perfmetrics.Collector {

	t.Helper()
	c, err := perfmetrics.NewCollector(perfmetrics.Config{
		Store:         store,
		FlushInterval: interval,
		Node:          "test-collector",
		Now:           clock.Now,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c
}

func observeAt(c *perfmetrics.Collector, at time.Time, completion int64) {
	latency := 100
	c.Observe(perfmetrics.Observation{
		Model:            "integration-model",
		Status:           perfmetrics.StatusSuccess,
		LatencyMS:        &latency,
		CompletionTokens: completion,
		ObservedAt:       at,
	})
}

func TestCollectorIntegrationBatchFlushAndQuery(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	clock := &collectorClock{t: time.Date(2026, 10, 22, 10, 1, 10, 0, time.UTC)}
	c := newIntegrationCollector(t, repo, clock, time.Hour)

	base := time.Date(2026, 10, 22, 10, 0, 20, 0, time.UTC)
	observeAt(c, base, 11)
	observeAt(c, base.Add(20*time.Second), 12)

	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if c.Stalled() {
		t.Fatalf("fresh pipeline must not be stalled")
	}
	buckets, updated, err := c.ModelPerfBuckets(context.Background(),
		base.Add(-time.Minute), base.Add(time.Minute), "1m", "integration-model")
	if err != nil {
		t.Fatalf("ModelPerfBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != 2 || buckets[0].CompletionTokens != 23 {
		t.Fatalf("buckets = %+v, want one bucket success=2 completion=23", buckets)
	}
	if updated.Before(clock.Now()) {
		t.Fatalf("updatedAt = %v, want >= flush time %v", updated, clock.Now())
	}
}

func TestCollectorIntegrationShutdownFlush(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	clock := &collectorClock{t: time.Date(2026, 10, 22, 11, 0, 30, 0, time.UTC)}
	c := newIntegrationCollector(t, repo, clock, time.Hour)
	c.Start(context.Background())
	observeAt(c, clock.Now(), 7)

	c.Stop()

	buckets, _, err := c.ModelPerfBuckets(context.Background(),
		clock.Now().Add(-time.Minute), clock.Now().Add(time.Minute), "1m", "integration-model")
	if err != nil {
		t.Fatalf("ModelPerfBuckets after Stop: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != 1 {
		t.Fatalf("shutdown flush buckets = %+v, want one success", buckets)
	}
}

type flakyStore struct {
	inner perfmetrics.Store
	mu    sync.Mutex
	fails int
}

func (f *flakyStore) UpsertBuckets(ctx context.Context, snapshots []perfmetrics.Snapshot) error {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return errors.New("injected storage failure")
	}
	f.mu.Unlock()
	return f.inner.UpsertBuckets(ctx, snapshots)
}

func (f *flakyStore) ModelPerfBuckets(ctx context.Context, start, end time.Time, granularity, model string) ([]perfmetrics.Bucket, time.Time, error) {
	return f.inner.ModelPerfBuckets(ctx, start, end, granularity, model)
}

func TestCollectorIntegrationThreeFailuresStallThenRecover(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	store := &flakyStore{inner: repo, fails: 3}
	clock := &collectorClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	c := newIntegrationCollector(t, store, clock, time.Minute)
	observeAt(c, clock.Now().Add(-30*time.Second), 1)

	for i := 0; i < 3; i++ {
		clock.Advance(time.Minute)
		if err := c.Flush(context.Background(), false); err == nil {
			t.Fatalf("flush %d must fail", i+1)
		}
	}
	if !c.Stalled() {
		t.Fatalf("three consecutive storage failures must mark stalled")
	}

	clock.Advance(time.Minute)
	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if c.Stalled() {
		t.Fatalf("successful flush must clear stalled")
	}
	buckets, _, err := c.ModelPerfBuckets(context.Background(),
		clock.Now().Add(-time.Hour), clock.Now(), "1m", "integration-model")
	if err != nil {
		t.Fatalf("query after recovery: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != 1 {
		t.Fatalf("retry must persist the retained snapshot, got %+v", buckets)
	}
}

func TestCollectorIntegrationErrorPathRetainsSnapshot(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	clock := &collectorClock{t: time.Date(2026, 10, 22, 13, 0, 10, 0, time.UTC)}
	c := newIntegrationCollector(t, repo, clock, time.Minute)
	observeAt(c, clock.Now().Add(-30*time.Second), 1)

	// 关闭连接池模拟指标存储不可用；Flush 必须返回错误但保留脏条目
	// （重试落库由 TestCollectorIntegrationThreeFailuresStallThenRecover 证明）。
	_ = db.Close()
	if err := c.Flush(context.Background(), false); err == nil {
		t.Fatalf("flush against closed db must fail")
	}
	if got := c.FailureStreak(); got != 1 {
		t.Fatalf("failure streak = %d, want 1", got)
	}
	if !c.LastDataAt().IsZero() {
		t.Fatalf("failed flush must not advance lastData")
	}
}

func TestCollectorIntegrationConcurrent(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	clock := &collectorClock{t: time.Date(2026, 10, 22, 14, 0, 30, 0, time.UTC)}
	c := newIntegrationCollector(t, repo, clock, time.Hour)

	const goroutines = 4
	const perGoroutine = 250
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				observeAt(c, clock.Now(), 1)
			}
		}()
	}
	for i := 0; i < 10; i++ {
		_ = c.Flush(context.Background(), false)
	}
	wg.Wait()
	if err := c.Flush(context.Background(), true); err != nil {
		t.Fatalf("final flush: %v", err)
	}
	buckets, _, err := c.ModelPerfBuckets(context.Background(),
		clock.Now().Add(-time.Minute), clock.Now().Add(time.Minute), "1m", "integration-model")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != goroutines*perGoroutine {
		t.Fatalf("buckets = %+v, want success=%d", buckets, goroutines*perGoroutine)
	}
}
