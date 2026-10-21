package channelhealth

// W7/M4-b：健康 worker 的单元验收（fake store / fake prober / miniredis）。
//
// 覆盖 brief §3 的 worker 语义：
//   - 默认关闭的自动禁用/自动恢复开关（R-M4-01/09）；
//   - auto_disabled → enabled 的恢复与 manual_disabled 永不恢复（R-M4-09/15/16）；
//   - 无 adapter 跳过、不改 last_test_*（R-M4-06）；
//   - 阈值边界 >=（R-M4-02/14）；
//   - 锁竞争/锁错误只跳过本轮（R-M4-13）；
//   - 并发上限、停机 cancel（R-M4-13）；
//   - test_model 优先于首模型（modules-06 F-06-12）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// ---------- fakes ----------

type periodicWrite struct {
	id      int64
	at      time.Time
	success bool
}

type disableCall struct {
	id     int64
	reason string
}

// fakeHealthStore 模拟 ChannelRepo 的条件更新语义。
//
// unconditional* 仅供变异防护用例：把 fake 放宽后，worker 自身守卫是否仍
// 阻止「manual_disabled 被恢复」就变成唯一防线（真实 SQL 守卫由 store 集成
// 用例覆盖）。
type fakeHealthStore struct {
	mu       sync.Mutex
	channels []store.Channel
	scanAll  bool

	periodic     []periodicWrite
	disableCalls []disableCall
	enableCalls  []int64

	unconditionalEnable bool

	listErr     error
	periodicErr error
	now         time.Time
}

func (f *fakeHealthStore) ListForHealthCheck(context.Context) ([]store.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.Channel, 0, len(f.channels))
	for _, ch := range f.channels {
		if f.scanAll || ch.Status == 1 || ch.Status == 3 {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (f *fakeHealthStore) AutoDisable(_ context.Context, id int64, reason string) (*store.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disableCalls = append(f.disableCalls, disableCall{id: id, reason: reason})
	for i := range f.channels {
		ch := &f.channels[i]
		if ch.ID != id || ch.Status != 1 || !ch.AutoBan {
			continue
		}
		ch.Status = 3
		value := reason
		ch.AutoDisabledReason = &value
		ch.UpdatedAt = f.now
		out := *ch
		return &out, true, nil
	}
	return nil, false, nil
}

func (f *fakeHealthStore) AutoEnable(_ context.Context, id int64) (*store.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableCalls = append(f.enableCalls, id)
	for i := range f.channels {
		ch := &f.channels[i]
		if ch.ID != id {
			continue
		}
		if !f.unconditionalEnable && ch.Status != 3 {
			continue
		}
		ch.Status = 1
		ch.AutoDisabledReason = nil
		ch.FailCount = 0
		ch.UpdatedAt = f.now
		out := *ch
		return &out, true, nil
	}
	return nil, false, nil
}

func (f *fakeHealthStore) UpdatePeriodicTestResult(_ context.Context, id int64, at time.Time, success bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.periodicErr != nil {
		return f.periodicErr
	}
	f.periodic = append(f.periodic, periodicWrite{id: id, at: at, success: success})
	for i := range f.channels {
		if f.channels[i].ID == id {
			value := success
			f.channels[i].LastTestAt = &at
			f.channels[i].LastTestResult = &value
		}
	}
	return nil
}

func (f *fakeHealthStore) channel(t *testing.T, id int64) store.Channel {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.channels {
		if ch.ID == id {
			return ch
		}
	}
	t.Fatalf("channel %d not found", id)
	return store.Channel{}
}

func (f *fakeHealthStore) periodicFor(id int64) []periodicWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []periodicWrite
	for _, write := range f.periodic {
		if write.id == id {
			out = append(out, write)
		}
	}
	return out
}

func (f *fakeHealthStore) enableCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.enableCalls)
}

type probeCall struct {
	id    int64
	model string
}

// fakeProber 按渠道 id 返回预设结果。
type fakeProber struct {
	mu       sync.Mutex
	results  map[int64]ProbeResult
	fallback ProbeResult
	calls    []probeCall
}

func (f *fakeProber) Probe(_ context.Context, ch *store.Channel, model string) ProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, probeCall{id: ch.ID, model: model})
	if result, ok := f.results[ch.ID]; ok {
		return result
	}
	return f.fallback
}

func (f *fakeProber) callModels() []probeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]probeCall(nil), f.calls...)
}

// fakeLocker 是 RoundLocker 的确定性实现。
type fakeLocker struct {
	acquired bool
	err      error
	calls    int32
	ttl      time.Duration
	releases int32
}

func (f *fakeLocker) TryLock(_ context.Context, _ string, ttl time.Duration) (func(), bool, error) {
	atomic.AddInt32(&f.calls, 1)
	f.ttl = ttl
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.acquired {
		return nil, false, nil
	}
	return func() { atomic.AddInt32(&f.releases, 1) }, true, nil
}

// ---------- helpers ----------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func workerChannel(id int64, status int16, autoBan bool, models string) store.Channel {
	return store.Channel{
		ID:              id,
		Name:            "channel-" + string(rune('a'+id)),
		Type:            string(contract.ChannelTypeOpenAI),
		Status:          status,
		AutoBan:         autoBan,
		SupportedModels: []byte(models),
		UpdatedAt:       time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
	}
}

// workerSettings 构造 rl.health 设置快照；缺失的键走注册表默认。
func workerSettings(values map[string]any) mapSettings {
	out := make(mapSettings, len(values))
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		out[key] = raw
	}
	return out
}

func newWorkerForTest(t *testing.T, storeImpl WorkerStore, prober ProbeRunner,
	settings SettingsSource, locker RoundLocker,
	auditImpl AuditWriter, notifierImpl AutoDisableNotifier) *Worker {
	t.Helper()
	worker, err := NewWorker(WorkerConfig{
		Store:       storeImpl,
		Config:      settings,
		Prober:      prober,
		Locker:      locker,
		Audit:       auditImpl,
		Notifier:    notifierImpl,
		Logger:      discardLogger(),
		ChannelsURL: "/admin/channels",
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return worker
}

// ---------- 自动禁用 ----------

// TestWorkerDisablesOnAuth：开关开启 + auto_ban → 401 迁移为 auto_disabled，
// 写 last_test_result=false，审计 channel.auto_disable + 一条站内通知。
func TestWorkerDisablesOnAuth(t *testing.T) {
	storeImpl := &fakeHealthStore{
		channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)},
		now:      time.Date(2026, 10, 21, 12, 30, 0, 0, time.UTC),
	}
	prober := &fakeProber{fallback: ProbeResult{
		StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth, Body: []byte(`{"error":"bad key"}`),
	}}
	auditImpl := &fakeAudit{}
	notifierImpl := &fakeNotifier{}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoDisableEnabled: true}),
		&fakeLocker{acquired: true}, auditImpl, notifierImpl)

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Tested != 1 || stats.Failed != 1 || stats.Disabled != 1 {
		t.Fatalf("stats = %+v, want tested/failed/disabled 1/1/1", stats)
	}
	channel := storeImpl.channel(t, 1)
	if channel.Status != 3 || channel.AutoDisabledReason == nil || *channel.AutoDisabledReason != ReasonAuthError {
		t.Fatalf("channel = status %d reason %v", channel.Status, channel.AutoDisabledReason)
	}
	writes := storeImpl.periodicFor(1)
	if len(writes) != 1 || writes[0].success {
		t.Fatalf("periodic writes = %+v, want one failure", writes)
	}
	events := auditImpl.snapshot()
	if len(events) != 1 || events[0].Action != audit.ActionChannelAutoDisable {
		t.Fatalf("audit events = %+v, want one auto_disable", events)
	}
	if events[0].Actor != 0 || events[0].Username != "system" {
		t.Fatalf("audit actor = %d/%q, want 0/system（R-M4-10）", events[0].Actor, events[0].Username)
	}
	if inputs := notifierImpl.snapshot(); len(inputs) != 1 || inputs[0].ReasonCode != ReasonAuthError {
		t.Fatalf("notifications = %+v, want one auth reason", inputs)
	}
}

// TestWorkerSwitchOffProbesButDoesNotDisable：默认关闭时体检照常进行
// （R-M4-07 默认常开），只写 last_test_result，不改状态、无审计。
func TestWorkerSwitchOffProbesButDoesNotDisable(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	prober := &fakeProber{fallback: ProbeResult{StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth}}
	auditImpl := &fakeAudit{}
	worker := newWorkerForTest(t, storeImpl, prober, workerSettings(nil),
		&fakeLocker{acquired: true}, auditImpl, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Tested != 1 || stats.Disabled != 0 {
		t.Fatalf("stats = %+v, want tested=1 disabled=0", stats)
	}
	if channel := storeImpl.channel(t, 1); channel.Status != 1 {
		t.Fatalf("开关关闭时 status = %d, want 1", channel.Status)
	}
	if writes := storeImpl.periodicFor(1); len(writes) != 1 || writes[0].success {
		t.Fatalf("periodic writes = %+v, want one failure", writes)
	}
	if len(auditImpl.snapshot()) != 0 {
		t.Fatal("开关关闭时不得写审计")
	}
}

// TestWorkerAutoBanFalseDoesNotDisable：auto_ban=false 时即使开关开启也不禁用。
func TestWorkerAutoBanFalseDoesNotDisable(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, false, `["gpt-test"]`)}}
	prober := &fakeProber{fallback: ProbeResult{StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth}}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoDisableEnabled: true}),
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if channel := storeImpl.channel(t, 1); channel.Status != 1 {
		t.Fatalf("auto_ban=false 时 status = %d, want 1", channel.Status)
	}
}

// TestWorkerKeywordFromProbeBody：探测响应体命中关键词 → channel_keyword。
func TestWorkerKeywordFromProbeBody(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	prober := &fakeProber{fallback: ProbeResult{
		StatusCode: 500, ErrorClass: contract.ErrUpstreamServer,
		Body: []byte("YOUR CREDIT BALANCE IS TOO LOW"),
	}}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoDisableEnabled: true}),
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	channel := storeImpl.channel(t, 1)
	if channel.Status != 3 || channel.AutoDisabledReason == nil || *channel.AutoDisabledReason != ReasonKeyword {
		t.Fatalf("channel = status %d reason %v, want 3/%s",
			channel.Status, channel.AutoDisabledReason, ReasonKeyword)
	}
}

// ---------- 自动恢复 ----------

// TestWorkerAutoRecoversAutoDisabled：开关开启 + 成功探测 → enabled，
// 清 reason/fail_count，审计 channel.auto_enable，且**不发通知**。
func TestWorkerAutoRecoversAutoDisabled(t *testing.T) {
	channel := workerChannel(1, 3, true, `["gpt-test"]`)
	reason := "channel_keyword"
	channel.AutoDisabledReason = &reason
	channel.FailCount = 5
	storeImpl := &fakeHealthStore{channels: []store.Channel{channel}}
	prober := &fakeProber{fallback: ProbeResult{LatencyMS: 100, StatusCode: 200, Success: true}}
	auditImpl := &fakeAudit{}
	notifierImpl := &fakeNotifier{}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoEnableEnabled: true}),
		&fakeLocker{acquired: true}, auditImpl, notifierImpl)

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.OK != 1 || stats.Enabled != 1 {
		t.Fatalf("stats = %+v, want ok=1 enabled=1", stats)
	}
	recovered := storeImpl.channel(t, 1)
	if recovered.Status != 1 || recovered.AutoDisabledReason != nil || recovered.FailCount != 0 {
		t.Fatalf("recovered = status %d reason %v fail %d, want 1/NULL/0（R-M4-16）",
			recovered.Status, recovered.AutoDisabledReason, recovered.FailCount)
	}
	events := auditImpl.snapshot()
	if len(events) != 1 || events[0].Action != audit.ActionChannelAutoEnable {
		t.Fatalf("audit events = %+v, want one auto_enable", events)
	}
	if events[0].Actor != 0 || events[0].Username != "system" {
		t.Fatalf("audit actor = %d/%q, want 0/system（R-M4-10）", events[0].Actor, events[0].Username)
	}
	if len(notifierImpl.snapshot()) != 0 {
		t.Fatal("自动恢复不得发送禁用通知（报告 §3.2）")
	}
	if writes := storeImpl.periodicFor(1); len(writes) != 1 || !writes[0].success {
		t.Fatalf("periodic writes = %+v, want one success", writes)
	}
}

// TestWorkerAutoRecoverOffByDefault：auto_enable_enabled 缺键 → 保持禁用。
func TestWorkerAutoRecoverOffByDefault(t *testing.T) {
	channel := workerChannel(1, 3, true, `["gpt-test"]`)
	reason := "channel_auth_error"
	channel.AutoDisabledReason = &reason
	storeImpl := &fakeHealthStore{channels: []store.Channel{channel}}
	prober := &fakeProber{fallback: ProbeResult{LatencyMS: 10, StatusCode: 200, Success: true}}
	auditImpl := &fakeAudit{}
	worker := newWorkerForTest(t, storeImpl, prober, workerSettings(nil),
		&fakeLocker{acquired: true}, auditImpl, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Enabled != 0 {
		t.Fatalf("stats.Enabled = %d, want 0（默认关闭）", stats.Enabled)
	}
	if got := storeImpl.channel(t, 1); got.Status != 3 {
		t.Fatalf("status = %d, want 3（保持 auto_disabled）", got.Status)
	}
	if len(auditImpl.snapshot()) != 0 {
		t.Fatal("未迁移不得写审计")
	}
}

// TestWorkerNeverRecoversManualDisabled 是「manual_disabled 永不自动恢复」的
// worker 侧守卫：即便 scan 返回了人工禁用行（生产查询不会），成功探测也不
// 得调用 AutoEnable。fake 的 AutoEnable 被放宽为无条件迁移，因此只有
// worker 的 status==3 判断能阻止它（变异：删除该判断 → 本用例红）。
func TestWorkerNeverRecoversManualDisabled(t *testing.T) {
	storeImpl := &fakeHealthStore{
		scanAll:             true,
		unconditionalEnable: true,
		channels:            []store.Channel{workerChannel(1, 2, true, `["gpt-test"]`)},
	}
	prober := &fakeProber{fallback: ProbeResult{LatencyMS: 10, StatusCode: 200, Success: true}}
	auditImpl := &fakeAudit{}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoEnableEnabled: true}),
		&fakeLocker{acquired: true}, auditImpl, &fakeNotifier{})

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := storeImpl.channel(t, 1); got.Status != 2 {
		t.Fatalf("manual_disabled 被恢复: status = %d, want 2", got.Status)
	}
	if storeImpl.enableCallCount() != 0 {
		t.Fatal("manual_disabled 不得调用 AutoEnable")
	}
	if len(auditImpl.snapshot()) != 0 {
		t.Fatal("manual_disabled 不得产生 auto_enable 审计")
	}
}

// TestWorkerDoesNotRecoverEnabledOnSuccess：已经是 enabled 的成功探测不会
// 产生任何状态迁移（避免把「恢复正常」误写成审计噪声）。
func TestWorkerDoesNotRecoverEnabledOnSuccess(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	prober := &fakeProber{fallback: ProbeResult{LatencyMS: 10, StatusCode: 200, Success: true}}
	auditImpl := &fakeAudit{}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoEnableEnabled: true}),
		&fakeLocker{acquired: true}, auditImpl, &fakeNotifier{})

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if storeImpl.enableCallCount() != 0 || len(auditImpl.snapshot()) != 0 {
		t.Fatal("enabled 渠道的成功探测不得触发 AutoEnable/审计")
	}
}

// ---------- 跳过 / 阈值 / 传输错误 ----------

// TestWorkerUnsupportedSkipsWithoutLastTest：无 adapter 整轮跳过，不改
// last_test_*、不判失败（R-M4-06；变异：去掉跳过 → 本用例红）。
func TestWorkerUnsupportedSkipsWithoutLastTest(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	prober := &fakeProber{fallback: ProbeResult{Unsupported: true}}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoDisableEnabled: true}),
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedUnsupported != 1 || stats.Tested != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want skipped_unsupported=1 tested=0", stats)
	}
	if len(storeImpl.periodicFor(1)) != 0 {
		t.Fatal("无 adapter 的渠道不得写 last_test_*")
	}
	if got := storeImpl.channel(t, 1); got.Status != 1 || got.LastTestAt != nil {
		t.Fatalf("channel = status %d last_test_at %v, want 1/nil", got.Status, got.LastTestAt)
	}
}

// TestWorkerSkipsCredentialAndBuildFailures：凭据解封/请求构造失败也跳过，
// 不写 last_test_*（本地配置问题不是上游故障）。
func TestWorkerSkipsCredentialAndBuildFailures(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{
		workerChannel(1, 1, true, `["gpt-test"]`),
		workerChannel(2, 1, true, `["gpt-test"]`),
	}}
	prober := &fakeProber{results: map[int64]ProbeResult{
		1: {CredentialInvalid: true},
		2: {BuildFailed: true},
	}}
	worker := newWorkerForTest(t, storeImpl, prober,
		workerSettings(map[string]any{KeyAutoDisableEnabled: true}),
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedInvalid != 2 || stats.Tested != 0 {
		t.Fatalf("stats = %+v, want skipped_invalid=2 tested=0", stats)
	}
	if len(storeImpl.periodicFor(1)) != 0 || len(storeImpl.periodicFor(2)) != 0 {
		t.Fatal("凭据/构造失败不得写 last_test_*")
	}
}

// TestWorkerThresholdBoundary：耗时 == 阈值 → channel_timeout（>= 边界）；
// 耗时 < 阈值 → 保持 enabled（变异：>`/`>= 漂移 → 本用例红）。
func TestWorkerThresholdBoundary(t *testing.T) {
	const thresholdSeconds = 5
	cases := []struct {
		name       string
		latencyMS  int
		wantStatus int16
		wantReason string
	}{
		{name: "exact-threshold-fails", latencyMS: thresholdSeconds * 1000, wantStatus: 3, wantReason: ReasonTimeout},
		{name: "below-threshold-ok", latencyMS: thresholdSeconds*1000 - 1, wantStatus: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
			prober := &fakeProber{fallback: ProbeResult{
				LatencyMS: tc.latencyMS, StatusCode: 200, Success: true,
			}}
			// 关键词表必须存在但命中不了，避免默认关键词干扰阈值用例。
			settings := workerSettings(map[string]any{
				KeyAutoDisableEnabled:           true,
				KeyDisableKeywords:              []string{"never-matches"},
				KeyResponseTimeThresholdSeconds: thresholdSeconds,
			})
			worker := newWorkerForTest(t, storeImpl, prober, settings,
				&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			got := storeImpl.channel(t, 1)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got.Status, tc.wantStatus)
			}
			if tc.wantReason != "" {
				if got.AutoDisabledReason == nil || *got.AutoDisabledReason != tc.wantReason {
					t.Fatalf("reason = %v, want %s", got.AutoDisabledReason, tc.wantReason)
				}
			}
			writes := storeImpl.periodicFor(1)
			if len(writes) != 1 || writes[0].success != (tc.wantStatus == 1) {
				t.Fatalf("periodic writes = %+v, want success=%v", writes, tc.wantStatus == 1)
			}
		})
	}
}

// TestWorkerTransportErrorOnlyRecordsFailure：传输层超时/网络失败只记
// last_test_result=false，不禁用（R-M4-01 C）。
func TestWorkerTransportErrorOnlyRecordsFailure(t *testing.T) {
	for _, class := range []string{TransportTimeout, TransportNetwork} {
		t.Run(class, func(t *testing.T) {
			storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
			prober := &fakeProber{fallback: ProbeResult{TransportClass: class}}
			auditImpl := &fakeAudit{}
			worker := newWorkerForTest(t, storeImpl, prober,
				workerSettings(map[string]any{KeyAutoDisableEnabled: true}),
				&fakeLocker{acquired: true}, auditImpl, &fakeNotifier{})

			stats, err := worker.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if stats.Failed != 1 || stats.Disabled != 0 {
				t.Fatalf("stats = %+v, want failed=1 disabled=0", stats)
			}
			if got := storeImpl.channel(t, 1); got.Status != 1 {
				t.Fatalf("status = %d, want 1（传输错误不禁用）", got.Status)
			}
			if writes := storeImpl.periodicFor(1); len(writes) != 1 || writes[0].success {
				t.Fatalf("periodic writes = %+v, want one failure", writes)
			}
			if len(auditImpl.snapshot()) != 0 {
				t.Fatal("传输错误不得写审计")
			}
		})
	}
}

// ---------- 锁 / 并发 / 停机 ----------

// TestWorkerLockContentionRunsOnce：两个 worker 共享 miniredis，第一个的
// 探测被阻塞时第二个必须跳过本轮（R-M4-13；变异：去掉锁 → 两个都探测）。
func TestWorkerLockContentionRunsOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	client1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = client1.Close()
		_ = client2.Close()
	})

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	prober := &blockingProber{entered: entered, release: release}
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	settings := workerSettings(map[string]any{KeyAutoDisableEnabled: true})
	settings[KeyTestConcurrency] = rawJSON(t, 1)

	worker1 := newWorkerForTest(t, storeImpl, prober, settings,
		NewRedisLocker(client1).WithLogger(discardLogger()), &fakeAudit{}, &fakeNotifier{})
	worker2 := newWorkerForTest(t, storeImpl, prober, settings,
		NewRedisLocker(client2).WithLogger(discardLogger()), &fakeAudit{}, &fakeNotifier{})

	done := make(chan WorkerStats, 1)
	go func() {
		stats, _ := worker1.RunOnce(context.Background())
		done <- stats
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker1 未开始探测")
	}

	stats2, err := worker2.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("worker2 RunOnce: %v", err)
	}
	if !stats2.SkippedLocked || stats2.Tested != 0 {
		t.Fatalf("worker2 stats = %+v, want SkippedLocked", stats2)
	}

	close(release)
	select {
	case stats1 := <-done:
		if stats1.Tested != 1 {
			t.Fatalf("worker1 stats = %+v, want tested=1", stats1)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker1 未在释放后完成")
	}
	if calls := atomic.LoadInt32(&prober.calls); calls != 1 {
		t.Fatalf("探测次数 = %d, want 1（锁竞争只跑一轮）", calls)
	}
}

// TestWorkerLockErrorSkipsRound：Redis 不可用时本轮跳过，不发起探测、不 panic。
func TestWorkerLockErrorSkipsRound(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	prober := &fakeProber{}
	worker := newWorkerForTest(t, storeImpl, prober, workerSettings(nil),
		&fakeLocker{err: errors.New("redis down")}, &fakeAudit{}, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !stats.LockError || stats.Tested != 0 {
		t.Fatalf("stats = %+v, want LockError", stats)
	}
	if len(prober.callModels()) != 0 {
		t.Fatal("锁错误时不得探测")
	}
}

// TestWorkerLockTTLCoversInterval：锁 TTL ≥ interval（R-M4-13 的 TTL 下界）。
func TestWorkerLockTTLCoversInterval(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	locker := &fakeLocker{acquired: true}
	worker := newWorkerForTest(t, storeImpl, &fakeProber{fallback: ProbeResult{Success: true}},
		workerSettings(nil), locker, &fakeAudit{}, &fakeNotifier{})

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if locker.ttl <= 0 || locker.ttl < 10*time.Minute {
		t.Fatalf("lock ttl = %v, want >= 默认 interval（10m）", locker.ttl)
	}
	if atomic.LoadInt32(&locker.releases) != 1 {
		t.Fatalf("release 次数 = %d, want 1", locker.releases)
	}
}

// TestWorkerConcurrencyBound：并发探测数不超过 test_concurrency。
func TestWorkerConcurrencyBound(t *testing.T) {
	channels := make([]store.Channel, 0, 8)
	for i := int64(1); i <= 8; i++ {
		channels = append(channels, workerChannel(i, 1, true, `["gpt-test"]`))
	}
	storeImpl := &fakeHealthStore{channels: channels}
	prober := &concurrencyProber{sleep: 10 * time.Millisecond}
	settings := workerSettings(map[string]any{KeyTestConcurrency: 2})
	worker := newWorkerForTest(t, storeImpl, prober, settings,
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Tested != 8 {
		t.Fatalf("tested = %d, want 8", stats.Tested)
	}
	if max := prober.maxSeen(); max > 2 || max < 1 {
		t.Fatalf("最大并发 = %d, want 1..2（test_concurrency=2）", max)
	}
}

// TestWorkerCancellationStopsProbes：ctx 取消后不再发起新探测、不写结果
// （R-M4-13 的停机语义）。
func TestWorkerCancellationStopsProbes(t *testing.T) {
	channels := make([]store.Channel, 0, 8)
	for i := int64(1); i <= 8; i++ {
		channels = append(channels, workerChannel(i, 1, true, `["gpt-test"]`))
	}
	storeImpl := &fakeHealthStore{channels: channels}
	prober := newCancelProber()
	settings := workerSettings(map[string]any{KeyTestConcurrency: 1})
	worker := newWorkerForTest(t, storeImpl, prober, settings,
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = worker.RunOnce(ctx)
	}()
	select {
	case <-prober.started:
	case <-time.After(2 * time.Second):
		t.Fatal("首个探测未开始")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnce 未随 ctx 取消返回")
	}
	storeImpl.mu.Lock()
	writes := len(storeImpl.periodic)
	storeImpl.mu.Unlock()
	if writes != 0 {
		t.Fatalf("取消后仍写入 last_test_* %d 次", writes)
	}
}

// TestWorkerRunLoopWaitsOneIntervalThenStops：启动后等待一个 interval 才跑
// 第一轮；cancel 后 RunLoop 立即返回（R-M4-13）。
func TestWorkerRunLoopWaitsOneIntervalThenStops(t *testing.T) {
	storeImpl := &fakeHealthStore{channels: []store.Channel{workerChannel(1, 1, true, `["gpt-test"]`)}}
	prober := &fakeProber{fallback: ProbeResult{LatencyMS: 1, StatusCode: 200, Success: true}}
	worker, err := NewWorker(WorkerConfig{
		Store:            storeImpl,
		Config:           workerSettings(nil),
		Prober:           prober,
		Locker:           &fakeLocker{acquired: true},
		Audit:            &fakeAudit{},
		Notifier:         &fakeNotifier{},
		Logger:           discardLogger(),
		TickInterval:     10 * time.Millisecond,
		IntervalOverride: 300 * time.Millisecond,
		Jitter:           func() time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.RunLoop(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	if calls := len(prober.callModels()); calls != 0 {
		t.Fatalf("启动后 %v 内已探测 %d 次，want 0（启动等待一个 interval）", 100*time.Millisecond, calls)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(prober.callModels()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := len(prober.callModels()); calls == 0 {
		t.Fatal("等待 interval 后未执行首轮体检")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunLoop 未随 ctx 取消退出")
	}
}

// ---------- 模型选择 ----------

// TestWorkerProbeModelSelection：test_model 优先；否则 supported_models 首项；
// 两者都空 → 跳过且不写 last_test_*。
func TestWorkerProbeModelSelection(t *testing.T) {
	testModel := "test-model"
	withTestModel := workerChannel(1, 1, true, `["m1","m2"]`)
	withTestModel.TestModel = &testModel
	withoutTestModel := workerChannel(2, 1, true, `["m1","m2"]`)
	noModel := workerChannel(3, 1, true, `[]`)

	storeImpl := &fakeHealthStore{channels: []store.Channel{withTestModel, withoutTestModel, noModel}}
	prober := &fakeProber{fallback: ProbeResult{LatencyMS: 1, StatusCode: 200, Success: true}}
	worker := newWorkerForTest(t, storeImpl, prober, workerSettings(nil),
		&fakeLocker{acquired: true}, &fakeAudit{}, &fakeNotifier{})

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	calls := prober.callModels()
	models := make(map[int64]string, len(calls))
	for _, call := range calls {
		models[call.id] = call.model
	}
	if models[1] != "test-model" {
		t.Fatalf("channel 1 model = %q, want test-model", models[1])
	}
	if models[2] != "m1" {
		t.Fatalf("channel 2 model = %q, want m1（首模型）", models[2])
	}
	if _, probed := models[3]; probed {
		t.Fatal("没有可测模型的渠道不得探测")
	}
	if stats.SkippedNoModel != 1 {
		t.Fatalf("stats.SkippedNoModel = %d, want 1", stats.SkippedNoModel)
	}
	if len(storeImpl.periodicFor(3)) != 0 {
		t.Fatal("没有可测模型时不得写 last_test_*")
	}
}

// ---------- 辅助 prober ----------

// blockingProber 只阻塞第一次探测（用来证明锁竞争时第二个 worker 不会探测）。
// 后续调用立即成功：变异掉锁后第二个 worker 会立刻返回并产生 tested=1，
// 让变异以断言失败（而不是测试超时）暴露。
type blockingProber struct {
	entered chan<- struct{}
	release <-chan struct{}
	calls   int32
}

func (p *blockingProber) Probe(ctx context.Context, ch *store.Channel, _ string) ProbeResult {
	if atomic.AddInt32(&p.calls, 1) == 1 {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		<-p.release
	}
	return ProbeResult{LatencyMS: 1, StatusCode: 200, Success: true}
}

type concurrencyProber struct {
	sleep   time.Duration
	current int32
	max     int32
}

func (p *concurrencyProber) Probe(context.Context, *store.Channel, string) ProbeResult {
	now := atomic.AddInt32(&p.current, 1)
	for {
		old := atomic.LoadInt32(&p.max)
		if now <= old || atomic.CompareAndSwapInt32(&p.max, old, now) {
			break
		}
	}
	time.Sleep(p.sleep)
	atomic.AddInt32(&p.current, -1)
	return ProbeResult{LatencyMS: 1, StatusCode: 200, Success: true}
}

func (p *concurrencyProber) maxSeen() int32 { return atomic.LoadInt32(&p.max) }

type cancelProber struct {
	started chan struct{}
}

func newCancelProber() *cancelProber {
	return &cancelProber{started: make(chan struct{}, 8)}
}

func (p *cancelProber) Probe(ctx context.Context, _ *store.Channel, _ string) ProbeResult {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ProbeResult{}
}
