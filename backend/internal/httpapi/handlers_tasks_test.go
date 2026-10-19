package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	billingpkg "newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/tasks"
	"newtg/gateway/plugins"
)

// 本文件是任务数据面端点的单元验收：提交管线的状态码/计费顺序/凭据不泄露、
// 归属过滤与台账可见性。真实 PG 行为（索引/CAS/并发）由 store 与 tasks 的
// 集成测试覆盖；这里全部走内存 fake，不依赖外部服务。

// ---------- fakes ----------

type fakeTaskStore struct {
	mu        sync.Mutex
	rows      []*tasks.Task
	inserted  int
	insertErr error
}

func (f *fakeTaskStore) Insert(_ context.Context, t *tasks.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	copied := *t
	f.rows = append(f.rows, &copied)
	f.inserted++
	t.ID = int64(f.inserted)
	return nil
}

func (f *fakeTaskStore) GetByPublicID(_ context.Context, userID int64, publicID, pluginKey string) (*tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.UserID == userID && row.PublicTaskID == publicID && row.PluginKey == pluginKey {
			copied := *row
			return &copied, nil
		}
	}
	return nil, tasks.ErrTaskNotFound
}

func (f *fakeTaskStore) GetByPublicIDAny(_ context.Context, publicID string) (*tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.PublicTaskID == publicID {
			copied := *row
			return &copied, nil
		}
	}
	return nil, tasks.ErrTaskNotFound
}

func (f *fakeTaskStore) ListByUser(_ context.Context, userID int64, _ tasks.ListFilter) ([]tasks.Task, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tasks.Task, 0)
	for _, row := range f.rows {
		if row.UserID == userID {
			out = append(out, *row)
		}
	}
	return out, len(out), nil
}

func (f *fakeTaskStore) ListAdmin(_ context.Context, filter tasks.ListFilter) ([]tasks.Task, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tasks.Task, 0)
	for _, row := range f.rows {
		if filter.UserID == 0 || row.UserID == filter.UserID {
			out = append(out, *row)
		}
	}
	return out, len(out), nil
}

type fakeTaskChannels struct {
	target *tasks.ChannelTarget
	err    error
	calls  int
}

func (f *fakeTaskChannels) SelectChannel(context.Context, string, string, string) (*tasks.ChannelTarget, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.target, nil
}

// GetChannel 返回同一个目标快照：单测的任务行只指向一个渠道。
func (f *fakeTaskChannels) GetChannel(context.Context, int64) (*tasks.ChannelTarget, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.target == nil {
		return nil, tasks.ErrChannelNotFound
	}
	return f.target, nil
}

type fakeTaskTokens struct{ meta *auth.TokenMeta }

func (f *fakeTaskTokens) Resolve(context.Context, string) (*auth.TokenMeta, error) {
	if f.meta == nil {
		return nil, errors.New("token not found")
	}
	copied := *f.meta
	return &copied, nil
}

type fakeTaskAccess struct{}

func (fakeTaskAccess) UserAccess(context.Context, int64) (string, []string, string, error) {
	return "user", nil, "enabled", nil
}

// taskBillingRecorder 记录 PreConsume/Reserve/Terminate/Refund/Detach 的调用
// 顺序，底层复用 billingseam.Stub 的幂等/预留语义。Detach 与两个 ByRequestID
// 让假会话实现 contract.BillingHandoff，覆盖异步交接路径。
type taskBillingRecorder struct {
	*billingseam.Stub
	mu            sync.Mutex
	calls         []string
	preConsumeErr error
	reserveErr    error
	refundErr     error
	terminateErr  error
	// hold 模拟真实引擎的预扣金额（桩的 PreConsume 只按 MaxTokens 预留，
	// 任务估算没有 MaxTokens 时会是 0）。测试用它断言 quota/退款行为。
	hold           int64
	detached       bool
	detachRequests []string
}

func newTaskBillingRecorder() *taskBillingRecorder {
	return &taskBillingRecorder{Stub: billingseam.NewStub()}
}

func (b *taskBillingRecorder) record(name string) {
	b.mu.Lock()
	b.calls = append(b.calls, name)
	b.mu.Unlock()
}

func (b *taskBillingRecorder) PreConsume(est contract.QuotaEstimate) error {
	b.record("preconsume")
	if b.preConsumeErr != nil {
		return b.preConsumeErr
	}
	if err := b.Stub.PreConsume(est); err != nil {
		return err
	}
	if b.hold > 0 {
		return b.Stub.Reserve(b.hold)
	}
	return nil
}

func (b *taskBillingRecorder) Reserve(delta int64) error {
	b.record("reserve")
	if b.reserveErr != nil {
		return b.reserveErr
	}
	return b.Stub.Reserve(delta)
}

func (b *taskBillingRecorder) Terminate(fact contract.UsageFact) error {
	b.record("terminate")
	if b.terminateErr != nil {
		return b.terminateErr
	}
	return b.Stub.Terminate(fact)
}

func (b *taskBillingRecorder) Refund() error {
	b.record("refund")
	if b.refundErr != nil {
		return b.refundErr
	}
	return b.Stub.Refund()
}

func (b *taskBillingRecorder) Detach(requestID string) error {
	b.record("detach")
	b.mu.Lock()
	b.detached = true
	b.detachRequests = append(b.detachRequests, requestID)
	b.mu.Unlock()
	return nil
}

func (b *taskBillingRecorder) TerminateByRequestID(string, contract.UsageFact) error {
	b.record("terminate_by_id")
	return nil
}

func (b *taskBillingRecorder) RefundByRequestID(string) error {
	b.record("refund_by_id")
	return nil
}

func (b *taskBillingRecorder) ReserveCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// ---------- helpers ----------

const inlinePluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel } };
  },
  parseSubmitResponse: function (resp) {
    return { taskId: "up_inline", immediate: { status: "SUCCESS" }, taskData: { usage: { seconds: 5, cost: 99 } } };
  },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`

func newTaskManager(t *testing.T, sources map[string]string) *plugin.Manager {
	t.Helper()
	manager, err := plugin.NewManager(sources, nil, plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return manager
}

func sampleTaskManager(t *testing.T) *plugin.Manager {
	t.Helper()
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("TaskPluginSources: %v", err)
	}
	return newTaskManager(t, sources)
}

func sampleInlineManager(t *testing.T) *plugin.Manager {
	t.Helper()
	return newTaskManager(t, map[string]string{"sample": inlinePluginSource})
}

func newTaskTestHandler(t *testing.T, store TaskRepository, channels TaskChannelSource,
	manager *plugin.Manager, billing func(*contract.RelayContext) contract.BillingSession) (*TasksHandler, *SecretSealer) {
	t.Helper()
	sealer := NewSecretSealer("task-handler-test-key")
	return NewTasksHandler(TasksHandlerConfig{
		Store:                store,
		Channels:             channels,
		Plugins:              manager,
		Sealer:               sealer,
		Tokens:               &fakeTaskTokens{meta: &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}},
		Billing:              billing,
		ArtifactSigner:       tasks.NewArtifactSigner("task-handler-test-key", 0),
		AllowPrivateUpstream: true,
	}), sealer
}

func sealedCredential(t *testing.T, sealer *SecretSealer, apiKey string) []byte {
	t.Helper()
	sealed, err := sealer.Seal([]byte(`{"api_key":"` + apiKey + `"}`))
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	return sealed
}

func newTaskRelayMux(handler *TasksHandler) *http.ServeMux {
	mux := http.NewServeMux()
	tokens := &fakeTaskTokens{meta: &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}}
	handler.RegisterRelay(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	return mux
}

func newTaskSubmitHTTPReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/sample", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestTaskSubmitSuccessAndBillingOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	defer upstream.Close()

	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-very-secret"),
	}}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, channels, sampleTaskManager(t), func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model","prompt":"hi","seconds":5}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp taskSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.PublicTaskID == "" || resp.State != tasks.StateSubmitted {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if store.inserted != 1 {
		t.Fatalf("inserted = %d, want 1", store.inserted)
	}
	row := store.rows[0]
	// 非终态任务：预扣 → 预留差额 → 异步交接（Detach），不结算不退款。
	if got := strings.Join(billing.ReserveCalls(), ","); got != "preconsume,reserve,detach" {
		t.Fatalf("billing call order = %q, want preconsume,reserve,detach", got)
	}
	if !billing.detached {
		t.Fatalf("task submission must detach the billing session")
	}
	if len(billing.detachRequests) != 1 || billing.detachRequests[0] != row.PrivateData["request_id"] {
		t.Fatalf("detach request id mismatch: %v vs %v", billing.detachRequests, row.PrivateData["request_id"])
	}
	if row.PrivateData["request_id"] == nil || row.PrivateData["request_id"] == "" {
		t.Fatalf("task row must persist request_id for T6b settlement: %#v", row.PrivateData)
	}
	if row.PluginGeneration == 0 || row.FetchMode != "poll" {
		t.Fatalf("unexpected generation/fetch mode: %#v", row)
	}
	if row.UserID != 42 || row.ChannelID != 9 || row.PluginKey != "sample" {
		t.Fatalf("unexpected task row: %#v", row)
	}
	// 秘密零落库/零响应：任务行与响应体都不得出现凭据明文。
	rowJSON, _ := json.Marshal(row)
	if strings.Contains(string(rowJSON), "sk-very-secret") {
		t.Fatalf("task row leaked the channel credential: %s", rowJSON)
	}
	if strings.Contains(rec.Body.String(), "sk-very-secret") {
		t.Fatalf("response leaked the channel credential: %s", rec.Body.String())
	}
}

func TestTaskSubmitNoChannel(t *testing.T) {
	store := &fakeTaskStore{}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{err: tasks.ErrChannelNotFound},
		sampleTaskManager(t), func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(billing.ReserveCalls()) != 0 {
		t.Fatalf("billing must not be touched without a channel: %v", billing.ReserveCalls())
	}
	if store.inserted != 0 {
		t.Fatalf("no task row may be inserted")
	}
}

func TestTaskSubmitUnknownPlugin(t *testing.T) {
	store := &fakeTaskStore{}
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{},
		sampleTaskManager(t), nil)
	mux := newTaskRelayMux(handler)

	req := newTaskSubmitHTTPReq(`{"model":"sample-model"}`)
	req.URL.Path = "/v1/tasks/nope"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestTaskSubmitPreConsumeFailure(t *testing.T) {
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
	}))
	defer upstream.Close()

	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	billing.preConsumeErr = billingpkg.ErrInsufficientQuota
	handler, _ := newTaskTestHandler(t, store, channels, sampleTaskManager(t), func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if upstreamHit {
		t.Fatalf("upstream must not be called after a failed pre-consume")
	}
	if store.inserted != 0 {
		t.Fatalf("402 must not persist a task row")
	}
}

func TestTaskSubmitPricingErrorIsNot402(t *testing.T) {
	store := &fakeTaskStore{}
	billing := newTaskBillingRecorder()
	billing.preConsumeErr = errors.New("billing: no pricing for plugin key")
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 1, BaseURL: "https://api.example.com", Credentials: nil,
	}}, sampleTaskManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	// 未配置定价 ≠ 余额不足：客户端不应被引导去充值。
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if store.inserted != 0 {
		t.Fatalf("pricing error must not persist a task row")
	}
}

func TestTaskSubmitUpstreamFailureRefunds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, channels, sampleTaskManager(t), func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	calls := billing.ReserveCalls()
	if len(calls) != 2 || calls[0] != "preconsume" || calls[1] != "refund" {
		t.Fatalf("billing calls = %v, want preconsume,refund", calls)
	}
	if store.inserted != 0 {
		t.Fatalf("failed submit must not persist a task row")
	}
}

func TestTaskSubmitInlineTerminal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, channels, sampleInlineManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	row := store.rows[0]
	if row.State != tasks.StateSuccess || row.ProgressPercent != 100 || row.FinishTime == nil {
		t.Fatalf("inline terminal not persisted: %#v", row)
	}
	if row.FetchMode != "inline" {
		t.Fatalf("immediate terminal fetch_mode = %q, want inline", row.FetchMode)
	}
	// SUCCESS 且已有 usage：立即 Terminate 结算，不做 Detach。
	if got := strings.Join(billing.ReserveCalls(), ","); got != "preconsume,reserve,terminate" {
		t.Fatalf("billing calls = %q, want preconsume,reserve,terminate", got)
	}
}

func TestTaskSubmitInlineFailureRefunds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	source := strings.Replace(inlinePluginSource,
		`immediate: { status: "SUCCESS" }`, `immediate: { status: "FAILURE" }`, 1)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, channels,
		newTaskManager(t, map[string]string{"sample": source}),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	row := store.rows[0]
	if row.State != tasks.StateFailure || row.Quota != 0 || row.FinishTime == nil {
		t.Fatalf("inline failure must settle to zero: %#v", row)
	}
	// 即时 FAILURE：全额退款（settle 为 0），不 Terminate。
	if got := strings.Join(billing.ReserveCalls(), ","); got != "preconsume,reserve,refund" {
		t.Fatalf("billing calls = %q, want preconsume,reserve,refund", got)
	}
	if _, pending := row.PrivateData["refund_pending"]; pending {
		t.Fatalf("refund succeeded but refund_pending was persisted")
	}
}

func TestTaskSubmitInlineSuccessWithoutUsageDetaches(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	source := strings.Replace(inlinePluginSource,
		`taskData: { usage: { seconds: 5, cost: 99 } }`, `taskData: {}`, 1)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, channels,
		newTaskManager(t, map[string]string{"sample": source}),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(billing.ReserveCalls(), ","); got != "preconsume,reserve,detach" {
		t.Fatalf("billing calls = %q, want preconsume,reserve,detach", got)
	}
}

func TestTaskSubmitRefundFailureRecordsPending(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	source := strings.Replace(inlinePluginSource,
		`immediate: { status: "SUCCESS" }`, `immediate: { status: "FAILURE" }`, 1)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	billing.hold = 100
	billing.refundErr = errors.New("refund backend unavailable")
	handler, _ := newTaskTestHandler(t, store, channels,
		newTaskManager(t, map[string]string{"sample": source}),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	row := store.rows[0]
	if row.PrivateData["refund_pending"] != true {
		t.Fatalf("failed refund must be persisted for compensation: %#v", row.PrivateData)
	}
	if row.Quota == 0 {
		t.Fatalf("failed refund must keep the pre-consumed amount")
	}
}

func TestTaskSubmitReserveFailureIsNotSwallowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	defer upstream.Close()

	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk")}}
	billing := newTaskBillingRecorder()
	billing.reserveErr = errors.New("reserve backend unavailable")
	handler, _ := newTaskTestHandler(t, store, channels, sampleTaskManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	if store.inserted != 0 {
		t.Fatalf("reserve failure must not persist a task row")
	}
	// 预扣已发生，失败后必须退款。
	calls := strings.Join(billing.ReserveCalls(), ",")
	if !strings.Contains(calls, "refund") {
		t.Fatalf("reserve failure must refund: %q", calls)
	}
}

func TestTaskQueryOwnershipAndUsageProjection(t *testing.T) {
	store := &fakeTaskStore{}
	manager := sampleInlineManager(t)
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{}, manager, nil)

	store.rows = append(store.rows, &tasks.Task{
		PublicTaskID: "task_abc", PluginKey: "sample", UserID: 42,
		State: tasks.StateInProgress, RawState: "running", ProgressPercent: 40,
		Data: map[string]any{
			"usage":     map[string]any{"seconds": float64(5), "cost": float64(99)},
			"artifacts": []any{map[string]any{"key": "v1", "type": "video"}},
		},
	})

	rec := httptest.NewRecorder()
	taskQueryMux(handler).ServeHTTP(rec, taskRequest(http.MethodGet, "/v1/tasks/sample?task_id=task_abc", 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp taskQueryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Usage["seconds"] != float64(5) {
		t.Fatalf("declared usage missing: %#v", resp.Usage)
	}
	if _, ok := resp.Usage["cost"]; ok {
		t.Fatalf("undeclared usage leaked: %#v", resp.Usage)
	}
	if len(resp.Artifacts) != 1 {
		t.Fatalf("artifacts = %#v", resp.Artifacts)
	}

	// 他人任务与不存在任务同为 404，不能出现归属侧信道。
	rec = httptest.NewRecorder()
	taskQueryMux(handler).ServeHTTP(rec, taskRequest(http.MethodGet, "/v1/tasks/sample?task_id=task_abc", 99))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign task status = %d, want 404", rec.Code)
	}
}

func TestTaskArtifactsEmptyShape(t *testing.T) {
	store := &fakeTaskStore{}
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{}, sampleTaskManager(t), nil)
	store.rows = append(store.rows, &tasks.Task{
		PublicTaskID: "task_empty", PluginKey: "sample", UserID: 42, Data: map[string]any{},
	})
	rec := httptest.NewRecorder()
	taskQueryMux(handler).ServeHTTP(rec, taskRequest(http.MethodGet, "/v1/tasks/sample/artifacts?task_id=task_empty", 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty artifacts shape = %s", rec.Body.String())
	}
}

func TestTaskSelfListHidesUserAndAdminRequiresPermission(t *testing.T) {
	store := &fakeTaskStore{}
	store.rows = append(store.rows,
		&tasks.Task{PublicTaskID: "task_1", PluginKey: "sample", UserID: 42, CreatedAt: time.Now()},
		&tasks.Task{PublicTaskID: "task_2", PluginKey: "sample", UserID: 7, CreatedAt: time.Now()},
	)
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{}, sampleTaskManager(t), nil)

	rec := httptest.NewRecorder()
	handler.handleSelfList(rec, taskRequest(http.MethodGet, "/api/task/self", 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("self status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"user_id":42`) || strings.Contains(rec.Body.String(), "task_2") {
		t.Fatalf("self list leaked user_id or foreign rows: %s", rec.Body.String())
	}

	// 普通用户访问 admin 台账 → 403。
	rec = httptest.NewRecorder()
	handler.handleAdminList(rec, taskRequest(http.MethodGet, "/api/task", 42))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}

	// admin 但缺 logs:read（非 nil 的空权限集）→ 403。
	req := httptest.NewRequest(http.MethodGet, "/api/task", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{
		UserID: 1, Role: "admin", Permissions: []string{},
	}))
	rec = httptest.NewRecorder()
	handler.handleAdminList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin without logs:read status = %d, want 403", rec.Code)
	}

	// admin + FULL 权限 → 200，看到全部用户。
	req = httptest.NewRequest(http.MethodGet, "/api/task", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{
		UserID: 1, Role: "admin", Permissions: nil,
	}))
	rec = httptest.NewRecorder()
	handler.handleAdminList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp taskListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	ids := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		ids = append(ids, item.PublicTaskID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "task_1,task_2" {
		t.Fatalf("admin list = %v", ids)
	}
}

func TestTaskSubmitRejectsOversizedBodyAndBadDuration(t *testing.T) {
	store := &fakeTaskStore{}
	billing := newTaskBillingRecorder()
	handler, _ := newTaskTestHandler(t, store, &fakeTaskChannels{}, sampleTaskManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing })
	mux := newTaskRelayMux(handler)

	big := `{"model":"sample-model","prompt":"` + strings.Repeat("a", maxTaskSubmitBytes) + `"}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(big))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model","seconds":999999}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad duration status = %d, want 400", rec.Code)
	}
	if len(billing.ReserveCalls()) != 0 {
		t.Fatalf("validation failures must not touch billing")
	}
}

// taskQueryMux 注册查询/产物路由，让 PathValue("key") 在直调测试里也能解析。
func taskQueryMux(handler *TasksHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tasks/{key}", handler.handleGet)
	mux.HandleFunc("GET /v1/tasks/{key}/artifacts", handler.handleArtifacts)
	return mux
}

// taskRequest 构造带会话身份的请求（/api 端点直接调用 handler，不经过中间件）。
func taskRequest(method, target string, userID int64) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	ctx := auth.WithPrincipal(req.Context(), &auth.Principal{UserID: userID, Role: "user"})
	return req.WithContext(ctx)
}
