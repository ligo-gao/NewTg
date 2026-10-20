package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/plugin"
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
}

func newFakeBatchTaskStore() *fakeBatchTaskStore {
	return &fakeBatchTaskStore{
		fakeTaskStore: &fakeTaskStore{},
		batches:       make(map[string]*tasks.Batch),
		items:         make(map[string][]string),
	}
}

// InsertBatch 复刻生产仓储的语义：每条 public_task_id 必须属于本用户，
// 否则整体失败（事务回滚由真实实现保证）。
func (f *fakeBatchTaskStore) InsertBatch(ctx context.Context, b *tasks.Batch, itemPublicIDs []string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, publicID := range itemPublicIDs {
		found := false
		for _, row := range f.fakeTaskStore.rows {
			if row.PublicTaskID == publicID && row.UserID == b.UserID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("batch item %q: task not found for user", publicID)
		}
	}
	copied := *b
	copied.CreatedAt = time.Now().UTC()
	copied.UpdatedAt = copied.CreatedAt
	f.batches[b.ID] = &copied
	f.items[b.ID] = append([]string(nil), itemPublicIDs...)
	f.inserted++
	return nil
}

// GetBatchByID 与生产实现同一套聚合口径（completed/failed/status）。
func (f *fakeBatchTaskStore) GetBatchByID(_ context.Context, userID int64, id string) (*tasks.Batch, error) {
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
  parseSubmitResponse: function () { return {}; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
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

func newBatchEnv(t *testing.T, manager *plugin.Manager) (*fakeBatchTaskStore, *http.ServeMux) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	t.Cleanup(upstream.Close)
	sealer := NewSecretSealer("task-handler-test-key")
	store := newFakeBatchTaskStore()
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-test"),
	}}
	handler, _ := newTaskTestHandler(t, store, channels, manager, nil)
	return store, newTaskRelayMux(handler)
}

// newBatchEnvFor 只取 mux（不需要直接操作存储的用例）。
func newBatchEnvFor(t *testing.T, manager *plugin.Manager) *http.ServeMux {
	t.Helper()
	_, mux := newBatchEnv(t, manager)
	return mux
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
	store, mux := newBatchEnv(t, manager)

	rec := postBatch(t, mux, twoItemBatchBody())
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
	if store.fakeTaskStore.inserted != 2 {
		t.Fatalf("inserted tasks = %d, want 2 (one per batch row)", store.inserted)
	}
	if len(created.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(created.Items))
	}

	got := getBatch(t, mux, created.ID)
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
	for _, row := range store.fakeTaskStore.rows {
		if row.PluginKey == "slow" {
			row.State = tasks.StateSuccess
			row.ProgressPercent = 100
		}
	}
	got = getBatch(t, mux, created.ID)
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
	store, mux := newBatchEnv(t, manager)
	body := `{"requests":[
	  {"custom_id":"ok","url":"/v1/videos","body":{"model":"fast-model"}},
	  {"custom_id":"bad","url":"/v1/videos","body":{"model":"broken-model"}}
	]}`
	rec := postBatch(t, mux, body)
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
	if store.fakeTaskStore.inserted != 1 {
		t.Fatalf("inserted tasks = %d, want 1 (failed row has no task row)", store.inserted)
	}
}

func TestBatchesRejectsUnsupportedInputsBeforeBilling(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	store, mux := newBatchEnv(t, manager)

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
			rec := postBatch(t, mux, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if store.inserted != 0 || store.fakeTaskStore.inserted != 0 {
				t.Fatalf("validation failure must not submit: batches=%d tasks=%d",
					store.inserted, store.fakeTaskStore.inserted)
			}
		})
	}
}

func TestBatchesOverLimitIsRejected(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	store, mux := newBatchEnv(t, manager)
	rows := make([]string, 0, maxBatchItems+1)
	for i := 0; i < maxBatchItems+1; i++ {
		rows = append(rows, fmt.Sprintf(`{"custom_id":"r%d","url":"/v1/videos","body":{"model":"fast-model"}}`, i))
	}
	rec := postBatch(t, mux, `{"requests":[`+strings.Join(rows, ",")+`]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.fakeTaskStore.inserted != 0 {
		t.Fatalf("inserted = %d, want 0", store.fakeTaskStore.inserted)
	}
}

func TestBatchesCancelFailsClosed(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	mux := newBatchEnvFor(t, manager)
	rec := postBatch(t, mux,
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
	mux.ServeHTTP(cancel, req)
	if cancel.Code != http.StatusNotImplemented {
		t.Fatalf("cancel status = %d, want 501; body=%s", cancel.Code, cancel.Body.String())
	}
	if ct := cancel.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	// 取消不改变任何状态：批次仍按子任务聚合。
	if view := getBatch(t, mux, created.ID); view.Code != http.StatusOK {
		t.Fatalf("get after cancel = %d", view.Code)
	}
}

func TestBatchesNotFoundAndOwnership(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	store, mux := newBatchEnv(t, manager)

	for _, id := range []string{"batch_not-a-uuid", "batch_00000000-0000-0000-0000-000000000000", "nope"} {
		if rec := getBatch(t, mux, id); rec.Code != http.StatusNotFound {
			t.Fatalf("id %q: status = %d, want 404; body=%s", id, rec.Code, rec.Body.String())
		}
	}

	rec := postBatch(t, mux,
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
	if _, err := store.GetBatchByID(t.Context(), 7, rawID); err != tasks.ErrBatchNotFound {
		t.Fatalf("foreign batch err = %v, want ErrBatchNotFound", err)
	}
}
