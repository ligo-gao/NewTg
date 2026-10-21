package channelhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

// fakeAutoDisableStore 记录条件更新调用并返回预设结果。
type fakeAutoDisableStore struct {
	mu      sync.Mutex
	calls   []autoDisableCall
	channel *store.Channel
	changed bool
	err     error
	// release 非 nil 时 AutoDisable 阻塞到它被关闭（验证上报不阻塞）。
	release chan struct{}
	entered chan struct{}
}

type autoDisableCall struct {
	id     int64
	reason string
}

func (f *fakeAutoDisableStore) AutoDisable(ctx context.Context, id int64, reason string) (*store.Channel, bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, autoDisableCall{id: id, reason: reason})
	channel, changed, err := f.channel, f.changed, f.err
	release, entered := f.release, f.entered
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return channel, changed, err
}

func (f *fakeAutoDisableStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAutoDisableStore) lastCall() (autoDisableCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return autoDisableCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

type fakeAudit struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (f *fakeAudit) LogEvent(_ context.Context, event audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return f.err
}

func (f *fakeAudit) snapshot() []audit.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Event(nil), f.events...)
}

type fakeNotifier struct {
	mu     sync.Mutex
	inputs []notify.ChannelAutoDisabledInput
	err    error
}

func (f *fakeNotifier) DeliverChannelAutoDisabledInApp(_ context.Context, in notify.ChannelAutoDisabledInput) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	return 1, f.err
}

func (f *fakeNotifier) snapshot() []notify.ChannelAutoDisabledInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.ChannelAutoDisabledInput(nil), f.inputs...)
}

// testReporter 构造一个开关打开、关键词为默认列表的 reporter。
func testReporter(t *testing.T, storeImpl AutoDisableStore, auditImpl AuditWriter, notifierImpl AutoDisableNotifier, cfg ReporterConfig) *Reporter {
	t.Helper()
	cfg.Store = storeImpl
	cfg.Audit = auditImpl
	cfg.Notifier = notifierImpl
	if cfg.Config == nil {
		cfg.Config = mapSettings{
			KeyAutoDisableEnabled: json.RawMessage(`true`),
			KeyDisableKeywords:    json.RawMessage(`"Your credit balance is too low"`),
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	reporter, err := NewReporter(cfg)
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	return reporter
}

func disabledChannel(id int64) *store.Channel {
	return &store.Channel{
		ID:        id,
		Name:      fmt.Sprintf("channel-%d", id),
		Status:    3,
		UpdatedAt: time.Date(2026, 10, 21, 12, 0, 0, 123456000, time.UTC),
	}
}

// TestReporterDisablesAndWritesSideEffects：鉴权失败 → 条件更新（reason code）
// → 审计（system）→ 站内通知，全部在一次事件里完成。
func TestReporterDisablesAndWritesSideEffects(t *testing.T) {
	channel := disabledChannel(17)
	storeImpl := &fakeAutoDisableStore{channel: channel, changed: true}
	auditImpl := &fakeAudit{}
	notifierImpl := &fakeNotifier{}
	reporter := testReporter(t, storeImpl, auditImpl, notifierImpl, ReporterConfig{})

	reporter.ReportAttemptFailure(context.Background(), 17, true,
		contract.ErrUpstreamAuth, 401, []byte(`{"error":"invalid api key"}`))
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1", processed)
	}

	call, ok := storeImpl.lastCall()
	if !ok || call.id != 17 || call.reason != ReasonAuthError {
		t.Fatalf("AutoDisable call = %+v/%v, want {17 %s}", call, ok, ReasonAuthError)
	}
	if got := reporter.Applied(); got != 1 {
		t.Fatalf("Applied = %d, want 1", got)
	}

	events := auditImpl.snapshot()
	if len(events) != 1 {
		t.Fatalf("审计事件数 = %d, want 1", len(events))
	}
	event := events[0]
	if event.Action != audit.ActionChannelAutoDisable {
		t.Fatalf("action = %q, want %q", event.Action, audit.ActionChannelAutoDisable)
	}
	if event.Actor != 0 || event.Username != "system" {
		t.Fatalf("actor/username = %d/%q, want 0/system（R-M4-10）", event.Actor, event.Username)
	}
	var detail map[string]any
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		t.Fatalf("detail 不是合法 JSON: %v", err)
	}
	if detail["channel_id"] != float64(17) || detail["reason"] != ReasonAuthError {
		t.Fatalf("detail = %v, want channel_id=17 reason=%s", detail, ReasonAuthError)
	}
	if _, leaked := detail["base_url"]; leaked {
		t.Fatal("审计 detail 不得包含 base_url/上游地址")
	}
	var before, after map[string]any
	_ = json.Unmarshal(event.Before, &before)
	_ = json.Unmarshal(event.After, &after)
	if before["status"] != float64(1) || after["status"] != float64(3) {
		t.Fatalf("before/after status = %v/%v, want 1/3", before, after)
	}
	if after["auto_disabled_reason"] != ReasonAuthError {
		t.Fatalf("after.reason = %v, want %s", after["auto_disabled_reason"], ReasonAuthError)
	}

	inputs := notifierImpl.snapshot()
	if len(inputs) != 1 {
		t.Fatalf("通知次数 = %d, want 1", len(inputs))
	}
	if inputs[0].ChannelID != 17 || inputs[0].ReasonCode != ReasonAuthError ||
		inputs[0].ChannelName != "channel-17" {
		t.Fatalf("通知输入 = %+v", inputs[0])
	}
	wantDedupe := "channel_auto_disabled:17:" + channel.UpdatedAt.Format(time.RFC3339Nano)
	if inputs[0].DedupeKey != wantDedupe {
		t.Fatalf("dedupe key = %q, want %q", inputs[0].DedupeKey, wantDedupe)
	}
}

// TestReporterKeywordFailure：5xx + 关键词同样上报，reason=channel_keyword。
func TestReporterKeywordFailure(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(3), changed: true}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{})
	reporter.ReportAttemptFailure(context.Background(), 3, true,
		contract.ErrUpstreamServer, 500, []byte("Your Credit Balance is too low"))
	reporter.RunOnce(context.Background())
	call, _ := storeImpl.lastCall()
	if call.reason != ReasonKeyword {
		t.Fatalf("reason = %q, want %q", call.reason, ReasonKeyword)
	}
}

// TestReporterSwitchOffDoesNotCallStore：全局开关关闭 → 不判定、不入队。
func TestReporterSwitchOffDoesNotCallStore(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(1), changed: true}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{
		Config: mapSettings{KeyAutoDisableEnabled: json.RawMessage(`false`)},
	})
	reporter.ReportAttemptFailure(context.Background(), 1, true, contract.ErrUpstreamAuth, 401, nil)
	if processed := reporter.RunOnce(context.Background()); processed != 0 {
		t.Fatalf("RunOnce processed = %d, want 0", processed)
	}
	if storeImpl.callCount() != 0 {
		t.Fatal("开关关闭时不得调用 AutoDisable")
	}
}

// TestReporterAutoBanFalseDoesNotCallStore：auto_ban=false → 不判定。
func TestReporterAutoBanFalseDoesNotCallStore(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(1), changed: true}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{})
	reporter.ReportAttemptFailure(context.Background(), 1, false, contract.ErrUpstreamAuth, 403, nil)
	reporter.RunOnce(context.Background())
	if storeImpl.callCount() != 0 {
		t.Fatal("auto_ban=false 时不得调用 AutoDisable")
	}
}

// TestReporterNoChangeSkipsSideEffects：条件更新 no-op（已人工改态/并发输家）
// 时不得写审计或发通知。
func TestReporterNoChangeSkipsSideEffects(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(1), changed: false}
	auditImpl := &fakeAudit{}
	notifierImpl := &fakeNotifier{}
	reporter := testReporter(t, storeImpl, auditImpl, notifierImpl, ReporterConfig{})
	reporter.ReportAttemptFailure(context.Background(), 1, true, contract.ErrUpstreamAuth, 401, nil)
	reporter.RunOnce(context.Background())
	if len(auditImpl.snapshot()) != 0 || len(notifierImpl.snapshot()) != 0 {
		t.Fatal("changed=false 时不得写审计/通知")
	}
}

// TestReporterStoreErrorIsContained：存储失败只计数/Warn，不 panic、不触发
// 审计/通知。"失败不阻塞主响应" 的第一层保证在 ReportAttemptFailure 的非阻塞。
func TestReporterStoreErrorIsContained(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{err: errors.New("db down")}
	auditImpl := &fakeAudit{}
	notifierImpl := &fakeNotifier{}
	reporter := testReporter(t, storeImpl, auditImpl, notifierImpl, ReporterConfig{})
	reporter.ReportAttemptFailure(context.Background(), 1, true, contract.ErrUpstreamAuth, 401, nil)
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1", processed)
	}
	if reporter.Failed() != 1 || reporter.Applied() != 0 {
		t.Fatalf("Failed/Applied = %d/%d, want 1/0", reporter.Failed(), reporter.Applied())
	}
	if len(auditImpl.snapshot()) != 0 || len(notifierImpl.snapshot()) != 0 {
		t.Fatal("状态迁移失败时不得写审计/通知")
	}
}

// TestReporterSideEffectFailuresDoNotRollback：审计/通知失败只 Warn，
// 已提交的状态迁移不回滚（R-M4-09），也不得 panic。
func TestReporterSideEffectFailuresDoNotRollback(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(21), changed: true}
	auditImpl := &fakeAudit{err: errors.New("audit db down")}
	notifierImpl := &fakeNotifier{err: errors.New("notify db down")}
	reporter := testReporter(t, storeImpl, auditImpl, notifierImpl, ReporterConfig{})
	reporter.ReportAttemptFailure(context.Background(), 21, true, contract.ErrUpstreamAuth, 401, nil)
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1", processed)
	}
	if reporter.Applied() != 1 {
		t.Fatalf("Applied = %d, want 1（副作用失败不得回滚状态迁移）", reporter.Applied())
	}
	// 两者都被尝试（审计失败不阻断通知）。
	if len(auditImpl.snapshot()) != 1 || len(notifierImpl.snapshot()) != 1 {
		t.Fatalf("audit=%d notify=%d, want 1/1",
			len(auditImpl.snapshot()), len(notifierImpl.snapshot()))
	}
}

// TestReporterCooldownMergesConcurrentReports：同一渠道 100 次并发上报只入队
// 一次（冷却窗口），store 只被调用一次。真正的「只迁移一次」由条件更新保证，
// 这里锁住负载抑制行为。
func TestReporterCooldownMergesConcurrentReports(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(9), changed: true}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reporter.ReportAttemptFailure(context.Background(), 9, true, contract.ErrUpstreamAuth, 401, nil)
		}()
	}
	wg.Wait()
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1（冷却窗口应合并同渠道上报）", processed)
	}
	if got := storeImpl.callCount(); got != 1 {
		t.Fatalf("AutoDisable calls = %d, want 1", got)
	}
}

// TestReporterQueueFullDropsWithoutBlocking：队列满时立即丢弃并计数，绝不阻塞。
func TestReporterQueueFullDropsWithoutBlocking(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(1), changed: true}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{QueueSize: 1})
	reporter.ReportAttemptFailure(context.Background(), 1, true, contract.ErrUpstreamAuth, 401, nil)
	// 不同渠道绕过冷却，第二次必然面对满队列。
	reporter.ReportAttemptFailure(context.Background(), 2, true, contract.ErrUpstreamAuth, 401, nil)
	if got := reporter.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1（队列满必须丢弃而不是阻塞）", got)
	}
}

// TestReporterReportReturnsWhileStoreIsSlow：条件更新在后台执行时，
// ReportAttemptFailure 必须立即返回（请求响应不等待自动禁用副作用）。
func TestReporterReportReturnsWhileStoreIsSlow(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	storeImpl := &fakeAutoDisableStore{
		channel: disabledChannel(1),
		changed: true,
		release: release,
		entered: entered,
	}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	reporter.Start(ctx)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		unblock()
		cancel()
		reporter.Wait()
	}()

	start := time.Now()
	reporter.ReportAttemptFailure(context.Background(), 1, true, contract.ErrUpstreamAuth, 401, nil)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("ReportAttemptFailure 耗时 %v，必须非阻塞", elapsed)
	}
	// 后台已进入慢写：此时释放，证明处理链最终完成。
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("后台 goroutine 未开始处理事件")
	}
	unblock()
	// 等待处理完成（Applied 在 handle 末尾置位前先写审计/通知；这里无 side effect）。
	deadline := time.Now().Add(2 * time.Second)
	for reporter.Applied() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if reporter.Applied() != 1 {
		t.Fatalf("Applied = %d, want 1", reporter.Applied())
	}
}

// TestReporterDrainsQueueOnShutdown：I-2——ctx 取消后队列里已判定的事件
// 仍会被处理（而不是被直接丢弃），Wait 返回时排空完成。
func TestReporterDrainsQueueOnShutdown(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(31), changed: true}
	reporter := testReporter(t, storeImpl, nil, nil, ReporterConfig{})
	// 先入队，再用已取消的 ctx 启动：loop 选择 ctx.Done 后必须进入 drain。
	reporter.ReportAttemptFailure(context.Background(), 31, true, contract.ErrUpstreamAuth, 401, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reporter.Start(ctx)
	reporter.Wait()
	if got := storeImpl.callCount(); got != 1 {
		t.Fatalf("停机排空调用 AutoDisable %d 次, want 1（I-2: 直接 return 必红）", got)
	}
}

// TestReporterStartStopsOnContextCancel：Start 只起一个 goroutine，ctx 取消后
// Wait 能返回（无 goroutine 泄漏）。
func TestReporterStartStopsOnContextCancel(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{}
	reporter, err := NewReporter(ReporterConfig{
		Store:  storeImpl,
		Config: mapSettings{KeyAutoDisableEnabled: json.RawMessage(`true`)},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reporter.Start(ctx)
	reporter.Start(ctx) // 重复 Start 是 no-op
	cancel()
	done := make(chan struct{})
	go func() {
		reporter.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 reporter goroutine 未退出")
	}
}

// TestReporterProbeFailureDisablesOnThreshold：手工测试的「慢但成功」按
// R-M4-02/14 记 channel_timeout 并触发禁用（手工测试与体检共用阈值语义）。
func TestReporterProbeFailureDisablesOnThreshold(t *testing.T) {
	channel := disabledChannel(21)
	storeImpl := &fakeAutoDisableStore{channel: channel, changed: true}
	auditImpl := &fakeAudit{}
	notifierImpl := &fakeNotifier{}
	reporter := testReporter(t, storeImpl, auditImpl, notifierImpl, ReporterConfig{
		Config: mapSettings{
			KeyAutoDisableEnabled:           json.RawMessage(`true`),
			KeyResponseTimeThresholdSeconds: json.RawMessage(`5`),
		},
	})

	reporter.ReportProbeFailure(context.Background(), 21, true,
		ProbeResult{Success: true, LatencyMS: 5000})
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1（阈值边界 >= 应判失败）", processed)
	}
	call, ok := storeImpl.lastCall()
	if !ok || call.reason != ReasonTimeout {
		t.Fatalf("AutoDisable reason = %+v/%v, want %s", call, ok, ReasonTimeout)
	}
	events := auditImpl.snapshot()
	if len(events) != 1 {
		t.Fatalf("审计事件 = %d, want 1", len(events))
	}
	var detail map[string]any
	if err := json.Unmarshal(events[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["trigger"] != TriggerTimeout {
		t.Fatalf("trigger = %v, want %s", detail["trigger"], TriggerTimeout)
	}
}

// TestReporterProbeFailureSuccessNeverRecovers：成功的探测（快速或自动恢复
// 开关开启）绝不产生任何上报——手工测试只禁不禁（R-M4-15）。
func TestReporterProbeFailureSuccessNeverRecovers(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(22), changed: true}
	reporter := testReporter(t, storeImpl, &fakeAudit{}, &fakeNotifier{}, ReporterConfig{
		Config: mapSettings{
			KeyAutoDisableEnabled:           json.RawMessage(`true`),
			KeyAutoEnableEnabled:            json.RawMessage(`true`),
			KeyResponseTimeThresholdSeconds: json.RawMessage(`5`),
		},
	})

	reporter.ReportProbeFailure(context.Background(), 22, true,
		ProbeResult{Success: true, LatencyMS: 10})
	if processed := reporter.RunOnce(context.Background()); processed != 0 {
		t.Fatalf("成功探测被入队 %d 条，want 0（手工测试绝不恢复）", processed)
	}
	if storeImpl.callCount() != 0 {
		t.Fatal("成功探测不得调用 AutoDisable")
	}
}

// TestReporterProbeFailureTransportIsNotAHealthSignal：传输层错误只记
// last_test_*（handler 已完成），reporter 不入队。
func TestReporterProbeFailureTransportIsNotAHealthSignal(t *testing.T) {
	storeImpl := &fakeAutoDisableStore{channel: disabledChannel(23), changed: true}
	reporter := testReporter(t, storeImpl, &fakeAudit{}, &fakeNotifier{}, ReporterConfig{
		Config: mapSettings{KeyAutoDisableEnabled: json.RawMessage(`true`)},
	})
	reporter.ReportProbeFailure(context.Background(), 23, true,
		ProbeResult{TransportClass: TransportTimeout})
	if processed := reporter.RunOnce(context.Background()); processed != 0 {
		t.Fatalf("传输错误入队 %d 条，want 0", processed)
	}
}
