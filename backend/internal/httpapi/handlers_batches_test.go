package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/tasks"
)

// T4 / P-04 的批次验收：
//   - 每行子请求逐个建 task，父批次只做聚合；
//   - 状态由子任务现算：任一非终态 → in_progress，全终态 → completed/failed；
//   - 提交失败的行计入 failed（没有任务行），参数错误在任何计费之前拒绝；
//   - 取消 fail-closed。

// fakeBatchTaskStore 同时实现 TaskRepository 与 BatchRepository：
// 生产 tasks.Repo 也是如此，测试用同一实例证明「不新造第二套仓储」。
type fakeBatchTaskStore struct {
	*fakeTaskStore
	mu       sync.Mutex
	batches  map[string]*tasks.Batch
	items    map[string][]string
	inserted int
	saveErr  error
	// beforeInsert 在落库前触发（测试用它模拟「持久化时请求 context 已取消」）。
	beforeInsert func()
	// checkCtx 模拟真实驱动：上下文已取消即返回错误。R-16 要求持久化用
	// WithoutCancel 的上下文，因此该检查在修复后必须不被触发。
	checkCtx bool
}

func (f *fakeBatchTaskStore) ctxErr(ctx context.Context) error {
	if f.checkCtx && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func newFakeBatchTaskStore() *fakeBatchTaskStore {
	return &fakeBatchTaskStore{
		fakeTaskStore: &fakeTaskStore{},
		batches:       make(map[string]*tasks.Batch),
		items:         make(map[string][]string),
	}
}

// InsertBatch 复刻生产仓储：只落父行（R-16 的第 1 步）。
func (f *fakeBatchTaskStore) InsertBatch(ctx context.Context, b *tasks.Batch) error {
	if f.beforeInsert != nil {
		f.beforeInsert()
	}
	if err := f.ctxErr(ctx); err != nil {
		return err
	}
	if f.saveErr != nil {
		return f.saveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *b
	copied.CreatedAt = time.Now().UTC()
	copied.UpdatedAt = copied.CreatedAt
	f.batches[b.ID] = &copied
	f.inserted++
	return nil
}

// LinkBatchItem 复刻生产仓储：任务必须存在且属于本用户，否则报错。
func (f *fakeBatchTaskStore) LinkBatchItem(ctx context.Context, batchID string, userID int64, publicID string) error {
	if err := f.ctxErr(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.batches[batchID]
	if !ok || stored.UserID != userID {
		return tasks.ErrBatchNotFound
	}
	for _, row := range f.fakeTaskStore.rows {
		if row.PublicTaskID == publicID && row.UserID == userID {
			f.items[batchID] = append(f.items[batchID], publicID)
			return nil
		}
	}
	return fmt.Errorf("batch item %q: task not found for user", publicID)
}

// GetBatchByID 与生产实现同一套聚合口径（completed/failed/status）。
func (f *fakeBatchTaskStore) GetBatchByID(ctx context.Context, userID int64, id string) (*tasks.Batch, error) {
	if err := f.ctxErr(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	stored, ok := f.batches[id]
	itemIDs := append([]string(nil), f.items[id]...)
	f.mu.Unlock()
	if !ok || stored.UserID != userID {
		return nil, tasks.ErrBatchNotFound
	}
	out := *stored
	out.ID = tasks.FormatBatchPublicID(id)
	completed, failedTasks := 0, 0
	for _, publicID := range itemIDs {
		for _, row := range f.fakeTaskStore.rows {
			if row.PublicTaskID != publicID {
				continue
			}
			switch row.State {
			case tasks.StateSuccess:
				completed++
			case tasks.StateFailure:
				failedTasks++
			}
			out.Items = append(out.Items, tasks.BatchItem{
				PublicTaskID: row.PublicTaskID, State: row.State, ProgressPercent: row.ProgressPercent,
			})
		}
	}
	missing := out.Total - len(itemIDs)
	if missing < 0 {
		missing = 0
	}
	out.Completed = completed
	out.Failed = failedTasks + missing
	switch {
	case out.Total == 0:
		out.Status = tasks.BatchStatusFailed
	case completed+out.Failed >= out.Total && completed == 0:
		out.Status = tasks.BatchStatusFailed
	case completed+out.Failed >= out.Total:
		out.Status = tasks.BatchStatusCompleted
	default:
		out.Status = tasks.BatchStatusInProgress
	}
	return &out, nil
}

// brokenPluginSource 的 parseSubmitResponse 返回空对象：提交管线会判定
// 「无效提交响应」并 502，用于覆盖「行提交失败但批次仍成立」的分支。
const brokenPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "broken",
  name: "Broken",
  version: "1.0.0",
  author: { name: "NewTG" },
  description: { en: "Broken plugin", zh: "损坏插件" },
  models: ["broken-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [],
  routes: [],
  protocols: { openai_video: {} },
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel } };
  },
  parseSubmitResponse: function () { return {}; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; },
  decodeRequest: function (ctx) { return ctx.body; },
  render: function () { return {}; }
};`

func newBatchManager(t *testing.T, sources map[string]string) *plugin.Manager {
	t.Helper()
	manager, err := plugin.NewManager(sources, nil, plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return manager
}

// batchTestEnv 聚合批次用例需要的接缝：存储、mux、计费记录器、渠道计数与
// 上游命中计数（M6 要求「零副作用」断言到计费与上游调用，而不只是任务行）。
type batchTestEnv struct {
	store        *fakeBatchTaskStore
	mux          *http.ServeMux
	billing      *billingCallLog
	channels     *fakeTaskChannels
	upstreamHits *atomic.Int32
}

// billingCallLog 记录每次提交管线对计费接缝的调用（跨多个请求共享）。
//
// 不能复用 taskBillingRecorder：它是**单会话**实现（底层 billingseam.Stub
// 只能 PreConsume 一次），批次有 N 个子请求，共享一个会话会让第二行开始
// 全部计费失败。这里每个请求新建会话，只共享调用日志——零副作用断言只关心
// 「有没有发生 PreConsume」。
type billingCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *billingCallLog) add(name string) {
	l.mu.Lock()
	l.calls = append(l.calls, name)
	l.mu.Unlock()
}

func (l *billingCallLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type perRequestBilling struct {
	*billingseam.Stub
	log *billingCallLog
}

func (b *perRequestBilling) PreConsume(est contract.QuotaEstimate) error {
	b.log.add("preconsume")
	return b.Stub.PreConsume(est)
}

func (b *perRequestBilling) Reserve(delta int64) error {
	b.log.add("reserve")
	return b.Stub.Reserve(delta)
}

func (b *perRequestBilling) Terminate(fact contract.UsageFact) error {
	b.log.add("terminate")
	return b.Stub.Terminate(fact)
}

func (b *perRequestBilling) Refund() error {
	b.log.add("refund")
	return b.Stub.Refund()
}

func (b *perRequestBilling) Detach(string) error {
	b.log.add("detach")
	return nil
}

func (b *perRequestBilling) TerminateByRequestID(string, contract.UsageFact) error {
	b.log.add("terminate_by_id")
	return nil
}

func (e *batchTestEnv) hits() int32 {
	if e.upstreamHits == nil {
		return 0
	}
	return e.upstreamHits.Load()
}

func newBatchEnv(t *testing.T, manager *plugin.Manager) *batchTestEnv {
	t.Helper()
	hits := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	t.Cleanup(upstream.Close)
	sealer := NewSecretSealer("task-handler-test-key")
	store := newFakeBatchTaskStore()
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-test"),
	}}
	billing := &billingCallLog{}
	handler, _ := newTaskTestHandler(t, store, channels, manager,
		func(*contract.RelayContext) contract.BillingSession {
			return &perRequestBilling{Stub: billingseam.NewStub(), log: billing}
		})
	return &batchTestEnv{
		store: store, mux: newTaskRelayMux(handler),
		billing: billing, channels: channels, upstreamHits: hits,
	}
}

func postBatch(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func getBatch(t *testing.T, mux *http.ServeMux, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/batches/"+id, nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func twoItemBatchBody() string {
	return `{
	  "endpoint": "/v1/videos",
	  "completion_window": "24h",
	  "requests": [
	    {"custom_id":"r1","method":"POST","url":"/v1/videos","body":{"model":"fast-model","seconds":5}},
	    {"custom_id":"r2","method":"POST","url":"/v1/videos","body":{"model":"slow-model","seconds":5}}
	  ]
	}`
}

func TestBatchesSubmitCreatesOneTaskPerRowAndAggregates(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
		"slow": videoPluginSource("slow", "slow-model", false),
	})
	env := newBatchEnv(t, manager)

	rec := postBatch(t, env.mux, twoItemBatchBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var created batchObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if !strings.HasPrefix(created.ID, tasks.BatchPublicIDPrefix) || created.Object != "batch" {
		t.Fatalf("unexpected batch object: %#v", created)
	}
	if created.RequestCounts.Total != 2 || created.RequestCounts.Completed != 1 || created.RequestCounts.Failed != 0 {
		t.Fatalf("request_counts = %#v, want total=2 completed=1 failed=0", created.RequestCounts)
	}
	// 部分子任务仍在进行 → in_progress（不是 created/completed）。
	if created.Status != tasks.BatchStatusInProgress {
		t.Fatalf("status = %q, want in_progress", created.Status)
	}
	if env.store.fakeTaskStore.inserted != 2 {
		t.Fatalf("inserted tasks = %d, want 2 (one per batch row)", env.store.inserted)
	}
	if len(created.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(created.Items))
	}

	got := getBatch(t, env.mux, created.ID)
	if got.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", got.Code, got.Body.String())
	}
	var view batchObject
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	if view.Status != tasks.BatchStatusInProgress {
		t.Fatalf("view status = %q, want in_progress", view.Status)
	}

	// 把仍在进行的那条任务推进到终态：批次下一次读取即 completed。
	for _, row := range env.store.fakeTaskStore.rows {
		if row.PluginKey == "slow" {
			row.State = tasks.StateSuccess
			row.ProgressPercent = 100
		}
	}
	got = getBatch(t, env.mux, created.ID)
	if got.Code != http.StatusOK {
		t.Fatalf("get after terminal: status = %d, body=%s", got.Code, got.Body.String())
	}
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode final view: %v", err)
	}
	if view.Status != tasks.BatchStatusCompleted || view.RequestCounts.Completed != 2 {
		t.Fatalf("final view = %#v, want completed with completed=2", view)
	}
}

func TestBatchesCountsFailedSubmissions(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast":   videoPluginSource("fast", "fast-model", true),
		"broken": brokenPluginSource,
	})
	env := newBatchEnv(t, manager)
	body := `{"requests":[
	  {"custom_id":"ok","url":"/v1/videos","body":{"model":"fast-model"}},
	  {"custom_id":"bad","url":"/v1/videos","body":{"model":"broken-model"}}
	]}`
	rec := postBatch(t, env.mux, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (row failure must not fail the batch); body=%s", rec.Code, rec.Body.String())
	}
	var created batchObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.RequestCounts.Total != 2 || created.RequestCounts.Completed != 1 || created.RequestCounts.Failed != 1 {
		t.Fatalf("request_counts = %#v, want total=2 completed=1 failed=1", created.RequestCounts)
	}
	// 全部行已终态（成功 + 提交失败）→ completed，而不是永远 in_progress。
	if created.Status != tasks.BatchStatusCompleted {
		t.Fatalf("status = %q, want completed", created.Status)
	}
	if env.store.fakeTaskStore.inserted != 1 {
		t.Fatalf("inserted tasks = %d, want 1 (failed row has no task row)", env.store.inserted)
	}
}

func TestBatchesRejectsUnsupportedInputsBeforeBilling(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"input_file_id", `{"input_file_id":"file-123"}`, http.StatusNotImplemented},
		{"unsupported_url", `{"requests":[{"custom_id":"x","url":"/v1/chat/completions","body":{"model":"fast-model"}}]}`, http.StatusBadRequest},
		{"duplicate_custom_id", `{"requests":[
		  {"custom_id":"x","url":"/v1/videos","body":{"model":"fast-model"}},
		  {"custom_id":"x","url":"/v1/videos","body":{"model":"fast-model"}}]}`, http.StatusBadRequest},
		{"metadata", `{"metadata":{"k":"v"},"requests":[{"custom_id":"x","url":"/v1/videos","body":{"model":"fast-model"}}]}`, http.StatusBadRequest},
		{"unknown_model", `{"requests":[{"custom_id":"x","url":"/v1/videos","body":{"model":"nope"}}]}`, http.StatusNotFound},
		{"empty", `{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个用例独立环境：M6 要求零副作用断言到渠道选择、计费接缝与
			// 上游调用，共享计数器会让断言失去意义。
			manager := newBatchManager(t, map[string]string{
				"fast": videoPluginSource("fast", "fast-model", true),
			})
			env := newBatchEnv(t, manager)
			rec := postBatch(t, env.mux, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if env.store.inserted != 0 || env.store.fakeTaskStore.inserted != 0 {
				t.Fatalf("validation failure must not submit: batches=%d tasks=%d",
					env.store.inserted, env.store.fakeTaskStore.inserted)
			}
			if env.channels.calls != 0 {
				t.Fatalf("channel selection calls = %d, want 0 (before billing)", env.channels.calls)
			}
			if calls := env.billing.snapshot(); len(calls) != 0 {
				t.Fatalf("billing calls = %v, want none (before any submission)", calls)
			}
			if hits := env.hits(); hits != 0 {
				t.Fatalf("upstream requests = %d, want 0", hits)
			}
		})
	}
}

func TestBatchesOverLimitIsRejected(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	env := newBatchEnv(t, manager)
	rows := make([]string, 0, maxBatchItems+1)
	for i := 0; i < maxBatchItems+1; i++ {
		rows = append(rows, fmt.Sprintf(`{"custom_id":"r%d","url":"/v1/videos","body":{"model":"fast-model"}}`, i))
	}
	rec := postBatch(t, env.mux, `{"requests":[`+strings.Join(rows, ",")+`]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if env.store.fakeTaskStore.inserted != 0 {
		t.Fatalf("inserted = %d, want 0", env.store.fakeTaskStore.inserted)
	}
}

func TestBatchesCancelFailsClosed(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	env := newBatchEnv(t, manager)
	rec := postBatch(t, env.mux,
		`{"requests":[{"custom_id":"x","url":"/v1/videos","body":{"model":"fast-model"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created batchObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/batches/"+created.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	cancel := httptest.NewRecorder()
	env.mux.ServeHTTP(cancel, req)
	if cancel.Code != http.StatusNotImplemented {
		t.Fatalf("cancel status = %d, want 501; body=%s", cancel.Code, cancel.Body.String())
	}
	if ct := cancel.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	// 取消不改变任何状态：批次仍按子任务聚合。
	if view := getBatch(t, env.mux, created.ID); view.Code != http.StatusOK {
		t.Fatalf("get after cancel = %d", view.Code)
	}
}

func TestBatchesNotFoundAndOwnership(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	env := newBatchEnv(t, manager)

	for _, id := range []string{"batch_not-a-uuid", "batch_00000000-0000-0000-0000-000000000000", "nope"} {
		if rec := getBatch(t, env.mux, id); rec.Code != http.StatusNotFound {
			t.Fatalf("id %q: status = %d, want 404; body=%s", id, rec.Code, rec.Body.String())
		}
	}

	rec := postBatch(t, env.mux,
		`{"requests":[{"custom_id":"x","url":"/v1/videos","body":{"model":"fast-model"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created batchObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 他人读取：仓储按 user_id 过滤，handler 统一 404。
	rawID, ok := tasks.ParseBatchPublicID(created.ID)
	if !ok {
		t.Fatalf("created id is not parseable: %q", created.ID)
	}
	if _, err := env.store.GetBatchByID(t.Context(), 7, rawID); err != tasks.ErrBatchNotFound {
		t.Fatalf("foreign batch err = %v, want ErrBatchNotFound", err)
	}
}

// TestBatchesAllSubmissionsFailedIsFailed 覆盖 BatchStatusFailed 分支：
// 两行都提交失败（没有任务行）→ status=failed、total=2/completed=0/failed=2。
// 该用例会杀掉「全失败 → in_progress」的变异。
func TestBatchesAllSubmissionsFailedIsFailed(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"broken": brokenPluginSource,
	})
	env := newBatchEnv(t, manager)
	body := `{"requests":[
	  {"custom_id":"a","url":"/v1/videos","body":{"model":"broken-model"}},
	  {"custom_id":"b","url":"/v1/videos","body":{"model":"broken-model"}}
	]}`
	rec := postBatch(t, env.mux, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (row failures are aggregated); body=%s", rec.Code, rec.Body.String())
	}
	var created batchObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Status != tasks.BatchStatusFailed {
		t.Fatalf("status = %q, want failed", created.Status)
	}
	if created.RequestCounts.Total != 2 || created.RequestCounts.Completed != 0 || created.RequestCounts.Failed != 2 {
		t.Fatalf("request_counts = %#v, want total=2 completed=0 failed=2", created.RequestCounts)
	}
	if env.store.fakeTaskStore.inserted != 0 {
		t.Fatalf("inserted tasks = %d, want 0", env.store.fakeTaskStore.inserted)
	}
}

// TestBatchesPersistFailureIsZeroCost 覆盖 R-16 第 1 步：父行落库失败必须发生
// 在任何计费/上游调用之前。记录型计费与渠道/上游计数证明零副作用，客户端可
// 以安全重试而不会重复建任务、重复扣费。
func TestBatchesPersistFailureIsZeroCost(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	env := newBatchEnv(t, manager)
	env.store.saveErr = errors.New("database is unavailable")

	rec := postBatch(t, env.mux,
		`{"requests":[{"custom_id":"x","url":"/v1/videos","body":{"model":"fast-model"}}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if env.store.fakeTaskStore.inserted != 0 {
		t.Fatalf("task rows = %d, want 0 (no submission before the parent row)", env.store.fakeTaskStore.inserted)
	}
	if env.channels.calls != 0 {
		t.Fatalf("channel selection calls = %d, want 0", env.channels.calls)
	}
	if calls := env.billing.snapshot(); len(calls) != 0 {
		t.Fatalf("billing calls = %v, want none", calls)
	}
	if hits := env.hits(); hits != 0 {
		t.Fatalf("upstream requests = %d, want 0", hits)
	}
}
