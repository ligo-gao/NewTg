package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/tasks"
)

// T4 / P-04 / R-13 的 HTTP 面验收：
//   - POST /v1/videos 落 tasks 行，GET /v1/videos/{id} 返回 OpenAI video 形态；
//   - model → 插件的解析是唯一入口（0 命中 404、多命中 400）；
//   - 归属不符 / 畸形 id 一律 404；
//   - 没有视频产物时 content 端点 404，不把别的产物冒充视频。

// videoPluginSource 构造一个只声明给定模型的最小任务插件。
// immediate=true 时 parseSubmitResponse 直接给出 SUCCESS（用于验证
// completed 状态的投影）；否则返回 SUBMITTED（交给轮询器）。
func videoPluginSource(key, model string, immediate bool) string {
	immediateJS := ""
	if immediate {
		immediateJS = `immediate: { status: "SUCCESS" },`
	}
	return fmt.Sprintf(`globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: %q,
  name: %q,
  version: "1.0.0",
  author: { name: "NewTG" },
  description: { en: "Video plugin", zh: "视频插件" },
  models: [%q],
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
  parseSubmitResponse: function () {
    return { taskId: "up_%s", %s taskData: { usage: { seconds: 5 } } };
  },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`, key, key, model, key, immediateJS)
}

func newVideoManager(t *testing.T, sources map[string]string) *plugin.Manager {
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

func newVideoEnv(t *testing.T, manager *plugin.Manager) (*TasksHandler, *http.ServeMux, *fakeTaskStore) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	t.Cleanup(upstream.Close)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-test"),
	}}
	handler, _ := newTaskTestHandler(t, store, channels, manager, nil)
	return handler, newTaskRelayMux(handler), store
}

func postVideo(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func getVideo(t *testing.T, mux *http.ServeMux, videoID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoID, nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestVideosSubmitCreatesTaskAndRetrieveReturnsVideoShape(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"vid": videoPluginSource("vid", "vid-model", false),
	})
	_, mux, store := newVideoEnv(t, manager)

	rec := postVideo(t, mux, `{"model":"vid-model","prompt":"a cat","seconds":5,"size":"1280x720"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var created videoObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode video: %v", err)
	}
	if !strings.HasPrefix(created.ID, videoIDPrefix) || created.Object != "video" || created.Status != "queued" {
		t.Fatalf("unexpected video object: %#v", created)
	}
	if store.inserted != 1 {
		t.Fatalf("inserted = %d, want 1 (videos must land a tasks row)", store.inserted)
	}
	row := store.rows[0]
	if row.PluginKey != "vid" || row.UserID != 42 || row.Properties["OriginModelName"] != "vid-model" {
		t.Fatalf("unexpected task row: %#v", row)
	}
	// 复用 /v1/tasks 的提交语义：请求体原样进入 Properties.Input。
	input, _ := row.Properties["Input"].(map[string]any)
	if input["prompt"] != "a cat" {
		t.Fatalf("request body must be preserved in task properties: %#v", row.Properties)
	}

	got := getVideo(t, mux, created.ID)
	if got.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d, want 200; body=%s", got.Code, got.Body.String())
	}
	var retrieved videoObject
	if err := json.Unmarshal(got.Body.Bytes(), &retrieved); err != nil {
		t.Fatalf("decode retrieved video: %v", err)
	}
	if retrieved.ID != created.ID || retrieved.Status != "queued" || retrieved.Model != "vid-model" {
		t.Fatalf("unexpected retrieved video: %#v", retrieved)
	}
	if retrieved.Size != "1280x720" {
		t.Fatalf("size = %q, want request value echoed", retrieved.Size)
	}
}

func TestVideosImmediateSuccessProjectsCompleted(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	_, mux, store := newVideoEnv(t, manager)
	rec := postVideo(t, mux, `{"model":"fast-model"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created videoObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 即时终态：落库即 success，创建响应也必须如实给出 completed（不是 queued）。
	if created.Status != "completed" || created.Progress != 100 {
		t.Fatalf("immediate success projected as %#v, want completed/100", created)
	}
	if store.rows[0].State != tasks.StateSuccess {
		t.Fatalf("task state = %s, want success", store.rows[0].State)
	}
}

func TestVideosUnknownModelIs404(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"vid": videoPluginSource("vid", "vid-model", false),
	})
	_, mux, store := newVideoEnv(t, manager)
	rec := postVideo(t, mux, `{"model":"not-declared"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertOpenAIErrorEnvelope(t, rec, "model_not_found")
	if store.inserted != 0 {
		t.Fatalf("inserted = %d, want 0 (unknown model must not submit)", store.inserted)
	}
}

func TestVideosAmbiguousModelIs400(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"a": videoPluginSource("a", "shared-model", false),
		"b": videoPluginSource("b", "shared-model", false),
	})
	_, mux, store := newVideoEnv(t, manager)
	rec := postVideo(t, mux, `{"model":"shared-model"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (ambiguous model); body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 0 {
		t.Fatalf("inserted = %d, want 0", store.inserted)
	}
}

func TestVideosMultipartAndMissingModelRejected(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"vid": videoPluginSource("vid", "vid-model", false),
	})
	_, mux, store := newVideoEnv(t, manager)

	for _, body := range []string{"", "not-json", `{"prompt":"no model"}`} {
		rec := postVideo(t, mux, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
	}
	if store.inserted != 0 {
		t.Fatalf("inserted = %d, want 0", store.inserted)
	}
}

func TestVideosRetrieveNotFoundAndOwnership(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"vid": videoPluginSource("vid", "vid-model", false),
	})
	_, mux, store := newVideoEnv(t, manager)

	// 畸形 id / 不存在 / 非 video_ 前缀：统一 404。
	for _, id := range []string{"video_nope", "task_deadbeef", "video_"} {
		rec := getVideo(t, mux, id)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id %q: status = %d, want 404; body=%s", id, rec.Code, rec.Body.String())
		}
	}

	// 他人任务：响应必须与「不存在」逐字节同类（除随机 requestId）。
	foreign := &tasks.Task{
		PublicTaskID: "task_0123456789abcdef0123456789abcdef",
		UserID:       7,
		State:        tasks.StateSuccess,
	}
	if err := store.Insert(t.Context(), foreign); err != nil {
		t.Fatalf("insert foreign: %v", err)
	}
	rec := getVideo(t, mux, "video_0123456789abcdef0123456789abcdef")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign task status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVideosContentWithoutArtifactIs404(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	_, mux, _ := newVideoEnv(t, manager)
	rec := postVideo(t, mux, `{"model":"fast-model"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created videoObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+created.ID+"/content", nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	got := httptest.NewRecorder()
	mux.ServeHTTP(got, req)
	// 插件没有产出 video 类型产物（parseTaskResult 只给 state）：不得把空内容
	// 当成功返回，也不得返回任何非视频产物。
	if got.Code != http.StatusNotFound {
		t.Fatalf("content status = %d, want 404; body=%s", got.Code, got.Body.String())
	}
}

func TestVideosRequireBearerToken(t *testing.T) {
	manager := newVideoManager(t, map[string]string{
		"vid": videoPluginSource("vid", "vid-model", false),
	})
	_, mux, store := newVideoEnv(t, manager)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"vid-model"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if store.inserted != 0 {
		t.Fatalf("inserted = %d, want 0", store.inserted)
	}
}
