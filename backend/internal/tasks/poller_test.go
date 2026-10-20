package tasks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
)

// 本文件验收 T6b 的轮询分类、CAS 终态结算与补偿重试（relay-06 §8/§9.4/§9.5）。
// 全部使用内存 fake + httptest，不依赖 PostgreSQL/Redis。

const pollTestPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "sample",
  name: "Sample",
  version: "1.0.0",
  author: { name: "NewTG" },
  description: { en: "Sample task plugin", zh: "示例任务插件" },
  models: ["sample-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [],
  routes: [],
  protocols: {},
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" }
    }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function (resp) { return { taskId: "up_1", state: "SUBMITTED" }; },
  buildQueryRequest: function (ctx) {
    return { url: ctx.baseUrl + "/query/" + ctx.taskId, method: "GET" };
  },
  parseTaskResult: function (ctx, body) {
    var data = {};
    try { data = JSON.parse(body || "{}"); } catch (e) { data = {}; }
    return { state: data.state, progress: data.progress, taskData: data };
  }
};`

type fakePollRepo struct {
	mu       sync.Mutex
	tasks    map[int64]*Task
	channels map[int64]*ChannelTarget
	// updates 记录 CAS 后的快照，供断言状态迁移。
	updates []*Task
	// casLog 记录每次 ApplyPoll 的 from → to。
	casLog []string
}

func newFakePollRepo() *fakePollRepo {
	return &fakePollRepo{tasks: map[int64]*Task{}, channels: map[int64]*ChannelTarget{}}
}

func (f *fakePollRepo) add(t *Task) *Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := cloneTask(t)
	if copied.ID == 0 {
		copied.ID = int64(len(f.tasks) + 1)
	}
	f.tasks[copied.ID] = copied
	return cloneTask(copied)
}

func (f *fakePollRepo) ListPending(_ context.Context, limit int) ([]*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Task, 0)
	for _, t := range f.tasks {
		if !t.State.Terminal() {
			out = append(out, cloneTask(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakePollRepo) ListReconcilable(_ context.Context, limit int) ([]*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Task, 0)
	for _, t := range f.tasks {
		if !t.State.Terminal() {
			continue
		}
		if boolFlag(t.PrivateData["billing_pending"]) || boolFlag(t.PrivateData["settlement_pending"]) ||
			boolFlag(t.PrivateData["refund_pending"]) {
			out = append(out, cloneTask(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakePollRepo) GetChannel(_ context.Context, id int64) (*ChannelTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	if !ok {
		return nil, ErrChannelNotFound
	}
	copied := *ch
	return &copied, nil
}

func (f *fakePollRepo) ApplyPoll(_ context.Context, id int64, from State, u PollUpdate) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok || t.State != from {
		return false, nil
	}
	t.State = u.State
	t.RawState = u.RawState
	t.ProgressPercent = u.Progress
	t.Data = mergeMaps(t.Data, u.Data)
	t.PrivateData = mergeMaps(t.PrivateData, u.Private)
	if u.FailReason != nil {
		t.FailReason = *u.FailReason
	}
	if u.FinishTime != nil {
		t.FinishTime = u.FinishTime
	}
	if u.Quota != nil {
		t.Quota = *u.Quota
	}
	f.casLog = append(f.casLog, string(from)+"->"+string(u.State))
	f.updates = append(f.updates, cloneTask(t))
	return true, nil
}

func (f *fakePollRepo) UpdateBilling(_ context.Context, id int64, quota *int64, private map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return fmt.Errorf("task %d missing", id)
	}
	if quota != nil {
		t.Quota = *quota
	}
	t.PrivateData = mergeMaps(t.PrivateData, private)
	return nil
}

func (f *fakePollRepo) get(id int64) *Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneTask(f.tasks[id])
}

func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	copied := *t
	copied.Data = mergeMaps(nil, t.Data)
	copied.PrivateData = mergeMaps(nil, t.PrivateData)
	copied.Properties = mergeMaps(nil, t.Properties)
	return &copied
}

type fakeHandoff struct {
	mu              sync.Mutex
	refunds         []string
	terminates      []string
	refundTokens    []int64
	terminateTokens []int64
	terminateErr    error
	refundErr       error
}

func (f *fakeHandoff) TerminateByRequestIDForToken(requestID string, tokenID int64, _ contract.UsageFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminates = append(f.terminates, requestID)
	f.terminateTokens = append(f.terminateTokens, tokenID)
	return f.terminateErr
}

func (f *fakeHandoff) RefundByRequestIDForToken(requestID string, tokenID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, requestID)
	f.refundTokens = append(f.refundTokens, tokenID)
	return f.refundErr
}

func (f *fakeHandoff) refundCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refunds)
}

func (f *fakeHandoff) terminateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.terminates)
}

func compilePollPlugin(t *testing.T) *plugin.CompiledPlugin {
	t.Helper()
	compiled, err := plugin.Compile(context.Background(), pollTestPluginSource, plugin.CompileOptions{
		ExpectedKey: "sample", Layer: plugin.LayerFactory,
	})
	if err != nil {
		t.Fatalf("compile poll plugin: %v", err)
	}
	return compiled
}

// fixedSnapshotter 让测试直接喂一个已编译快照（Manager.Reload 需要 DB 版本表）。
type fixedSnapshotter struct{ snap *plugin.Snapshot }

func newTestSnapshotter(t *testing.T) *fixedSnapshotter {
	t.Helper()
	manager, err := plugin.NewManager(map[string]string{"sample": pollTestPluginSource}, nil, plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return &fixedSnapshotter{snap: manager.Snapshot()}
}

func (f *fixedSnapshotter) Snapshot() *plugin.Snapshot { return f.snap }

func (f *fixedSnapshotter) Reload(context.Context) error { return nil }

func newTestPoller(t *testing.T, repo *fakePollRepo, handoff *fakeHandoff, client *http.Client) *Poller {
	t.Helper()
	p, err := NewPoller(PollerConfig{
		Repo:         repo,
		Plugins:      newTestSnapshotter(t),
		Billing:      handoff,
		Client:       client,
		TaskGap:      0,
		AllowPrivate: true,
		Now:          func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

func submittedTask(id int64, channelID int64) *Task {
	submit := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	return &Task{
		ID:             id,
		PublicTaskID:   fmt.Sprintf("task_%032d", id),
		UpstreamTaskID: fmt.Sprintf("up_%d", id),
		Family:         "plugin",
		Action:         "generate",
		PluginKey:      "sample",
		FetchMode:      "poll",
		State:          StateSubmitted,
		UserID:         42,
		ChannelID:      channelID,
		Quota:          100,
		SubmitTime:     &submit,
		Properties:     map[string]any{"OriginModelName": "sample-model"},
		// token_id 让 token-aware 结算路径可断言（T6b 复核 H1）。
		PrivateData: map[string]any{"request_id": fmt.Sprintf("req_%d", id), "token_id": int64(9)},
		Data:        map[string]any{},
	}
}

func TestPollSuccessKeepsReservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"SUCCESS","progress":100,"artifacts":[{"key":"v1","type":"video"}],"usage":{"seconds":5000,"cost":99}}`))
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := repo.add(submittedTask(1, 7))
	handoff := &fakeHandoff{}

	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State != StateSuccess || got.FinishTime == nil || got.ProgressPercent != 100 {
		t.Fatalf("task not finalized: %#v", got)
	}
	if !boolFlag(got.PrivateData["billing_final"]) || boolFlag(got.PrivateData["settlement_pending"]) ||
		boolFlag(got.PrivateData["billing_pending"]) {
		t.Fatalf("success must be settled: private=%#v", got.PrivateData)
	}
	if got.Quota != 100 {
		t.Fatalf("quota = %d, want the untouched pre-consumed 100", got.Quota)
	}
	if handoff.refundCount() != 0 || handoff.terminateCount() != 1 {
		t.Fatalf("success must terminate at held without refund (refunds=%d terminates=%d)",
			handoff.refundCount(), handoff.terminateCount())
	}
	if handoff.terminates[0] != "req_1" || handoff.terminateTokens[0] != 9 {
		t.Fatalf("terminate used wrong request id/token: ids=%v tokens=%v", handoff.terminates, handoff.terminateTokens)
	}
	usage, _ := got.Data["usage"].(map[string]any)
	// seconds 钳到 MaxRequestTaskDurationSeconds；未声明字段（cost）不得落库。
	if usage["seconds"] != float64(contract.MaxRequestTaskDurationSeconds) {
		t.Fatalf("seconds not clamped: %#v", usage)
	}
	if _, ok := usage["cost"]; ok {
		t.Fatalf("undeclared usage field leaked into task data: %#v", usage)
	}
	arts, _ := got.Data["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("artifacts snapshot not merged: %#v", got.Data)
	}
}

func TestPoll404RefundsImmediately(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := repo.add(submittedTask(1, 7))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State != StateFailure || got.Quota != 0 {
		t.Fatalf("404 must fail and zero quota: %#v", got)
	}
	if handoff.refundCount() != 1 || !boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("refund not settled: refunds=%d private=%#v", handoff.refundCount(), got.PrivateData)
	}
	if got.FailReason != "upstream_task_not_found" {
		t.Fatalf("fail reason = %q", got.FailReason)
	}
	if len(handoff.refundTokens) != 1 || handoff.refundTokens[0] != 9 {
		t.Fatalf("refund must carry the task token id: %v", handoff.refundTokens)
	}
}

func TestPollAuthFailureDoesNotRefundOrChangeState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := repo.add(submittedTask(1, 7))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State != StateSubmitted {
		t.Fatalf("401 must not change state, got %s", got.State)
	}
	if privateInt(got.PrivateData["poll_failures"]) != 1 {
		t.Fatalf("poll_failures = %v, want 1", got.PrivateData["poll_failures"])
	}
	if handoff.refundCount() != 0 {
		t.Fatalf("401 must not refund")
	}
}

func TestPollOther4xxNonTerminalAccumulatesFailure(t *testing.T) {
	// 其它 4xx 即使 body 能解析成 IN_PROGRESS，也必须累计失败（relay-06
	// §8.2）：否则持续 400 的上游可把任务拖到 1440 分钟超时，hold 被多锁。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"state":"IN_PROGRESS","progress":50}`))
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := repo.add(submittedTask(1, 7))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State == StateSuccess || got.State == StateFailure {
		t.Fatalf("non-terminal 4xx must not finalize: %s", got.State)
	}
	if privateInt(got.PrivateData["poll_failures"]) != 1 {
		t.Fatalf("poll_failures = %v, want 1", got.PrivateData["poll_failures"])
	}
	if handoff.refundCount() != 0 || handoff.terminateCount() != 0 {
		t.Fatalf("non-terminal 4xx must not settle or refund")
	}
}

func TestPollRetryableAccumulatesThenRefundsAtLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := submittedTask(1, 7)
	task.PrivateData["poll_failures"] = float64(19) // 下一次就是第 20 次
	repo.add(task)
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State != StateFailure || handoff.refundCount() != 1 {
		t.Fatalf("20th failure must fail+refund: state=%s refunds=%d", got.State, handoff.refundCount())
	}
	if got.FailReason != "poll_failures_exceeded" {
		t.Fatalf("fail reason = %q", got.FailReason)
	}
}

func TestPollTimeoutRefundsWithoutCallingUpstream(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := submittedTask(1, 7)
	old := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) // 超过 1440 分钟
	task.SubmitTime = &old
	repo.add(task)
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State != StateFailure || handoff.refundCount() != 1 || got.FailReason != "task_timeout" {
		t.Fatalf("timeout not handled: state=%s reason=%s refunds=%d", got.State, got.FailReason, handoff.refundCount())
	}
	if calls != 0 {
		t.Fatalf("timed-out task must not call upstream, got %d calls", calls)
	}
}

func TestPollUnknownCountsAsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"WHAT"}`))
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := repo.add(submittedTask(1, 7))
	p := newTestPoller(t, repo, &fakeHandoff{}, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	if got.State != StateUnknown || privateInt(got.PrivateData["poll_failures"]) != 1 {
		t.Fatalf("UNKNOWN must persist unknown with one failure: state=%s failures=%v",
			got.State, got.PrivateData["poll_failures"])
	}
}

func TestPollSerializesWithinChannelWithGap(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"IN_PROGRESS"}`))
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	repo.add(submittedTask(1, 7))
	repo.add(submittedTask(2, 7))

	var sleeps []time.Duration
	p, err := NewPoller(PollerConfig{
		Repo: repo, Plugins: newTestSnapshotter(t), Billing: &fakeHandoff{},
		Client: NewOutboundClient(), AllowPrivate: true,
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.RunOnce(context.Background())

	if len(order) != 2 || order[0] != "/query/up_1" || order[1] != "/query/up_2" {
		t.Fatalf("same-channel tasks must run serially in id order: %v", order)
	}
	if len(sleeps) != 1 || sleeps[0] != DefaultTaskGap {
		t.Fatalf("expected one 1s inter-task sleep, got %v", sleeps)
	}
}

func TestReconcileRetriesRefundPending(t *testing.T) {
	repo := newFakePollRepo()
	task := submittedTask(1, 7)
	task.State = StateFailure
	task.PrivateData["billing_pending"] = true
	repo.add(task)
	handoff := &fakeHandoff{refundErr: errors.New("wallet unavailable")}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())

	p.RunOnce(context.Background())
	got := repo.get(task.ID)
	if !boolFlag(got.PrivateData["refund_pending"]) || !boolFlag(got.PrivateData["billing_pending"]) {
		t.Fatalf("failed refund must leave refund_pending+billing_pending: %#v", got.PrivateData)
	}

	handoff.mu.Lock()
	handoff.refundErr = nil
	handoff.mu.Unlock()
	p.RunOnce(context.Background())
	got = repo.get(task.ID)
	if got.Quota != 0 || boolFlag(got.PrivateData["refund_pending"]) || !boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("reconcile did not finish the refund: quota=%d private=%#v", got.Quota, got.PrivateData)
	}
	if handoff.refundCount() != 2 {
		t.Fatalf("expected retry call, refunds=%d", handoff.refundCount())
	}
}

func TestSettleIsIdempotentOnFailure(t *testing.T) {
	repo := newFakePollRepo()
	task := submittedTask(1, 7)
	task.State = StateFailure
	repo.add(task)
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())

	live := repo.get(task.ID)
	p.settleTerminal(context.Background(), live, nil)
	p.settleTerminal(context.Background(), live, nil)

	// 幂等性由 RefundByRequestID 保证；这里断言两次调用使用同一个 requestID，
	// 且任务不会被推进到其它状态。
	if handoff.refundCount() != 2 || handoff.refunds[0] != handoff.refunds[1] {
		t.Fatalf("settle must always use the same requestID: %v", handoff.refunds)
	}
	if got := repo.get(task.ID); got.State != StateFailure || got.Quota != 0 {
		t.Fatalf("repeat settlement changed state/quota: %#v", got)
	}
}

func TestSuccessTerminateFailureMarksPendingAndRetries(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"SUCCESS"}`))
	}))
	defer upstream.Close()

	repo := newFakePollRepo()
	repo.channels[7] = &ChannelTarget{ID: 7, BaseURL: upstream.URL}
	task := repo.add(submittedTask(1, 7))
	handoff := &fakeHandoff{terminateErr: errors.New("ledger unavailable")}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())
	p.RunOnce(context.Background())

	got := repo.get(task.ID)
	// 终态不回退；settlement_pending + billing_pending 供补偿重试。
	if got.State != StateSuccess || !boolFlag(got.PrivateData["settlement_pending"]) ||
		!boolFlag(got.PrivateData["billing_pending"]) {
		t.Fatalf("failed terminate must leave settlement_pending: state=%s private=%#v", got.State, got.PrivateData)
	}
	// 同一轮 reconcile 还会再试一次（补偿），仍然失败但不会误退。
	if handoff.refundCount() != 0 {
		t.Fatalf("failed terminate must never refund a successful task")
	}

	handoff.mu.Lock()
	handoff.terminateErr = nil
	handoff.mu.Unlock()
	p.RunOnce(context.Background())
	got = repo.get(task.ID)
	if !boolFlag(got.PrivateData["billing_final"]) || boolFlag(got.PrivateData["settlement_pending"]) {
		t.Fatalf("reconcile did not finish the held settlement: %#v", got.PrivateData)
	}
}

func TestBuildTaskUsageFactUsesDeclaredUnits(t *testing.T) {
	manifest := &plugin.Manifest{
		UsageSchema: map[string]plugin.UsageField{
			"seconds": {Type: "number", Unit: "second"},
			"mode":    {Type: "enum"},
		},
	}
	fact := buildTaskUsageFact(map[string]any{"usage": map[string]any{
		"seconds": float64(5),
		"mode":    "pro",
		"cost":    float64(9),
	}}, manifest)
	if len(fact.Facts) != 2 || fact.Facts["seconds"].Num == nil || *fact.Facts["seconds"].Num != 5 {
		t.Fatalf("unexpected facts: %#v", fact.Facts)
	}
	if fact.Units["seconds"] != contract.UnitSecond {
		t.Fatalf("unit mapping wrong: %#v", fact.Units)
	}
	if _, ok := fact.Facts["mode"]; !ok || fact.Facts["mode"].Str == nil || *fact.Facts["mode"].Str != "pro" {
		t.Fatalf("enum fact missing: %#v", fact.Facts)
	}
	if _, ok := fact.Facts["cost"]; ok {
		t.Fatalf("undeclared field leaked into usage fact")
	}
}

func TestNormalizeUsageClampsAndFilters(t *testing.T) {
	manifest := &plugin.Manifest{
		UsageSchema: map[string]plugin.UsageField{
			"seconds": {Type: "number", Unit: "second"},
			"count":   {Type: "number", Unit: "count"},
			"credits": {Type: "number", Unit: "credit"},
			"mode":    {Type: "enum"},
		},
	}
	got := normalizeUsage(map[string]any{
		"seconds": float64(9000),
		"count":   float64(1000),
		"credits": float64(3.5),
		"mode":    "pro",
		"cost":    float64(99),
	}, manifest)
	if got["seconds"] != float64(3600) || got["count"] != float64(128) || got["credits"] != float64(3.5) {
		t.Fatalf("clamping wrong: %#v", got)
	}
	if got["mode"] != "pro" {
		t.Fatalf("enum field must be preserved: %#v", got)
	}
	if _, ok := got["cost"]; ok {
		t.Fatalf("undeclared field must be dropped: %#v", got)
	}
}
