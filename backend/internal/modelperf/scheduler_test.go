package modelperf

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

// fakeModels 是 ModelSource 的确定性实现。
type fakeModels struct {
	names []string
	err   error
}

func (f *fakeModels) ListPublicChatModels(context.Context) ([]string, error) {
	return f.names, f.err
}

// fakePicker 是 ChannelPicker 的确定性实现；errFor 按模型注入选渠道失败。
type fakePicker struct {
	channel *store.Channel
	ok      bool
	err     error
	errFor  map[string]error
}

func (f *fakePicker) PickTop(_ context.Context, model string) (*store.Channel, bool, error) {
	if err, ok := f.errFor[model]; ok {
		return nil, false, err
	}
	return f.channel, f.ok, f.err
}

// fakeTests 记录写入的汇总行与明细行数；failFor 按模型注入 Create 失败。
// RunOnce 并发调用 Create，因此内部加锁（读断言统一走 snapshot/runCount）。
type fakeTests struct {
	mu          sync.Mutex
	created     []store.ModelPerformanceTest
	runsByModel map[string]int
	failFor     map[string]error
}

func (f *fakeTests) Create(_ context.Context, test store.ModelPerformanceTest,
	runs []store.ModelPerformanceRun) (store.ModelPerformanceTest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failFor[test.Model]; ok {
		return store.ModelPerformanceTest{}, err
	}
	if f.runsByModel == nil {
		f.runsByModel = make(map[string]int)
	}
	f.runsByModel[test.Model] = len(runs)
	f.created = append(f.created, test)
	return test, nil
}

func (f *fakeTests) snapshot() []store.ModelPerformanceTest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ModelPerformanceTest(nil), f.created...)
}

func (f *fakeTests) runCount(model string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runsByModel[model]
}

// openLocker 总是抢锁成功：RunOnce 本身不碰锁，仅为满足 NewScheduler 的必填校验。
type openLocker struct{}

func (openLocker) TryLock(context.Context, string, time.Duration) (func(), bool, error) {
	return func() {}, true, nil
}

func newTestSchedule(t *testing.T) Schedule {
	t.Helper()
	sched, err := ParseTimes("00:00,12:00", shanghai(t))
	if err != nil {
		t.Fatalf("ParseTimes: %v", err)
	}
	return sched
}

// staticClock 让 nextDue 与轮次耗时断言可确定。
func staticClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// stepClock 让 RunOnce 观测到 5s 的轮次耗时：NewScheduler 先调用一次 now
// 初始化 nextDue，RunOnce 的 start 是第二次调用，收尾是第三次；前两次返回
// start，之后返回 start+5s。
func stepClock(start time.Time) func() time.Time {
	var calls atomic.Int64
	return func() time.Time {
		if calls.Add(1) <= 2 {
			return start
		}
		return start.Add(5 * time.Second)
	}
}

func mustScheduler(t *testing.T, cfg SchedulerConfig) *Scheduler {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s, err := NewScheduler(cfg)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	return s
}

func TestRunOnceEmptyModelListIsNoop(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	var performerCalls atomic.Int64
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{},
		Channels: &fakePicker{},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			performerCalls.Add(1)
			return nil
		},
		Tests:    tests,
		Schedule: newTestSchedule(t),
		Locker:   openLocker{},
		Now:      stepClock(now),
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Duration 仍必须填写：空列表早退不能跳过计时。
	want := Stats{Duration: 5 * time.Second}
	if stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := performerCalls.Load(); got != 0 {
		t.Fatalf("performer called %d times for an empty model list", got)
	}
	if got := tests.snapshot(); len(got) != 0 {
		t.Fatalf("persisted %d tests for an empty model list", len(got))
	}
}

func TestRunOnceSkipsModelWithoutChannel(t *testing.T) {
	var performerCalls atomic.Int64
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"gpt-4o"}},
		Channels: &fakePicker{ok: false},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			performerCalls.Add(1)
			return nil
		},
		Tests:    tests,
		Schedule: newTestSchedule(t),
		Locker:   openLocker{},
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Models != 1 || stats.SkippedNoChannel != 1 || stats.Tested != 0 || stats.PersistFailed != 0 {
		t.Fatalf("stats = %+v, want models=1 skipped_no_channel=1", stats)
	}
	if got := performerCalls.Load(); got != 0 {
		t.Fatalf("performer called %d times without a channel", got)
	}
	if got := tests.snapshot(); len(got) != 0 {
		t.Fatalf("persisted %d tests without a channel", len(got))
	}
}

func TestRunOncePersistsAggregatedSuccess(t *testing.T) {
	channel := &store.Channel{ID: 9, Type: "openai"}
	tests := &fakeTests{}
	var (
		gotRuns    int
		gotChannel *store.Channel
		gotModel   string
	)
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"gpt-4o"}},
		Channels: &fakePicker{channel: channel, ok: true},
		Performer: func(_ context.Context, ch *store.Channel, model string, runs int) []channelhealth.PerfRunResult {
			gotRuns, gotChannel, gotModel = runs, ch, model
			return []channelhealth.PerfRunResult{
				{Success: true, TTFTMS: intPtr(120), LatencyMS: intPtr(800), OutputTokens: intPtr(40), TPS: floatPtr(50)},
				{Success: false, ErrorClass: "timeout"},
				{Success: false, ErrorClass: "upstream"},
			}
		},
		Tests:    tests,
		Schedule: newTestSchedule(t),
		Runs:     3,
		Locker:   openLocker{},
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if gotRuns != 3 || gotChannel != channel || gotModel != "gpt-4o" {
		t.Fatalf("performer args = (%d, %v, %q), want (3, channel 9, gpt-4o)", gotRuns, gotChannel, gotModel)
	}
	if stats.Models != 1 || stats.Tested != 1 || stats.SkippedNoChannel != 0 || stats.PersistFailed != 0 {
		t.Fatalf("stats = %+v, want models=1 tested=1", stats)
	}

	created := tests.snapshot()
	if len(created) != 1 {
		t.Fatalf("persisted %d tests, want 1", len(created))
	}
	test := created[0]
	if test.ChannelID != 9 || test.Model != "gpt-4o" {
		t.Fatalf("persisted identity = (channel %d, model %q)", test.ChannelID, test.Model)
	}
	if test.Runs != 3 {
		t.Fatalf("test.Runs = %d, want 3", test.Runs)
	}
	if got := tests.runCount("gpt-4o"); got != 3 {
		t.Fatalf("Create received %d run rows, want 3", got)
	}
	if test.CreatedBy != nil {
		t.Fatalf("created_by = %v, want nil for scheduled runs", *test.CreatedBy)
	}
	if test.SuccessCount != 1 || math.Abs(test.SuccessRate-1.0/3.0) > 1e-9 {
		t.Fatalf("success = %d/%v, want 1/%.6f", test.SuccessCount, test.SuccessRate, 1.0/3.0)
	}
	if test.TTFTMSP50 == nil || *test.TTFTMSP50 != 120 {
		t.Fatalf("ttft p50 = %v, want 120", test.TTFTMSP50)
	}
}

func TestRunOnceContinuesAfterPersistFailure(t *testing.T) {
	const bad, good = "bad-model", "good-model"
	tests := &fakeTests{failFor: map[string]error{bad: errors.New("db down")}}
	var performerCalls atomic.Int64
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{bad, good}},
		Channels: &fakePicker{channel: &store.Channel{ID: 3}, ok: true},
		Performer: func(_ context.Context, _ *store.Channel, _ string, _ int) []channelhealth.PerfRunResult {
			performerCalls.Add(1)
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:       tests,
		Schedule:    newTestSchedule(t),
		Runs:        1,
		Concurrency: 1,
		Locker:      openLocker{},
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := performerCalls.Load(); got != 2 {
		t.Fatalf("performer called %d times, want 2 (a persist failure must not abort the round)", got)
	}
	if stats.Models != 2 || stats.Tested != 1 || stats.PersistFailed != 1 {
		t.Fatalf("stats = %+v, want models=2 tested=1 persist_failed=1", stats)
	}
	created := tests.snapshot()
	if len(created) != 1 || created[0].Model != good {
		t.Fatalf("persisted = %+v, want only %q", created, good)
	}
}

func TestRunOnceReturnsModelSourceError(t *testing.T) {
	sourceErr := errors.New("catalog unavailable")
	var performerCalls atomic.Int64
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{err: sourceErr},
		Channels: &fakePicker{channel: &store.Channel{ID: 1}, ok: true},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			performerCalls.Add(1)
			return nil
		},
		Tests:    &fakeTests{},
		Schedule: newTestSchedule(t),
		Locker:   openLocker{},
	})

	stats, err := s.RunOnce(context.Background())
	if !errors.Is(err, sourceErr) {
		t.Fatalf("RunOnce error = %v, want %v", err, sourceErr)
	}
	if stats != (Stats{}) {
		t.Fatalf("stats = %+v, want zero value", stats)
	}
	if got := performerCalls.Load(); got != 0 {
		t.Fatalf("performer called %d times after a model-source error", got)
	}
}

func TestRunOnceSkipsModelOnPickError(t *testing.T) {
	const bad, good = "bad-model", "good-model"
	pickErr := errors.New("route lookup failed")
	var performerCalls atomic.Int64
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models: &fakeModels{names: []string{bad, good}},
		Channels: &fakePicker{
			channel: &store.Channel{ID: 4},
			ok:      true,
			errFor:  map[string]error{bad: pickErr},
		},
		Performer: func(_ context.Context, _ *store.Channel, _ string, _ int) []channelhealth.PerfRunResult {
			performerCalls.Add(1)
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:       tests,
		Schedule:    newTestSchedule(t),
		Runs:        1,
		Concurrency: 1,
		Locker:      openLocker{},
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := performerCalls.Load(); got != 1 {
		t.Fatalf("performer called %d times, want 1 (pick error skips only that model)", got)
	}
	if stats.Models != 2 || stats.Tested != 1 || stats.SkippedNoChannel != 0 || stats.PersistFailed != 0 {
		t.Fatalf("stats = %+v, want models=2 tested=1 (pick error is not SkippedNoChannel)", stats)
	}
	created := tests.snapshot()
	if len(created) != 1 || created[0].Model != good {
		t.Fatalf("persisted = %+v, want only %q", created, good)
	}
}

func TestRunOnceRespectsConcurrencyLimit(t *testing.T) {
	const concurrency = 2
	names := []string{"m1", "m2", "m3", "m4"}
	started := make(chan string, len(names))
	gate := make(chan struct{})

	// 峰值在飞数：进入 Performer 时 +1、退出时 -1，CAS 维护最大值。
	// 这是本用例的主要断言——正确信号量下峰值必然恰好等于 Concurrency；
	// 仅靠 started 的非阻塞检查会受 goroutine 调度时机影响（可能漏报）。
	var inFlight, peak atomic.Int64
	enter := func() {
		current := inFlight.Add(1)
		for {
			observed := peak.Load()
			if current <= observed || peak.CompareAndSwap(observed, current) {
				return
			}
		}
	}

	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: names},
		Channels: &fakePicker{channel: &store.Channel{ID: 1}, ok: true},
		Performer: func(_ context.Context, _ *store.Channel, model string, _ int) []channelhealth.PerfRunResult {
			enter()
			defer inFlight.Add(-1)
			started <- model
			// 阻塞在 gate 上模拟慢探测：槽位未释放前不应有第 3 个模型启动。
			<-gate
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:       &fakeTests{},
		Schedule:    newTestSchedule(t),
		Runs:        1,
		Concurrency: concurrency,
		Locker:      openLocker{},
	})

	type outcome struct {
		stats Stats
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		stats, err := s.RunOnce(context.Background())
		done <- outcome{stats: stats, err: err}
	}()

	for i := 0; i < concurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for model %d to start", i+1)
		}
	}
	// 次要断言：两个槽位都被占满且都在 gate 上阻塞，第 3 个模型不可能启动。
	// 它是确定性的非阻塞检查（不是基于时间的等待），但只覆盖「此刻已启动」，
	// 所以主断言仍是下面的峰值。
	select {
	case model := <-started:
		t.Fatalf("model %q started while all %d slots were held", model, concurrency)
	default:
	}

	// 宽限窗口：信号量若失效，其余模型会陆续进入 Performer 并阻塞在 gate 上。
	// 这段等待只为让错误实现「到齐」，不参与正确性判断——正确实现下第 3 个
	// 模型无论调度如何都进不来，峰值恒为 concurrency。
	extra := 0
	grace := time.After(250 * time.Millisecond)
drain:
	for extra < len(names)-concurrency {
		select {
		case <-started:
			extra++
		case <-grace:
			break drain
		}
	}

	close(gate)
	for i := 0; i < len(names)-concurrency-extra; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a queued model to start after releasing the gate")
		}
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("RunOnce: %v", out.err)
		}
		if out.stats.Models != len(names) || out.stats.Tested != len(names) {
			t.Fatalf("stats = %+v, want models=%d tested=%d", out.stats, len(names), len(names))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not finish after releasing the gate")
	}
	if got := peak.Load(); got != concurrency {
		t.Fatalf("peak in-flight = %d, want %d (semaphore not limiting)", got, concurrency)
	}
}

func TestRunOnceStopsDispatchingAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		performerCalls atomic.Int64
		cancelOnce     sync.Once
	)
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"m1", "m2", "m3", "m4"}},
		Channels: &fakePicker{channel: &store.Channel{ID: 1}, ok: true},
		Performer: func(_ context.Context, _ *store.Channel, _ string, _ int) []channelhealth.PerfRunResult {
			performerCalls.Add(1)
			cancelOnce.Do(cancel) // 第一次探测就取消：后续模型不得再派发。
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:       &fakeTests{},
		Schedule:    newTestSchedule(t),
		Runs:        1,
		Concurrency: 1,
		Locker:      openLocker{},
		Now:         stepClock(time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)),
	})

	type outcome struct {
		stats Stats
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		stats, err := s.RunOnce(ctx)
		done <- outcome{stats: stats, err: err}
	}()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("RunOnce: %v", out.err)
		}
		if out.stats.Models != 4 {
			t.Fatalf("stats.Models = %d, want 4", out.stats.Models)
		}
		if out.stats.Tested >= 4 {
			t.Fatalf("tested = %d, want fewer than 4 after cancellation", out.stats.Tested)
		}
		if out.stats.Duration != 5*time.Second {
			t.Fatalf("duration = %v, want 5s (must still be filled after cancellation)", out.stats.Duration)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce deadlocked after context cancellation")
	}

	// 取消后最多再派发一个已经通过 ctx 检查的模型，但绝不可能跑满 4 个。
	if got := performerCalls.Load(); got >= 4 {
		t.Fatalf("performer called %d times after cancellation", got)
	}
}

func TestRunOnceRecoversFromPerformerPanic(t *testing.T) {
	// 两个模型 + 并发 1：panic 的那个若泄漏信号量或漏掉 wg.Done，
	// 第二个模型就永远不会被测试，比单模型用例更能钉住 defer 顺序。
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:      &fakeModels{names: []string{"panic-model", "ok-model"}},
		Channels:    &fakePicker{channel: &store.Channel{ID: 5}, ok: true},
		Concurrency: 1,
		Performer: func(_ context.Context, _ *store.Channel, model string, _ int) []channelhealth.PerfRunResult {
			if model == "panic-model" {
				panic("boom")
			}
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:    tests,
		Schedule: newTestSchedule(t),
		Locker:   openLocker{},
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Models != 2 || stats.Tested != 1 || stats.PersistFailed != 0 {
		t.Fatalf("stats = %+v, want models=2 tested=1 (a panic is logged, not counted)", stats)
	}
	got := tests.snapshot()
	if len(got) != 1 || got[0].Model != "ok-model" {
		t.Fatalf("persisted = %+v, want only ok-model", got)
	}
}

func TestNewSchedulerRejectsMissingDependencies(t *testing.T) {
	base := func(t *testing.T) SchedulerConfig {
		return SchedulerConfig{
			Models:    &fakeModels{},
			Channels:  &fakePicker{},
			Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult { return nil },
			Tests:     &fakeTests{},
			Schedule:  newTestSchedule(t),
			Locker:    openLocker{},
		}
	}
	cases := []struct {
		name   string
		mutate func(*SchedulerConfig)
	}{
		{"models", func(c *SchedulerConfig) { c.Models = nil }},
		{"channels", func(c *SchedulerConfig) { c.Channels = nil }},
		{"performer", func(c *SchedulerConfig) { c.Performer = nil }},
		{"tests", func(c *SchedulerConfig) { c.Tests = nil }},
		{"locker", func(c *SchedulerConfig) { c.Locker = nil }},
		{"schedule", func(c *SchedulerConfig) { c.Schedule = Schedule{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base(t)
			tc.mutate(&cfg)
			if _, err := NewScheduler(cfg); err == nil {
				t.Fatalf("NewScheduler accepted a config without %s", tc.name)
			}
		})
	}
}

func TestNewSchedulerClampsAndDefaults(t *testing.T) {
	loc := shanghai(t)
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, loc)
	performer := func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult { return nil }

	cfg := SchedulerConfig{
		Models:      &fakeModels{},
		Channels:    &fakePicker{},
		Performer:   performer,
		Tests:       &fakeTests{},
		Schedule:    newTestSchedule(t),
		Runs:        99,
		Concurrency: 0,
		Locker:      openLocker{},
		Now:         staticClock(now),
	}
	s, err := NewScheduler(cfg)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if s.runs != 5 || s.concurrency != 1 {
		t.Fatalf("clamps = runs %d, concurrency %d; want 5 and 1", s.runs, s.concurrency)
	}
	if s.tickInterval != 30*time.Second {
		t.Fatalf("tickInterval = %v, want 30s", s.tickInterval)
	}
	if s.slotTTL != 24*time.Hour {
		t.Fatalf("slotTTL = %v, want 24h", s.slotTTL)
	}
	if s.logger != slog.Default() {
		t.Fatalf("logger = %v, want slog.Default()", s.logger)
	}
	wantDue := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantDue) {
		t.Fatalf("nextDue = %v, want %v", s.nextDue, wantDue)
	}

	cfg.Runs, cfg.Concurrency = -3, 99
	cfg.TickInterval, cfg.SlotTTL = time.Second, time.Hour
	s, err = NewScheduler(cfg)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if s.runs != 1 || s.concurrency != 8 {
		t.Fatalf("clamps = runs %d, concurrency %d; want 1 and 8", s.runs, s.concurrency)
	}
	if s.tickInterval != time.Second || s.slotTTL != time.Hour {
		t.Fatalf("explicit intervals not kept: tick %v, ttl %v", s.tickInterval, s.slotTTL)
	}
}
