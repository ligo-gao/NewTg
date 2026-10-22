package perfmetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可并发推进的测试时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// memStore 是 Store 的内存实现，用于不依赖 PostgreSQL 的单元测试。
type memStore struct {
	mu        sync.Mutex
	rows      map[string]Snapshot
	failCount int
	writeErr  error
}

func newMemStore() *memStore { return &memStore{rows: make(map[string]Snapshot)} }

func (m *memStore) UpsertBuckets(_ context.Context, snapshots []Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCount > 0 {
		m.failCount--
		if m.writeErr != nil {
			return m.writeErr
		}
		return errors.New("injected flush failure")
	}
	for _, snap := range snapshots {
		key := snapshotKey(snap.BucketStart, snap.Model, snap.Node, snap.Epoch)
		if old, ok := m.rows[key]; !ok || snap.Seq > old.Seq {
			m.rows[key] = snap
		}
	}
	return nil
}

func (m *memStore) ModelPerfBuckets(context.Context, time.Time, time.Time, string, string) ([]Bucket, time.Time, error) {
	return nil, time.Time{}, nil
}

func (m *memStore) snapshots() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.rows))
	for _, snap := range m.rows {
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BucketStart.Equal(out[j].BucketStart) {
			return out[i].Model < out[j].Model
		}
		return out[i].BucketStart.Before(out[j].BucketStart)
	})
	return out
}

func (m *memStore) setFailCount(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCount = n
}

func snapshotKey(start time.Time, model, node, epoch string) string {
	return fmt.Sprintf("%s|%s|%s|%s", start.UTC().Format(time.RFC3339), model, node, epoch)
}

func intPtr(v int) *int { return &v }

func observeSuccess(c *Collector, at time.Time, latency int, ttft *int, stream bool, completion int64) {
	c.Observe(Observation{
		Model:            "gpt-test",
		Status:           StatusSuccess,
		Stream:           stream,
		LatencyMS:        &latency,
		FirstTokenMS:     ttft,
		CompletionTokens: completion,
		ObservedAt:       at,
	})
}

func newTestCollector(t *testing.T, store Store, clock *fakeClock, interval time.Duration) *Collector {
	t.Helper()
	c, err := NewCollector(Config{
		Store:         store,
		FlushInterval: interval,
		Node:          "node-a",
		Now:           clock.Now,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c
}

func TestBucketIndex(t *testing.T) {
	cases := []struct {
		ms   int
		want int
	}{
		{0, 0}, {50, 0}, {51, 1}, {100, 1}, {101, 2},
		{30000, 8}, {30001, -1}, {-1, -1},
	}
	for _, tc := range cases {
		if got := BucketIndex(tc.ms); got != tc.want {
			t.Fatalf("BucketIndex(%d) = %d, want %d", tc.ms, got, tc.want)
		}
	}
}

func TestObserveBuildsAbsoluteSnapshot(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 0, 30, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Minute)

	observeSuccess(c, clock.Now(), 80, intPtr(40), true, 100)
	c.Observe(Observation{
		Model: "gpt-test", Status: StatusError, LatencyMS: intPtr(1200),
		ObservedAt: clock.Now(),
	})
	c.Observe(Observation{
		Model: "gpt-test", Status: StatusRefused, Stream: true,
		LatencyMS: intPtr(100), FirstTokenMS: intPtr(20),
		ObservedAt: clock.Now(),
	})
	if err := c.Flush(context.Background(), true); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	snaps := store.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snaps))
	}
	got := snaps[0]
	if got.Success != 1 || got.Error != 1 || got.Refused != 1 {
		t.Fatalf("status counts = (%d,%d,%d), want (1,1,1)", got.Success, got.Error, got.Refused)
	}
	if got.LatencyCount != 3 || got.LatencySumMS != 1380 {
		t.Fatalf("latency = (%d, %v), want (3, 1380)", got.LatencyCount, got.LatencySumMS)
	}
	// 80/100 -> (50,100] 桶；1200 -> (1000,2000] 桶。
	if got.LatencyBuckets[1] != 2 || got.LatencyBuckets[5] != 1 {
		t.Fatalf("latency buckets = %v, want bucket1=2 bucket5=1", got.LatencyBuckets)
	}
	if got.TTFTCount != 2 || got.TTFTBuckets[0] != 2 {
		t.Fatalf("ttft = (%d, %v), want (2, bucket0=2)", got.TTFTCount, got.TTFTBuckets)
	}
	if got.CompletionTokens != 100 {
		t.Fatalf("completion tokens = %d, want 100", got.CompletionTokens)
	}
	// 流式 80-40=40ms、非流式 1200ms、流式 100-20=80ms → 1.32s。
	if math.Abs(got.GenerationSeconds-1.32) > 1e-9 {
		t.Fatalf("generation seconds = %v, want 1.32", got.GenerationSeconds)
	}
	if got.Node != "node-a" || got.Epoch == "" || got.Seq != 3 {
		t.Fatalf("snapshot identity = (%q,%q,%d), want node-a/non-empty/3", got.Node, got.Epoch, got.Seq)
	}
}

func TestFlushOnlyIncludesClosedMinutes(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 2, 10, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Minute)

	base := time.Date(2026, 10, 22, 10, 0, 20, 0, time.UTC)
	observeSuccess(c, base, 100, nil, false, 1)
	observeSuccess(c, base.Add(2*time.Minute), 100, nil, false, 1) // 当前分钟

	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("Flush closed: %v", err)
	}
	if got := len(store.snapshots()); got != 1 {
		t.Fatalf("closed flush wrote %d snapshots, want 1", got)
	}
	if err := c.Flush(context.Background(), true); err != nil {
		t.Fatalf("Flush force: %v", err)
	}
	if got := len(store.snapshots()); got != 2 {
		t.Fatalf("forced flush wrote %d snapshots, want 2", got)
	}
}

func TestShutdownFlushWritesCurrentMinute(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 0, 10, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Hour)
	c.Start(context.Background())
	observeSuccess(c, clock.Now(), 50, nil, false, 7)

	c.Stop()

	snaps := store.snapshots()
	if len(snaps) != 1 || snaps[0].Success != 1 {
		t.Fatalf("shutdown flush snapshots = %+v, want one success bucket", snaps)
	}
}

func TestFlushRetriesAfterFailureAndDoesNotDoubleCount(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 1, 10, 0, time.UTC))
	store := newMemStore()
	store.setFailCount(1)
	c := newTestCollector(t, store, clock, time.Minute)
	observeSuccess(c, time.Date(2026, 10, 22, 10, 0, 20, 0, time.UTC), 100, nil, false, 1)

	if err := c.Flush(context.Background(), false); err == nil {
		t.Fatalf("Flush must return the injected error")
	}
	if got := c.FailureStreak(); got != 1 {
		t.Fatalf("failure streak = %d, want 1", got)
	}
	if got := len(store.snapshots()); got != 0 {
		t.Fatalf("failed flush must not write rows, got %d", got)
	}
	// 重试前又来了一个迟到样本：累计绝对量 +1，Seq 递增。
	observeSuccess(c, time.Date(2026, 10, 22, 10, 0, 50, 0, time.UTC), 100, nil, false, 1)
	clock.Advance(time.Minute)
	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	snaps := store.snapshots()
	if len(snaps) != 1 || snaps[0].Success != 2 || snaps[0].Seq != 2 {
		t.Fatalf("retry snapshot = %+v, want success=2 seq=2", snaps)
	}
	if got := c.FailureStreak(); got != 0 {
		t.Fatalf("failure streak after success = %d, want 0", got)
	}
}

func TestThreeFailedFlushesMarkStalled(t *testing.T) {
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	store := newMemStore()
	store.setFailCount(3)
	c := newTestCollector(t, store, clock, time.Minute)
	observeSuccess(c, start.Add(-30*time.Second), 100, nil, false, 1)

	for i := 0; i < 3; i++ {
		clock.Advance(time.Minute)
		if err := c.Flush(context.Background(), false); err == nil {
			t.Fatalf("flush %d must fail", i+1)
		}
	}
	if !c.Stalled() {
		t.Fatalf("3 consecutive failures must mark the pipeline stalled")
	}
	if got := c.FailureStreak(); got != 3 {
		t.Fatalf("failure streak = %d, want 3", got)
	}
	// 读面必须立即暴露停摆：返回 ReasonPipelineStalled，让 handler 进入闸门。
	_, _, queryErr := c.ModelPerfBuckets(context.Background(), start, clock.Now(), "1m", "")
	var unavailable *UnavailableError
	if !errors.As(queryErr, &unavailable) || unavailable.Reason != ReasonPipelineStalled {
		t.Fatalf("3× failure query error = %v, want reason %q", queryErr, ReasonPipelineStalled)
	}
	// 恢复后一次成功 flush 必须清除停摆标记。
	clock.Advance(time.Minute)
	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if c.Stalled() {
		t.Fatalf("successful flush must clear stalled state")
	}
}

func TestLateObservationRewritesCumulativeSnapshot(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 1, 10, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Minute)
	at := time.Date(2026, 10, 22, 10, 0, 20, 0, time.UTC)
	observeSuccess(c, at, 100, nil, false, 1)
	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("first Flush: %v", err)
	}
	observeSuccess(c, at.Add(20*time.Second), 100, nil, false, 1)
	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	snaps := store.snapshots()
	if len(snaps) != 1 || snaps[0].Success != 2 || snaps[0].Seq != 2 {
		t.Fatalf("late observation snapshot = %+v, want success=2 seq=2", snaps)
	}
}

func TestConcurrentObserveAndFlush(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 0, 30, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Hour)

	const goroutines = 8
	const perGoroutine = 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				observeSuccess(c, clock.Now(), 10, nil, false, 1)
			}
		}()
	}
	// 与写入并发的 flush 不应丢样本，也不应让竞态检测器报警。
	for i := 0; i < 20; i++ {
		_ = c.Flush(context.Background(), false)
	}
	wg.Wait()
	if err := c.Flush(context.Background(), true); err != nil {
		t.Fatalf("final Flush: %v", err)
	}
	snaps := store.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1 bucket", len(snaps))
	}
	if got, want := snaps[0].Success, int64(goroutines*perGoroutine); got != want {
		t.Fatalf("success count = %d, want %d（并发采样丢失）", got, want)
	}
}

func TestObserveAfterStopIsIgnored(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 0, 10, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Minute)
	c.Stop()
	observeSuccess(c, clock.Now(), 10, nil, false, 1)
	if err := c.Flush(context.Background(), true); err != nil {
		t.Fatalf("Flush after stop: %v", err)
	}
	if got := len(store.snapshots()); got != 0 {
		t.Fatalf("observations after Stop must be ignored, got %d rows", got)
	}
}

func TestUnknownStatusIsNotSampled(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 0, 10, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Minute)
	c.Observe(Observation{Model: "m", Status: Status(3), LatencyMS: intPtr(10), ObservedAt: clock.Now()})
	if err := c.Flush(context.Background(), true); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(store.snapshots()); got != 0 {
		t.Fatalf("refunded/unknown status must not be sampled, got %d rows", got)
	}
}

func TestModelPerfBucketsUsesLastDataTime(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 10, 22, 10, 0, 10, 0, time.UTC))
	store := newMemStore()
	c := newTestCollector(t, store, clock, time.Minute)
	observeSuccess(c, clock.Now(), 10, nil, false, 1)
	clock.Advance(2 * time.Minute)
	if err := c.Flush(context.Background(), false); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_, updated, err := c.ModelPerfBuckets(context.Background(), clock.Now().Add(-time.Hour), clock.Now(), "1m", "")
	if err != nil {
		t.Fatalf("ModelPerfBuckets: %v", err)
	}
	if !updated.Equal(clock.Now()) {
		t.Fatalf("updatedAt = %v, want last data flush %v", updated, clock.Now())
	}
}

type fakeRetentionStore struct {
	*memStore
	mu    sync.Mutex
	calls int
	opts  RetentionOptions
}

func (f *fakeRetentionStore) RunRetention(_ context.Context, opts RetentionOptions) (RetentionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.opts = opts
	return RetentionResult{Locked: true}, nil
}

func (f *fakeRetentionStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestMaybeRunRetentionHonorsInterval(t *testing.T) {
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	store := &fakeRetentionStore{memStore: newMemStore()}
	c, err := NewCollector(Config{
		Store:             store,
		FlushInterval:     time.Minute,
		RetentionInterval: time.Hour,
		Now:               clock.Now,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	// 构造时 lastRetention=start：30 分钟内不应触发。
	clock.Advance(30 * time.Minute)
	c.maybeRunRetention(context.Background(), clock.Now())
	if got := store.callCount(); got != 0 {
		t.Fatalf("retention calls = %d, want 0 before interval", got)
	}
	clock.Advance(31 * time.Minute)
	c.maybeRunRetention(context.Background(), clock.Now())
	if got := store.callCount(); got != 1 {
		t.Fatalf("retention calls = %d, want 1 after interval", got)
	}
	if store.opts.MinuteRetention != MinuteRetention {
		t.Fatalf("minute retention = %v, want %v", store.opts.MinuteRetention, MinuteRetention)
	}
}

func TestUnavailableSourceCarriesReason(t *testing.T) {
	source := NewUnavailableSource(ReasonDSNMissing)
	_, _, err := source.ModelPerfBuckets(context.Background(), time.Now(), time.Now(), "1m", "")
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want UnavailableError", err)
	}
	if unavailable.Reason != ReasonDSNMissing {
		t.Fatalf("reason = %q, want %q", unavailable.Reason, ReasonDSNMissing)
	}
	if NewUnavailableSource("").reason != ReasonStorageUnavailable {
		t.Fatalf("empty reason must fall back to storage unavailable")
	}
}
