package modelperf

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
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

// fakePicker 是 ChannelPicker 的确定性实现。
type fakePicker struct {
	channel *store.Channel
	ok      bool
	err     error
}

func (f *fakePicker) PickTop(context.Context, string) (*store.Channel, bool, error) {
	return f.channel, f.ok, f.err
}

// fakeTests 记录写入的汇总行；failFor 按模型注入 Create 失败。
// RunOnce 并发调用 Create，因此内部加锁（读断言统一走 snapshot）。
type fakeTests struct {
	mu      sync.Mutex
	created []store.ModelPerformanceTest
	failFor map[string]error
}

func (f *fakeTests) Create(_ context.Context, test store.ModelPerformanceTest,
	_ []store.ModelPerformanceRun) (store.ModelPerformanceTest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failFor[test.Model]; ok {
		return store.ModelPerformanceTest{}, err
	}
	f.created = append(f.created, test)
	return test, nil
}

func (f *fakeTests) snapshot() []store.ModelPerformanceTest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ModelPerformanceTest(nil), f.created...)
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

// staticClock 让 Duration 与 nextDue 的断言可确定。
func staticClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
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
	performerCalls := 0
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{},
		Channels: &fakePicker{},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			performerCalls++
			return nil
		},
		Tests:    tests,
		Schedule: newTestSchedule(t),
		Locker:   openLocker{},
		Now:      staticClock(now),
	})

	stats, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats != (Stats{}) {
		t.Fatalf("stats = %+v, want zero value", stats)
	}
	if performerCalls != 0 {
		t.Fatalf("performer called %d times for an empty model list", performerCalls)
	}
	if got := tests.snapshot(); len(got) != 0 {
		t.Fatalf("persisted %d tests for an empty model list", len(got))
	}
}

func TestRunOnceSkipsModelWithoutChannel(t *testing.T) {
	performerCalls := 0
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"gpt-4o"}},
		Channels: &fakePicker{ok: false},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			performerCalls++
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
	if performerCalls != 0 {
		t.Fatalf("performer called %d times without a channel", performerCalls)
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
	performerCalls := 0
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{bad, good}},
		Channels: &fakePicker{channel: &store.Channel{ID: 3}, ok: true},
		Performer: func(_ context.Context, _ *store.Channel, _ string, _ int) []channelhealth.PerfRunResult {
			performerCalls++
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
	if performerCalls != 2 {
		t.Fatalf("performer called %d times, want 2 (a persist failure must not abort the round)", performerCalls)
	}
	if stats.Models != 2 || stats.Tested != 1 || stats.PersistFailed != 1 {
		t.Fatalf("stats = %+v, want models=2 tested=1 persist_failed=1", stats)
	}
	created := tests.snapshot()
	if len(created) != 1 || created[0].Model != good {
		t.Fatalf("persisted = %+v, want only %q", created, good)
	}
}

func TestRunOnceRespectsConcurrencyLimit(t *testing.T) {
	const concurrency = 2
	started := make(chan string, 4)
	gate := make(chan struct{})
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"m1", "m2", "m3", "m4"}},
		Channels: &fakePicker{channel: &store.Channel{ID: 1}, ok: true},
		Performer: func(_ context.Context, _ *store.Channel, model string, _ int) []channelhealth.PerfRunResult {
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
	// 两个槽位都被占满且都在 gate 上阻塞，第 3 个模型不可能启动——
	// 这是一次确定性的非阻塞检查，不是基于时间的等待。
	select {
	case model := <-started:
		t.Fatalf("model %q started while all %d slots were held", model, concurrency)
	default:
	}

	close(gate)
	for i := 0; i < 2; i++ {
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
		if out.stats.Models != 4 || out.stats.Tested != 4 {
			t.Fatalf("stats = %+v, want models=4 tested=4", out.stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not finish after releasing the gate")
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
