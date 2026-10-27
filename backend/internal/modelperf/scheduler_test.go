package modelperf

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
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

func newTestSchedule(t *testing.T) DailyTimes {
	t.Helper()
	sched, err := ParseTimes("00:00,12:00", shanghai(t))
	if err != nil {
		t.Fatalf("ParseTimes: %v", err)
	}
	return sched
}

// fixedScheduleFor 把静态时刻表适配成 ScheduleFor：不关心动态调度的用例
// （RunOnce、锁与时段推进）都用它保持改造前的行为。
func fixedScheduleFor(sched Schedule) func(time.Time) Schedule {
	return func(time.Time) Schedule { return sched }
}

// scheduleSource 是测试用的可变时刻表来源：ScheduleFor 每次调用都读取当前
// 值，用例可在 tick 之间替换生效时刻表（包括置 nil 验证回落）。
type scheduleSource struct {
	mu    sync.Mutex
	sched Schedule
}

func newScheduleSource(sched Schedule) *scheduleSource {
	return &scheduleSource{sched: sched}
}

func (s *scheduleSource) get(time.Time) Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sched
}

func (s *scheduleSource) set(sched Schedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched = sched
}

// roundSettingsSource 是测试用的可变 Settings 回调。
type roundSettingsSource struct {
	mu sync.Mutex
	st RoundSettings
}

func (r *roundSettingsSource) get() RoundSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

func (r *roundSettingsSource) set(st RoundSettings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st = st
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
		Tests:       tests,
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Locker:      openLocker{},
		Now:         stepClock(now),
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
		Tests:       tests,
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Locker:      openLocker{},
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
		Tests:       tests,
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Runs:        3,
		Locker:      openLocker{},
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
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
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
		Tests:       &fakeTests{},
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Locker:      openLocker{},
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
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
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
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
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
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
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
		Tests:       tests,
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Locker:      openLocker{},
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
			Models:      &fakeModels{},
			Channels:    &fakePicker{},
			Performer:   func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult { return nil },
			Tests:       &fakeTests{},
			ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
			Locker:      openLocker{},
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
		{"schedule", func(c *SchedulerConfig) { c.ScheduleFor = nil }},
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
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
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

// fakeLocker 让测试直接决定时段锁的结局，并记录每次尝试的 key 与 TTL。
// step 用例都是同步调用，但仍加锁记录：这样将来若从别的 goroutine 驱动
// step，读取记录切片也不会出现数据竞争。
type fakeLocker struct {
	mu       sync.Mutex
	acquired bool
	err      error
	keys     []string
	ttls     []time.Duration
}

func (f *fakeLocker) TryLock(_ context.Context, key string, ttl time.Duration) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	f.ttls = append(f.ttls, ttl)
	return func() {}, f.acquired, f.err
}

func (f *fakeLocker) snapshot() (keys []string, ttls []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...), append([]time.Duration(nil), f.ttls...)
}

// newStepScheduler 返回一个时钟停在 2026-10-27T11:59+08:00 的调度器，因此
// nextDue 是当天 12:00。返回的 *time.Time 是该用例时钟的唯一来源：测试改写
// 它来推进时间，step 之间不存在任何真实等待。
func newStepScheduler(t *testing.T, locker Locker) (*Scheduler, *time.Time, *atomic.Int64, *fakeTests) {
	t.Helper()
	return newStepSchedulerWith(t, locker, nil, 0)
}

func newStepSchedulerWithLogger(t *testing.T, locker Locker,
	logger *slog.Logger) (*Scheduler, *time.Time, *atomic.Int64, *fakeTests) {
	t.Helper()
	return newStepSchedulerWith(t, locker, logger, 0)
}

func newStepSchedulerWith(t *testing.T, locker Locker, logger *slog.Logger,
	tick time.Duration) (*Scheduler, *time.Time, *atomic.Int64, *fakeTests) {
	t.Helper()
	start := time.Date(2026, 10, 27, 11, 59, 0, 0, shanghai(t))
	at := &start
	calls := &atomic.Int64{}
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"gpt-4o"}},
		Channels: &fakePicker{channel: &store.Channel{ID: 7}, ok: true},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			calls.Add(1)
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:        tests,
		Runs:         1,
		Concurrency:  1,
		ScheduleFor:  fixedScheduleFor(newTestSchedule(t)),
		Locker:       locker,
		Logger:       logger,
		TickInterval: tick,
		Now:          func() time.Time { return *at },
	})
	return s, at, calls, tests
}

// atomicClock 是可并发读写的时钟：Performer 在 RunOnce 的 worker goroutine 里
// 推进它，而 step/RunOnce 在测试 goroutine 里读取，因此必须加锁。
type atomicClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *atomicClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *atomicClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// bufferLogger 返回同步写入内存 buffer 的 logger，用于断言日志契约。
// 只在日志写完后读取：step 用例是同步的，RunLoop 用例会先等待循环返回。
func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestStepRunsOnlyWhenDue(t *testing.T) {
	locker := &fakeLocker{acquired: true}
	s, at, calls, tests := newStepScheduler(t, locker)
	loc := s.schedule.Location()
	due := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	if !s.nextDue.Equal(due) {
		t.Fatalf("nextDue = %v, want %v", s.nextDue, due)
	}

	// 11:59 还不到点：不碰锁，也不探测。
	s.step(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("performer called %d times before the slot", got)
	}
	if !s.nextDue.Equal(due) {
		t.Fatalf("nextDue moved to %v before the slot", s.nextDue)
	}
	if keys, _ := locker.snapshot(); len(keys) != 0 {
		t.Fatalf("lock attempted before the slot: %v", keys)
	}

	// 到点：取锁 → 跑一轮 → nextDue 推进到次日 00:00。
	*at = due
	s.step(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("performer called %d times at the slot, want 1", got)
	}
	if got := len(tests.snapshot()); got != 1 {
		t.Fatalf("persisted %d tests at the slot, want 1", got)
	}
	wantNext := time.Date(2026, 10, 28, 0, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantNext) {
		t.Fatalf("nextDue = %v after the round, want %v", s.nextDue, wantNext)
	}
	keys, ttls := locker.snapshot()
	if len(keys) != 1 || keys[0] != "model_perf_schedule:2026-10-27T12:00" {
		t.Fatalf("lock keys = %v, want the 2026-10-27T12:00 slot", keys)
	}
	if len(ttls) != 1 || ttls[0] != 24*time.Hour {
		t.Fatalf("lock ttls = %v, want the default 24h", ttls)
	}

	// 同一时刻的重复 tick 不得重复探测：nextDue 已推进。
	s.step(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("performer called %d times after the round, want 1", got)
	}
}

func TestStepSkipsWhenLockHeld(t *testing.T) {
	logger, buf := bufferLogger()
	locker := &fakeLocker{acquired: false}
	s, at, calls, tests := newStepSchedulerWithLogger(t, locker, logger)
	loc := s.schedule.Location()
	*at = time.Date(2026, 10, 27, 12, 0, 0, 0, loc)

	s.step(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("performer called %d times although another replica holds the slot", got)
	}
	if got := len(tests.snapshot()); got != 0 {
		t.Fatalf("persisted %d tests although another replica holds the slot", got)
	}
	// 别的副本已经在跑这一轮：本副本照样把 nextDue 推进到下一个时段。
	wantNext := time.Date(2026, 10, 28, 0, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantNext) {
		t.Fatalf("nextDue = %v, want %v", s.nextDue, wantNext)
	}
	keys, _ := locker.snapshot()
	if len(keys) != 1 || keys[0] != "model_perf_schedule:2026-10-27T12:00" {
		t.Fatalf("lock keys = %v, want the 2026-10-27T12:00 slot", keys)
	}
	// 日志契约：跳过必须记为 INFO 且带 skipped_locked=true，否则运维无法区分
	// 「被别的副本抢先」和「没到点」。
	out := buf.String()
	if !strings.Contains(out, "modelperf: scheduled round skipped") ||
		!strings.Contains(out, "skipped_locked=true") ||
		!strings.Contains(out, "slot=2026-10-27T12:00:00+08:00") {
		t.Fatalf("skip log missing contract fields: %q", out)
	}
}

func TestStepRetriesOnLockErrorAndSkipsMissedSlot(t *testing.T) {
	logger, buf := bufferLogger()
	locker := &fakeLocker{acquired: true, err: errors.New("redis unavailable")}
	s, at, calls, tests := newStepSchedulerWithLogger(t, locker, logger)
	loc := s.schedule.Location()
	missedSlot := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	*at = missedSlot

	// 1) 仍在 12:00 与次日 00:00 之间：锁报错则原地重试，不推进 nextDue。
	s.step(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("performer called %d times after a lock error", got)
	}
	if !s.nextDue.Equal(missedSlot) {
		t.Fatalf("nextDue = %v after a lock error, want it to stay at %v", s.nextDue, missedSlot)
	}
	// 锁报错必须以 WARN 暴露并带上时段，否则 Redis 故障会完全静默。
	out := buf.String()
	if !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, "modelperf: acquire slot lock failed; will retry") ||
		!strings.Contains(out, "slot=2026-10-27T12:00:00+08:00") {
		t.Fatalf("lock-error WARN missing from %q", out)
	}
	// 第二个 tick 仍在同一时段内：应当再次尝试同一把锁。
	s.step(context.Background())
	if keys, _ := locker.snapshot(); len(keys) != 2 {
		t.Fatalf("lock attempted %d times, want 2 retries in the same slot", len(keys))
	}
	if !s.nextDue.Equal(missedSlot) {
		t.Fatalf("nextDue = %v on retry, want %v", s.nextDue, missedSlot)
	}
	if got := len(tests.snapshot()); got != 0 {
		t.Fatalf("persisted %d tests while the lock kept failing", got)
	}

	// 2) 时钟越过下一个时段（次日 00:00）才恢复：直接跳到未来时段，
	//    既不补跑错过的 12:00，也不执行 00:00。
	*at = time.Date(2026, 10, 28, 0, 1, 0, 0, loc)
	s.step(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("performer called %d times after missing a whole cycle, want 0", got)
	}
	wantNext := time.Date(2026, 10, 28, 12, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantNext) {
		t.Fatalf("nextDue = %v after missing a whole cycle, want %v", s.nextDue, wantNext)
	}
	if got := len(tests.snapshot()); got != 0 {
		t.Fatalf("persisted %d tests after missing a whole cycle, want 0", got)
	}
}

func TestStepLockErrorAtFollowingSlotBoundaryJumps(t *testing.T) {
	locker := &fakeLocker{acquired: true, err: errors.New("redis unavailable")}
	s, at, calls, tests := newStepScheduler(t, locker)
	loc := s.schedule.Location()
	// 时钟恰好在下一个时段（次日 00:00 整）上：边界属于「已错过」，
	// 必须跳到未来时段，而不是再等一个 tick 去补跑 00:00。
	*at = time.Date(2026, 10, 28, 0, 0, 0, 0, loc)

	s.step(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("performer called %d times at the following slot boundary", got)
	}
	if got := len(tests.snapshot()); got != 0 {
		t.Fatalf("persisted %d tests at the following slot boundary", got)
	}
	wantNext := time.Date(2026, 10, 28, 12, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantNext) {
		t.Fatalf("nextDue = %v at the following slot boundary, want %v", s.nextDue, wantNext)
	}
}

func TestStepAdvancesFromCompletionTime(t *testing.T) {
	loc := shanghai(t)
	clock := &atomicClock{at: time.Date(2026, 10, 27, 11, 59, 0, 0, loc)}
	completion := time.Date(2026, 10, 28, 0, 0, 30, 0, loc)
	var calls atomic.Int64
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"gpt-4o"}},
		Channels: &fakePicker{channel: &store.Channel{ID: 7}, ok: true},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			calls.Add(1)
			// 一轮探测耗时跨过了下一个时段（次日 00:00）。
			clock.set(completion)
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:       tests,
		Runs:        1,
		Concurrency: 1,
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Locker:      &fakeLocker{acquired: true},
		Now:         clock.now,
	})
	// 构造时 nextDue 由 11:59 算出 12:00；把时钟拨到点上再 step。
	clock.set(time.Date(2026, 10, 27, 12, 0, 0, 0, loc))

	s.step(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("performer called %d times, want 1", got)
	}
	if got := len(tests.snapshot()); got != 1 {
		t.Fatalf("persisted %d tests, want 1", got)
	}
	// 下一时段必须从「轮次结束时刻」算：12:00 的下一时段是次日 00:00，
	// 但一轮跑到 00:00:30 才结束，所以该跳到次日 12:00。
	wantNext := time.Date(2026, 10, 28, 12, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantNext) {
		t.Fatalf("nextDue = %v after a round that crossed 00:00, want %v", s.nextDue, wantNext)
	}
	// 反向防呆：若按触发时刻（slot=12:00）推进，结果会是次日 00:00，
	// 与本用例的期望不同——这正是要钉住的行为差异。
	if stale := s.schedule.Next(time.Date(2026, 10, 27, 12, 0, 0, 0, loc)); stale.Equal(wantNext) {
		t.Fatalf("test slot cannot distinguish completion-time advance from slot-time advance")
	}
}

func TestSlotKeyUsesScheduleTimezone(t *testing.T) {
	s, _, _, _ := newStepScheduler(t, &fakeLocker{acquired: true})
	loc := s.schedule.Location()
	slot := time.Date(2026, 10, 27, 0, 0, 0, 0, loc)

	const want = "model_perf_schedule:2026-10-27T00:00"
	if got := s.slotKey(slot); got != want {
		t.Fatalf("slotKey = %q, want %q", got, want)
	}
	// 同一瞬间换成 UTC 表示，key 必须不变：所有副本都按时刻表时区成键。
	if got := s.slotKey(slot.UTC()); got != want {
		t.Fatalf("slotKey(UTC instant) = %q, want %q", got, want)
	}
	// 防呆：这个瞬间在 UTC 下确实是另一天，用例才能区分两种格式化。
	if utc := "model_perf_schedule:" + slot.UTC().Format("2006-01-02T15:04"); utc == want {
		t.Fatalf("test slot does not distinguish schedule timezone from UTC: %q", utc)
	}
}

func TestRunLoopLogsStartAndReturnsNilOnCancel(t *testing.T) {
	var buf bytes.Buffer
	locker := &fakeLocker{acquired: true}
	s, _, calls, tests := newStepSchedulerWithLogger(t, locker,
		slog.New(slog.NewTextHandler(&buf, nil)))

	// 已取消的 ctx + 30s 的默认 tick：RunLoop 必须立刻返回，且不跑任何一轮。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RunLoop(ctx); err != nil {
		t.Fatalf("RunLoop = %v, want nil on cancellation", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("performer called %d times on an already-cancelled context", got)
	}
	if got := len(tests.snapshot()); got != 0 {
		t.Fatalf("persisted %d tests on an already-cancelled context", got)
	}
	out := buf.String()
	if !strings.Contains(out, "modelperf: scheduler started") {
		t.Fatalf("startup log missing from %q", out)
	}
	if !strings.Contains(out, "next_due=2026-10-27T12:00:00+08:00") {
		t.Fatalf("startup log next_due missing from %q", out)
	}
}

func TestRunLoopRunsScheduledRoundOnTick(t *testing.T) {
	loc := shanghai(t)
	at := time.Date(2026, 10, 27, 11, 59, 0, 0, loc)
	performed := make(chan struct{}, 1)
	tests := &fakeTests{}
	s := mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: []string{"gpt-4o"}},
		Channels: &fakePicker{channel: &store.Channel{ID: 7}, ok: true},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
			select {
			case performed <- struct{}{}:
			default:
			}
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:        tests,
		Runs:         1,
		Concurrency:  1,
		ScheduleFor:  fixedScheduleFor(newTestSchedule(t)),
		Locker:       &fakeLocker{acquired: true},
		TickInterval: time.Millisecond,
		Now:          func() time.Time { return at },
	})
	// 构造时 nextDue 由 11:59 算出 12:00；启动循环前把时钟拨到点上。
	at = time.Date(2026, 10, 27, 12, 0, 0, 0, loc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunLoop(ctx) }()

	// performer 的信号到达即证明 tick 路径跑完了一轮，无需 sleep 轮询。
	select {
	case <-performed:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("RunLoop did not run the due round within 5s")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLoop = %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop did not return after cancellation")
	}

	if got := len(tests.snapshot()); got != 1 {
		t.Fatalf("persisted %d tests, want 1", got)
	}
	if want := time.Date(2026, 10, 28, 0, 0, 0, 0, loc); !s.nextDue.Equal(want) {
		t.Fatalf("nextDue = %v, want %v", s.nextDue, want)
	}
}

// roundOutcome 是 OnRound 收到的一次调用，供同步的 step 用例断言。
type roundOutcome struct {
	startedAt time.Time
	stats     Stats
	err       error
}

func TestStepCallsOnRoundOnceAfterRound(t *testing.T) {
	s, at, calls, _ := newStepScheduler(t, &fakeLocker{acquired: true})
	loc := s.schedule.Location()
	var rounds []roundOutcome
	s.SetOnRound(func(_ context.Context, startedAt time.Time, stats Stats, err error) {
		// step 是同步调用，钩子与断言在同一个 goroutine 上，无需加锁。
		rounds = append(rounds, roundOutcome{startedAt: startedAt, stats: stats, err: err})
	})

	// 未到点不执行，也就不写运行记录。
	s.step(context.Background())
	if got := len(rounds); got != 0 {
		t.Fatalf("OnRound called %d times before the slot", got)
	}

	due := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	*at = due
	s.step(context.Background())

	if got := len(rounds); got != 1 {
		t.Fatalf("OnRound called %d times for one round, want exactly 1", got)
	}
	got := rounds[0]
	if !got.startedAt.Equal(due) {
		t.Fatalf("OnRound startedAt = %v, want the slot start %v", got.startedAt, due)
	}
	if got.err != nil {
		t.Fatalf("OnRound err = %v, want nil", got.err)
	}
	// 计数必须就是 RunOnce 的结果，而不是调度器另算的近似值。
	if got.stats.Tested != 1 || got.stats.Models != 1 {
		t.Fatalf("OnRound stats = %+v, want 1 tested of 1 model", got.stats)
	}
	if calls.Load() != 1 {
		t.Fatalf("performer called %d times, want 1", calls.Load())
	}
}

// TestStepStartedAtIsRoundStartNotEnd 钉住 startedAt 的取钟时机：必须在 RunOnce
// 之前读时钟，取的是轮次开始时刻而不是结束时刻，也不是时刻表上的时段。
//
// 为什么需要「可推进的时钟」：既有时钟是冻结的，startedAt/轮次结束/nextDue
// 三者恰好相等，于是把 s.now() 挪到 RunOnce 之后、或改成读 s.nextDue，断言
// 仍然全绿。这里让 performer 执行期间把时间推到未来，三种取值才真正可分。
func TestStepStartedAtIsRoundStartNotEnd(t *testing.T) {
	loc := shanghai(t)
	due := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)

	// runRound 构造一个 nextDue=12:00 的调度器，在 startAt 触发一次 step，
	// 并在 performer 执行期间把时钟推进到 advanceTo；返回 OnRound 收到的
	// startedAt。构造时先停在前一个时段，确保 nextDue 由时刻表算出。
	runRound := func(t *testing.T, startAt, advanceTo time.Time) time.Time {
		t.Helper()
		clock := &atomicClock{at: time.Date(2026, 10, 27, 11, 59, 0, 0, loc)}
		s := mustScheduler(t, SchedulerConfig{
			Models:   &fakeModels{names: []string{"gpt-4o"}},
			Channels: &fakePicker{channel: &store.Channel{ID: 7}, ok: true},
			Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult {
				// 一轮探测耗时 7 分钟：结束时刻与开始时刻必须能在记录里区分。
				clock.set(advanceTo)
				return []channelhealth.PerfRunResult{{Success: true}}
			},
			Tests:       &fakeTests{},
			Runs:        1,
			Concurrency: 1,
			ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
			Locker:      &fakeLocker{acquired: true},
			Now:         clock.now,
		})
		clock.set(startAt)

		var got []time.Time
		s.SetOnRound(func(_ context.Context, startedAt time.Time, _ Stats, _ error) {
			// step 同步调用 OnRound，与断言同 goroutine，无需加锁。
			got = append(got, startedAt)
		})
		s.step(context.Background())
		if len(got) != 1 {
			t.Fatalf("OnRound called %d times, want exactly 1", len(got))
		}
		return got[0]
	}

	t.Run("clock advanced during the round", func(t *testing.T) {
		// startedAt 必须在 RunOnce 之前取：否则运行历史会把一轮 7 分钟的探测
		// 记成「从结束时刻开始」，worker_tasks.last_run_at 也随之被推后。
		if got := runRound(t, due, due.Add(7*time.Minute)); !got.Equal(due) {
			t.Fatalf("OnRound startedAt = %v, want the round start %v (clock read after RunOnce records the end time)", got, due)
		}
	})

	t.Run("late tick uses execution time not slot", func(t *testing.T) {
		// 迟到的 tick：真实起点是 12:02，时段是 12:00。若 startedAt 改读
		// s.nextDue，记录会把这段等待时间从轮次耗时里抹掉（甚至出现负耗时）。
		late := due.Add(2 * time.Minute)
		if got := runRound(t, late, late.Add(7*time.Minute)); !got.Equal(late) {
			t.Fatalf("OnRound startedAt = %v, want the tick execution time %v (not the slot %v or the end %v)",
				got, late, due, late.Add(7*time.Minute))
		}
	})
}

func TestStepCallsOnRoundOnRoundError(t *testing.T) {
	loc := shanghai(t)
	at := time.Date(2026, 10, 27, 11, 59, 0, 0, loc)
	s := mustScheduler(t, SchedulerConfig{
		// 取模型列表失败是 RunOnce 唯一会返回 error 的路径。
		Models:      &fakeModels{err: errors.New("model list unavailable")},
		Channels:    &fakePicker{channel: &store.Channel{ID: 7}, ok: true},
		Performer:   func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult { return nil },
		Tests:       &fakeTests{},
		Locker:      &fakeLocker{acquired: true},
		ScheduleFor: fixedScheduleFor(newTestSchedule(t)),
		Now:         func() time.Time { return at },
	})
	var (
		count    atomic.Int64
		gotErr   error
		gotStats Stats
	)
	s.SetOnRound(func(_ context.Context, _ time.Time, stats Stats, err error) {
		count.Add(1)
		gotErr, gotStats = err, stats
	})

	at = time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	s.step(context.Background())

	if got := count.Load(); got != 1 {
		t.Fatalf("OnRound called %d times for a failed round, want 1", got)
	}
	if gotErr == nil {
		t.Fatal("OnRound err = nil, want the RunOnce error")
	}
	if gotStats.Tested != 0 {
		t.Fatalf("OnRound stats.Tested = %d for a failed round, want 0", gotStats.Tested)
	}
}

func TestStepDoesNotCallOnRoundWithoutExecution(t *testing.T) {
	loc := shanghai(t)
	due := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)

	cases := []struct {
		name   string
		locker Locker
		at     time.Time
	}{
		{"not due", &fakeLocker{acquired: true}, due.Add(-time.Minute)},
		{"lock error", &fakeLocker{acquired: true, err: errors.New("redis unavailable")}, due},
		{"lock held by another replica", &fakeLocker{acquired: false}, due},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, at, calls, _ := newStepScheduler(t, tc.locker)
			var count atomic.Int64
			s.SetOnRound(func(context.Context, time.Time, Stats, error) { count.Add(1) })

			*at = tc.at
			s.step(context.Background())

			if got := count.Load(); got != 0 {
				t.Fatalf("OnRound called %d times without an executed round, want 0", got)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("performer called %d times without an executed round, want 0", got)
			}
			if loc := s.schedule.Location(); tc.name == "lock held by another replica" &&
				!s.nextDue.Equal(time.Date(2026, 10, 28, 0, 0, 0, 0, loc)) {
				t.Fatalf("nextDue = %v after a skipped slot, want the following slot", s.nextDue)
			}
		})
	}
}

func TestStepOnRoundPanicKeepsScheduling(t *testing.T) {
	logger, buf := bufferLogger()
	s, at, calls, _ := newStepSchedulerWithLogger(t, &fakeLocker{acquired: true}, logger)
	loc := s.schedule.Location()
	var count atomic.Int64
	s.SetOnRound(func(context.Context, time.Time, Stats, error) {
		count.Add(1)
		panic("hook exploded")
	})

	slot := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	*at = slot
	s.step(context.Background())

	if got := calls.Load(); got != 1 {
		t.Fatalf("performer called %d times, want 1", got)
	}
	if !strings.Contains(buf.String(), "modelperf: round hook panicked") {
		t.Fatalf("hook panic not logged: %q", buf.String())
	}
	// 钩子 panic 后 nextDue 仍必须推进，否则同一时段会被无限重跑。
	wantNext := time.Date(2026, 10, 28, 0, 0, 0, 0, loc)
	if !s.nextDue.Equal(wantNext) {
		t.Fatalf("nextDue = %v after a hook panic, want %v", s.nextDue, wantNext)
	}

	// 循环还在：拨到下一个时段仍能真实执行一轮。
	*at = wantNext
	s.step(context.Background())
	if got := calls.Load(); got != 2 {
		t.Fatalf("performer called %d times after a hook panic, want 2", got)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("OnRound called %d times after a hook panic, want 2", got)
	}
}

func TestRunOnceDoesNotCallOnRound(t *testing.T) {
	s, _, calls, _ := newStepScheduler(t, openLocker{})
	var count atomic.Int64
	s.SetOnRound(func(context.Context, time.Time, Stats, error) { count.Add(1) })

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// 手动触发/测试直接调用 RunOnce，由任务框架自己写运行记录。
	if got := count.Load(); got != 0 {
		t.Fatalf("OnRound called %d times by a direct RunOnce, want 0", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("performer called %d times, want 1", calls.Load())
	}
}

func TestNextDueMatchesScheduleNext(t *testing.T) {
	s, _, _, _ := newStepScheduler(t, openLocker{})
	loc := s.schedule.Location()
	sched, err := ParseTimes("00:00,12:00", loc)
	if err != nil {
		t.Fatalf("ParseTimes: %v", err)
	}
	cases := []time.Time{
		time.Date(2026, 10, 27, 11, 59, 0, 0, loc),
		time.Date(2026, 10, 27, 12, 0, 0, 0, loc),
		// 跨日：23:30 之后是次日 00:00，不是当天更早的时段。
		time.Date(2026, 10, 27, 23, 30, 0, 0, loc),
		// 用 UTC 表示的同一瞬间必须按时刻表时区解释：16:30Z 已是次日 00:30
		// (+08)，下一次应为次日 12:00。
		time.Date(2026, 10, 27, 16, 30, 0, 0, time.UTC),
	}
	for _, now := range cases {
		if got, want := s.NextDue(now), sched.Next(now); !got.Equal(want) {
			t.Fatalf("NextDue(%v) = %v, want Schedule.Next = %v", now, got, want)
		}
	}
	if got, want := s.NextDue(cases[2]), time.Date(2026, 10, 28, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("NextDue across midnight = %v, want %v", got, want)
	}
}

// dynamicScheduler 把「可变时刻表 + 可变设置 + 可推进时钟」捆在一起，供
// 「每 tick 重新读取」的用例复用。settings 初值为启用、runs=1、并发=1；
// 时刻表初值为 12h 间隔（与每日 00:00/12:00 逐点一致）。
type dynamicScheduler struct {
	s        *Scheduler
	schedule *scheduleSource
	settings *roundSettingsSource
	clock    *atomicClock
	locker   *fakeLocker
	tests    *fakeTests
	calls    *atomic.Int64
}

func newDynamicScheduler(t *testing.T, at time.Time, models []string,
	performer Performer) *dynamicScheduler {
	t.Helper()
	loc := shanghai(t)
	d := &dynamicScheduler{
		schedule: newScheduleSource(IntervalSchedule{Hours: 12, Loc: loc}),
		settings: &roundSettingsSource{st: RoundSettings{Enabled: true, Runs: 1, Concurrency: 1}},
		clock:    &atomicClock{at: at},
		locker:   &fakeLocker{acquired: true},
		tests:    &fakeTests{},
		calls:    &atomic.Int64{},
	}
	d.s = mustScheduler(t, SchedulerConfig{
		Models:   &fakeModels{names: models},
		Channels: &fakePicker{channel: &store.Channel{ID: 7}, ok: true},
		Performer: func(ctx context.Context, ch *store.Channel, model string, runs int) []channelhealth.PerfRunResult {
			d.calls.Add(1)
			if performer != nil {
				return performer(ctx, ch, model, runs)
			}
			return []channelhealth.PerfRunResult{{Success: true}}
		},
		Tests:       d.tests,
		ScheduleFor: d.schedule.get,
		Settings:    d.settings.get,
		Locker:      d.locker,
		Now:         d.clock.now,
	})
	return d
}

func TestSchedulerRecomputesNextDueWhenScheduleChanges(t *testing.T) {
	loc := shanghai(t)
	// 06:30 时 12h 表的下一次是 12:00，1h 表的下一次是 07:00，两者可分。
	d := newDynamicScheduler(t, time.Date(2026, 10, 27, 6, 30, 0, 0, loc),
		[]string{"gpt-4o"}, nil)

	noon := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	if !d.s.nextDue.Equal(noon) {
		t.Fatalf("nextDue = %v, want %v", d.s.nextDue, noon)
	}
	// 未到点的常规 tick 不改变 nextDue。
	d.s.step(context.Background())

	// 运维把间隔从 12h 改成 1h：下一个 tick 必须按新表重算到 07:00。
	d.schedule.set(IntervalSchedule{Hours: 1, Loc: loc})
	d.s.step(context.Background())
	if want := time.Date(2026, 10, 27, 7, 0, 0, 0, loc); !d.s.nextDue.Equal(want) {
		t.Fatalf("nextDue after schedule change = %v, want %v", d.s.nextDue, want)
	}
	// 换表本身不触发探测，也不去抢锁（不回补）。
	if got := d.calls.Load(); got != 0 {
		t.Fatalf("performer called %d times on a schedule change, want 0", got)
	}
	if keys, _ := d.locker.snapshot(); len(keys) != 0 {
		t.Fatalf("lock attempted on a schedule change: %v", keys)
	}
}

func TestSchedulerSkipsWhenDisabled(t *testing.T) {
	loc := shanghai(t)
	d := newDynamicScheduler(t, time.Date(2026, 10, 27, 11, 59, 0, 0, loc),
		[]string{"gpt-4o"}, nil)
	d.settings.set(RoundSettings{Enabled: false, Runs: 1, Concurrency: 1})

	d.clock.set(time.Date(2026, 10, 27, 12, 0, 0, 0, loc))
	// 到点也不取锁、不跑。
	d.s.step(context.Background())
	if got := d.calls.Load(); got != 0 {
		t.Fatalf("performer called %d times while disabled, want 0", got)
	}
	if keys, _ := d.locker.snapshot(); len(keys) != 0 {
		t.Fatalf("lock attempted while disabled: %v", keys)
	}
	if got := len(d.tests.snapshot()); got != 0 {
		t.Fatalf("persisted %d tests while disabled, want 0", got)
	}

	// 停用期间 nextDue 仍推进：同一时刻重复 tick 不会补跑，且始终在未来。
	for i := 0; i < 3; i++ {
		d.s.step(context.Background())
		if !d.s.nextDue.After(d.clock.now()) {
			t.Fatalf("nextDue = %v is not in the future while disabled", d.s.nextDue)
		}
	}
	if want := time.Date(2026, 10, 28, 0, 0, 0, 0, loc); !d.s.nextDue.Equal(want) {
		t.Fatalf("nextDue = %v after the skipped slot, want %v", d.s.nextDue, want)
	}
}

func TestSchedulerReenableWaitsForNextSlot(t *testing.T) {
	loc := shanghai(t)
	d := newDynamicScheduler(t, time.Date(2026, 10, 27, 11, 59, 0, 0, loc),
		[]string{"gpt-4o"}, nil)
	d.settings.set(RoundSettings{Enabled: false, Runs: 1, Concurrency: 1})

	// 停用期间走完 3 个周期（12:00、次日 00:00、次日 12:00），每个时段都跳过。
	for _, slot := range []time.Time{
		time.Date(2026, 10, 27, 12, 0, 0, 0, loc),
		time.Date(2026, 10, 28, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 28, 12, 0, 0, 0, loc),
	} {
		d.clock.set(slot)
		d.s.step(context.Background())
	}
	if got := d.calls.Load(); got != 0 {
		t.Fatalf("performer called %d times while disabled", got)
	}

	// 在第三个周期上重新启用：当次 tick 不能立刻补跑。
	d.settings.set(RoundSettings{Enabled: true, Runs: 1, Concurrency: 1})
	d.s.step(context.Background())
	if got := d.calls.Load(); got != 0 {
		t.Fatalf("performer called %d times right after re-enabling, want 0", got)
	}
	if !d.s.nextDue.After(d.clock.now()) {
		t.Fatalf("nextDue = %v, want a future slot after re-enabling", d.s.nextDue)
	}

	// 到了下一个时段才真的跑。
	d.clock.set(d.s.nextDue)
	d.s.step(context.Background())
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("performer called %d times at the next slot, want 1", got)
	}
}

func TestSchedulerReadsRunsAndConcurrencyPerRound(t *testing.T) {
	loc := shanghai(t)
	models := []string{"m1", "m2", "m3"}

	var (
		mu      sync.Mutex
		gotRuns []int
		// 第二轮靠屏障钉住并发：若并发仍沿用上一轮的 1，屏障永远凑不齐，
		// 探测在超时后失败，峰值也只有 1。
		barrier  = make(chan struct{})
		arrived  atomic.Int64
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	var d *dynamicScheduler
	performer := func(_ context.Context, _ *store.Channel, _ string, runs int) []channelhealth.PerfRunResult {
		mu.Lock()
		gotRuns = append(gotRuns, runs)
		// 第一轮探测进行中就把设置改掉：RunOnce 已在轮次开始时快照，
		// 本轮剩余模型必须仍用旧值（2 次）。
		if len(gotRuns) == 1 {
			d.settings.set(RoundSettings{Enabled: true, Runs: 5, Concurrency: 2})
		}
		mu.Unlock()

		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			observed := peak.Load()
			if cur <= observed || peak.CompareAndSwap(observed, cur) {
				break
			}
		}
		if runs == 5 {
			if arrived.Add(1) == 2 {
				close(barrier)
			}
			select {
			case <-barrier:
			case <-time.After(5 * time.Second):
				t.Errorf("a probe never saw a second concurrent probe; concurrency was not re-read")
			}
		}
		return []channelhealth.PerfRunResult{{Success: true}}
	}

	d = newDynamicScheduler(t, time.Date(2026, 10, 27, 11, 59, 0, 0, loc), models, performer)
	d.settings.set(RoundSettings{Enabled: true, Runs: 2, Concurrency: 1})

	// 第一轮：runs=2、并发=1。
	d.clock.set(time.Date(2026, 10, 27, 12, 0, 0, 0, loc))
	d.s.step(context.Background())

	mu.Lock()
	firstRound := append([]int(nil), gotRuns...)
	mu.Unlock()
	if len(firstRound) != len(models) {
		t.Fatalf("round 1 performer calls = %d, want %d", len(firstRound), len(models))
	}
	for i, runs := range firstRound {
		if runs != 2 {
			t.Fatalf("round 1 call %d used runs=%d, want 2 (a mid-round settings change must not affect the running round)", i, runs)
		}
	}
	firstPeak := peak.Load()
	peak.Store(0)

	// 第二轮：step 重新读设置，runs=5、并发=2。
	d.clock.set(time.Date(2026, 10, 28, 0, 0, 0, 0, loc))
	d.s.step(context.Background())

	mu.Lock()
	all := append([]int(nil), gotRuns...)
	mu.Unlock()
	if len(all) != 2*len(models) {
		t.Fatalf("performer calls = %d after two rounds, want %d", len(all), 2*len(models))
	}
	for i, runs := range all[len(models):] {
		if runs != 5 {
			t.Fatalf("round 2 call %d used runs=%d, want 5 (settings not re-read per round)", i, runs)
		}
	}
	if firstPeak != 1 {
		t.Fatalf("round 1 peak in-flight = %d, want 1", firstPeak)
	}
	if got := peak.Load(); got != 2 {
		t.Fatalf("round 2 peak in-flight = %d, want 2 (concurrency not re-read per round)", got)
	}
}

func TestSchedulerRequiresScheduleFor(t *testing.T) {
	base := SchedulerConfig{
		Models:    &fakeModels{},
		Channels:  &fakePicker{},
		Performer: func(context.Context, *store.Channel, string, int) []channelhealth.PerfRunResult { return nil },
		Tests:     &fakeTests{},
		Locker:    openLocker{},
	}
	if _, err := NewScheduler(base); err == nil {
		t.Fatal("NewScheduler accepted a config without ScheduleFor")
	}
	base.ScheduleFor = func(time.Time) Schedule { return nil }
	if _, err := NewScheduler(base); err == nil {
		t.Fatal("NewScheduler accepted a ScheduleFor that returns a nil schedule")
	}
}

func TestSchedulerNilScheduleFallback(t *testing.T) {
	loc := shanghai(t)
	d := newDynamicScheduler(t, time.Date(2026, 10, 27, 6, 30, 0, 0, loc),
		[]string{"gpt-4o"}, nil)

	// 先让一次 tick 存下 1h 表：「沿用上一个非 nil」只有在存在上一个时才可观测。
	d.schedule.set(IntervalSchedule{Hours: 1, Loc: loc})
	d.s.step(context.Background())
	if want := time.Date(2026, 10, 27, 7, 0, 0, 0, loc); !d.s.nextDue.Equal(want) {
		t.Fatalf("nextDue after schedule switch = %v, want %v", d.s.nextDue, want)
	}

	// 之后读取器返回 nil：沿用上一个时刻表，不 panic。
	d.schedule.set(nil)
	d.s.step(context.Background())
	if got := d.calls.Load(); got != 0 {
		t.Fatalf("performer called %d times before the inherited slot, want 0", got)
	}

	d.clock.set(time.Date(2026, 10, 27, 7, 0, 0, 0, loc))
	d.s.step(context.Background())
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("performer called %d times at the inherited slot, want 1", got)
	}
	if want := time.Date(2026, 10, 27, 8, 0, 0, 0, loc); !d.s.nextDue.Equal(want) {
		t.Fatalf("nextDue = %v after a nil-schedule round, want %v", d.s.nextDue, want)
	}
	// 锁键仍按上一个时刻表成键。
	keys, _ := d.locker.snapshot()
	if len(keys) != 1 || keys[0] != "model_perf_schedule:2026-10-27T07:00" {
		t.Fatalf("lock keys = %v, want the 2026-10-27T07:00 slot", keys)
	}
	// NextDue 同样回落，不 panic。
	if got, want := d.s.NextDue(time.Date(2026, 10, 27, 7, 30, 0, 0, loc)),
		time.Date(2026, 10, 27, 8, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("NextDue with a nil schedule = %v, want the inherited %v", got, want)
	}
}

// TestNextDueUsesCurrentSchedule 钉住 NextDue 的并发安全语义：它由 HTTP
// handler goroutine 调用，必须读 step 最近一次存下的时刻表，而不是构造时的
// 快照，也不能在 ScheduleFor 返回 nil 时 panic。
func TestNextDueUsesCurrentSchedule(t *testing.T) {
	loc := shanghai(t)
	now := time.Date(2026, 10, 27, 6, 30, 0, 0, loc)
	d := newDynamicScheduler(t, now, nil, nil)

	if got, want := d.s.NextDue(now), time.Date(2026, 10, 27, 12, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("NextDue = %v, want %v", got, want)
	}

	// 换成 1h 表后走一个 tick：NextDue 必须反映新时刻表。
	d.schedule.set(IntervalSchedule{Hours: 1, Loc: loc})
	d.s.step(context.Background())
	if got, want := d.s.NextDue(now), time.Date(2026, 10, 27, 7, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("NextDue after the switch = %v, want %v", got, want)
	}

	// 读取器随后返回 nil 也不能 panic：回落到 step 存下的 1h 表。
	d.schedule.set(nil)
	if got, want := d.s.NextDue(now), time.Date(2026, 10, 27, 7, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("NextDue with a nil schedule = %v, want the stored %v", got, want)
	}
}
