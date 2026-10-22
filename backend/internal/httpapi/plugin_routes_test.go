package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/tasks"
)

// 本文件是 T6c-a 的 HTTP 面验收：
//   - 插件原生路由命中原生分发面，未命中一律 404 OpenAI 信封（P-05）；
//   - 命中路由的鉴权与 /v1/tasks 同一条 RelayAuth 链；
//   - R-04：`/v1/` 兜底不得吞掉已注册的精确路由（chat/models/tasks）。

// nativeRoutePluginSource 声明两类原生路由：精确 submit 与带参数的 query。
// 元数据保持与仓库既有测试插件同形，只追加 routes[]。
const nativeRoutePluginSource = `globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "video",
  name: "Video",
  version: "1.0.0",
  author: { name: "NewTG" },
  description: { en: "Video task plugin", zh: "视频任务插件" },
  models: ["video-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [],
  routes: [
    { method: "POST", path: "/v1/video-native", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" },
    { method: "POST", path: "/v1/videos/:key/remix", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" },
    { method: "GET", path: "/v1/suno/fetch/:task_id", kind: "query", render: "parseTaskResult" }
  ],
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
  parseSubmitResponse: function () { return { taskId: "up_native" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`

func newNativeRouteManager(t *testing.T) *plugin.Manager {
	t.Helper()
	manager, err := plugin.NewManager(map[string]string{"video": nativeRoutePluginSource}, nil,
		plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return manager
}

// recordingNativeDispatcher 是 NativeRouteDispatcher 的内存 fake：索引来自
// 真实 plugin.Manager（证明注册期产物可直接被 HTTP 面消费），分发只记录命中。
type recordingNativeDispatcher struct {
	manager *plugin.Manager
	mu      sync.Mutex
	calls   []plugin.RouteMatch
}

func (d *recordingNativeDispatcher) NativeRoutes() plugin.RouteIndex {
	return d.manager.Snapshot().Routes()
}

func (d *recordingNativeDispatcher) DispatchNativeRoute(w http.ResponseWriter, _ *http.Request, match plugin.RouteMatch) {
	d.mu.Lock()
	d.calls = append(d.calls, match)
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (d *recordingNativeDispatcher) last() (plugin.RouteMatch, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return plugin.RouteMatch{}, false
	}
	return d.calls[len(d.calls)-1], true
}

func (d *recordingNativeDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// newNativeRelayMux 构造只挂中继面（含 /v1/ 兜底）的 mux。
func newNativeRelayMux(t *testing.T, dispatcher NativeRouteDispatcher) *http.ServeMux {
	t.Helper()
	h := NewRelayHandler(RelayHandlerOptions{
		Channels: &fakeRelayChannelStore{models: []string{"gpt-test"}},
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: relayTestTokenID, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
		}),
		Access:       &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:       NewSecretSealer(relaySealerMaster),
		NativeRoutes: dispatcher,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func doNative(t *testing.T, mux http.Handler, method, path, body string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+relayTestToken)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// assertOpenAIErrorEnvelope 断言响应是 /v1 唯一的 OpenAI 错误信封（relay-01
// §3.6），而不是根 mux 的纯文本 404。
func assertOpenAIErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) relayErrorBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json; body=%s", ct, rec.Body.String())
	}
	var envelope relayErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not an JSON error envelope: %v; body=%s", err, rec.Body.String())
	}
	if envelope.Error.Type != relayErrorType {
		t.Fatalf("error.type = %q, want %q", envelope.Error.Type, relayErrorType)
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q", envelope.Error.Code, wantCode)
	}
	if envelope.Error.Message == "" {
		t.Fatal("error.message must not be empty")
	}
	return envelope.Error
}

// TestPluginRouteUnknownPathIs404Envelope 对应 brief Step 1：未声明的路径必须
// 404 + OpenAI 信封，绝不能 500 或落到通用中继。
func TestPluginRouteUnknownPathIs404Envelope(t *testing.T) {
	mux := newNativeRelayMux(t, &recordingNativeDispatcher{manager: newNativeRouteManager(t)})

	rec := doNative(t, mux, http.MethodGet, "/v1/plugin-not-declared", "", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertOpenAIErrorEnvelope(t, rec, "invalid_request_error")
}

// TestPluginRouteUnknownPathWithoutTokenStill404：未知路径的可见性与兜底上线
// 之前一致（根 mux 原本就 404），不因缺少令牌变成 401。这里注入**非空**索引，
// 确保走的是「索引未命中」分支而不是「无分发面直接 404」的短路。
func TestPluginRouteUnknownPathWithoutTokenStill404(t *testing.T) {
	dispatcher := &recordingNativeDispatcher{manager: newNativeRouteManager(t)}
	mux := newNativeRelayMux(t, dispatcher)

	rec := doNative(t, mux, http.MethodGet, "/v1/plugin-not-declared", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertOpenAIErrorEnvelope(t, rec, "invalid_request_error")
	if dispatcher.callCount() != 0 {
		t.Fatal("unknown path must not reach the native dispatcher")
	}
}

// TestPluginNativeRouteDispatches 证明声明的精确路由被兜底交给原生分发面。
func TestPluginNativeRouteDispatches(t *testing.T) {
	dispatcher := &recordingNativeDispatcher{manager: newNativeRouteManager(t)}
	mux := newNativeRelayMux(t, dispatcher)

	rec := doNative(t, mux, http.MethodPost, "/v1/video-native", `{"model":"video-model"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dispatched); body=%s", rec.Code, rec.Body.String())
	}
	match, ok := dispatcher.last()
	if !ok {
		t.Fatal("dispatcher was not called")
	}
	if match.PluginKey != "video" || match.Route.Kind != "submit" {
		t.Fatalf("unexpected match: %+v", match)
	}
	if len(match.PathParams) != 0 {
		t.Fatalf("exact route must not carry params: %v", match.PathParams)
	}
}

// TestPluginNativeRouteParamCapture：带参数的路由把路径参数交给分发面。
func TestPluginNativeRouteParamCapture(t *testing.T) {
	dispatcher := &recordingNativeDispatcher{manager: newNativeRouteManager(t)}
	mux := newNativeRelayMux(t, dispatcher)

	rec := doNative(t, mux, http.MethodGet, "/v1/suno/fetch/task_abc", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	match, ok := dispatcher.last()
	if !ok {
		t.Fatal("dispatcher was not called")
	}
	if match.Route.Kind != "query" || match.PathParams["task_id"] != "task_abc" {
		t.Fatalf("unexpected match: %+v", match)
	}
}

// TestPluginNativeRouteRequiresAuth：命中原生路由才鉴权；无令牌不得分发。
func TestPluginNativeRouteRequiresAuth(t *testing.T) {
	dispatcher := &recordingNativeDispatcher{manager: newNativeRouteManager(t)}
	mux := newNativeRelayMux(t, dispatcher)

	rec := doNative(t, mux, http.MethodPost, "/v1/video-native", `{"model":"video-model"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if dispatcher.callCount() != 0 {
		t.Fatal("unauthenticated request must not reach the native dispatcher")
	}
}

// TestPluginFallbackDoesNotSwallowExactRoutes 是 R-04 的硬性证据：
// `/v1/` 兜底上线后，三条既有精确路由仍必须命中各自的原 handler。
func TestPluginFallbackDoesNotSwallowExactRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called in this test")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	taskHandler, _ := newTaskTestHandler(t, &fakeTaskStore{}, &fakeTaskChannels{}, sampleTaskManager(t), nil)

	mux := http.NewServeMux()
	env.handler.Register(mux) // 含 /v1/ 兜底
	taskHandler.RegisterRelay(mux, env.handler.AuthMiddleware())
	dispatcher := &recordingNativeDispatcher{manager: newNativeRouteManager(t)}
	// 换入一个非空索引，确保兜底确实「有能力」抢路由，而不是因为索引为空
	// 才碰巧放行精确路由。
	env.handler.nativeRoutes = dispatcher

	t.Run("POST /v1/chat/completions", func(t *testing.T) {
		rec := doNative(t, mux, http.MethodPost, "/v1/chat/completions", `{"model":`, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 from chat handler; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("body = %s, want chat handler's invalid_request", rec.Body.String())
		}
	})

	t.Run("GET /v1/models", func(t *testing.T) {
		rec := doNative(t, mux, http.MethodGet, "/v1/models", "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 from models handler; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "gpt-test") {
			t.Fatalf("body = %s, want models list", rec.Body.String())
		}
	})

	t.Run("POST /v1/tasks/{key}", func(t *testing.T) {
		rec := doNative(t, mux, http.MethodPost, "/v1/tasks/sample", "not-json", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 from tasks handler; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "request body must be a JSON object") {
			t.Fatalf("body = %s, want tasks handler's body validation error", rec.Body.String())
		}
	})

	if dispatcher.callCount() != 0 {
		t.Fatalf("native dispatcher must not see exact routes: %v", dispatcher.calls)
	}
}

// nativeTaskEnv 是「中继面 + 真实 TasksHandler」的端到端装配：
// RelayAuth（中继）→ /v1/ 兜底 → DispatchNativeRoute → handleSubmit/handleGet。
type nativeTaskEnv struct {
	handler *TasksHandler
	mux     *http.ServeMux
	store   *fakeTaskStore
	billing *taskBillingRecorder
}

func newNativeTaskEnv(t *testing.T) *nativeTaskEnv {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_native"}`))
	}))
	t.Cleanup(upstream.Close)

	taskSealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, taskSealer, "sk-secret"),
	}}
	billing := newTaskBillingRecorder()
	taskHandler, _ := newTaskTestHandler(t, store, channels, newNativeRouteManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing })

	relayHandler := NewRelayHandler(RelayHandlerOptions{
		Channels: &fakeRelayChannelStore{models: []string{"gpt-test"}},
		// 身份必须与 newTaskTestHandler 的 fakeTaskTokens（ID 1 / UserID 42）
		// 完全一致：BuildRelayContext 会拒绝两次解析指向不同行的令牌。
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: 1, UserID: 42, Status: "enabled", UnlimitedQuota: true,
		}),
		Access:       &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:       NewSecretSealer(relaySealerMaster),
		NativeRoutes: taskHandler,
	})
	mux := http.NewServeMux()
	relayHandler.Register(mux)
	taskHandler.RegisterRelay(mux, relayHandler.AuthMiddleware())
	return &nativeTaskEnv{handler: taskHandler, mux: mux, store: store, billing: billing}
}

func (e *nativeTaskEnv) submitVideo(t *testing.T) string {
	t.Helper()
	rec := doNative(t, e.mux, http.MethodPost, "/v1/video-native",
		`{"model":"video-model","prompt":"hi","seconds":5}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var resp taskSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.PublicTaskID == "" {
		t.Fatalf("public_task_id must not be empty: %s", rec.Body.String())
	}
	return resp.PublicTaskID
}

// TestPluginNativeRouteReusesTaskSubmitPath 是「复用提交流程」的端到端证据：
// 原生路由命中后走 TasksHandler 的 handleSubmit（建任务行 + 预扣 + 提交上游），
// 而不是任何单独的转发实现。
func TestPluginNativeRouteReusesTaskSubmitPath(t *testing.T) {
	env := newNativeTaskEnv(t)
	env.submitVideo(t)

	if env.store.inserted != 1 {
		t.Fatalf("inserted = %d, want 1", env.store.inserted)
	}
	row := env.store.rows[0]
	if row.PluginKey != "video" {
		t.Fatalf("plugin key = %q, want video (route index must drive submission)", row.PluginKey)
	}
	if row.UserID != 42 {
		t.Fatalf("user id = %d, want 42 (RelayAuth identity must survive dispatch)", row.UserID)
	}
	calls := env.billing.ReserveCalls()
	if len(calls) == 0 || calls[0] != "preconsume" {
		t.Fatalf("billing calls = %v, want preconsume as the first step", calls)
	}
}

// TestPluginNativeRouteParamNamedKeyDoesNotOverridePluginKey 覆盖复核 Minor-1：
// manifest 把路径参数命名为 `{key}` 时，URL 段不得覆盖路由索引给出的插件 key。
func TestPluginNativeRouteParamNamedKeyDoesNotOverridePluginKey(t *testing.T) {
	env := newNativeTaskEnv(t)

	rec := doNative(t, env.mux, http.MethodPost, "/v1/videos/evil-plugin/remix",
		`{"model":"video-model","seconds":5}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if env.store.inserted != 1 {
		t.Fatalf("inserted = %d, want 1", env.store.inserted)
	}
	if got := env.store.rows[0].PluginKey; got != "video" {
		t.Fatalf("plugin key = %q, want video: URL segment {key} must not override the indexed key", got)
	}
}

// TestPluginNativeQueryRouteDispatchesToTaskLookup 覆盖复核 Minor-3：
// query 原生路由必须把 taskIdParam 注入 ?task_id= 并复用 handleGet。
// 若把它改成走 handleSubmit（GET + 空 body），本用例会以 400 失败。
func TestPluginNativeQueryRouteDispatchesToTaskLookup(t *testing.T) {
	env := newNativeTaskEnv(t)
	publicID := env.submitVideo(t)

	rec := doNative(t, env.mux, http.MethodGet, "/v1/suno/fetch/"+publicID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp taskQueryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode query response: %v", err)
	}
	if resp.TaskID != publicID {
		t.Fatalf("task_id = %q, want %q", resp.TaskID, publicID)
	}

	// 缺参分支：路由声明的 taskIdParam 在 PathParams 里不存在时必须 400，
	// 而不是把空 task_id 交给查询层（那会变成 404，混淆配置错误与不存在）。
	// 直接调用分发面，避免为这条分支再造一份 manifest。
	req := httptest.NewRequest(http.MethodGet, "/v1/suno/fetch/x", nil)
	missing := httptest.NewRecorder()
	env.handler.DispatchNativeRoute(missing, req, plugin.RouteMatch{
		PluginKey:  "video",
		Route:      plugin.Route{Method: http.MethodGet, Path: "/v1/suno/fetch/:task_id", Kind: "query", TaskIDParam: "missing"},
		PathParams: map[string]string{"task_id": "x"},
	})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing task id: status = %d, want 400; body=%s", missing.Code, missing.Body.String())
	}
	assertOpenAIErrorEnvelope(t, missing, "invalid_request_error")
}

// TestPluginNativeRouteUnknownKindIsRejected 覆盖复核 Minor-2：未知 kind 不得
// 静默落入提交路径（加载期已拒绝，但索引与校验分叉时必须 fail-closed）。
func TestPluginNativeRouteUnknownKindIsRejected(t *testing.T) {
	env := newNativeTaskEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/video-native", strings.NewReader(`{"model":"video-model"}`))
	rec := httptest.NewRecorder()
	env.handler.DispatchNativeRoute(rec, req, plugin.RouteMatch{
		PluginKey: "video",
		Route:     plugin.Route{Method: http.MethodPost, Path: "/v1/video-native", Kind: "bogus"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if env.store.inserted != 0 {
		t.Fatalf("inserted = %d, want 0 (unknown kind must not submit)", env.store.inserted)
	}
	assertOpenAIErrorEnvelope(t, rec, "internal_error")
}

// TestPluginFallbackPreservesMethodNotAllowed：`/v1/` 匹配所有方法，兜底必须
// 自己补回 ServeMux 的 405 语义（否则 POST /v1beta/openai/models 会退化成 404）。
//
// R-W18-12 删除了 GET /v1/realtime 半死桩，因此这里改用仍存在的 GET-only
// 内建路由（Gemini 兼容模型列表）验证同一语义。
func TestPluginFallbackPreservesMethodNotAllowed(t *testing.T) {
	mux := newNativeRelayMux(t, &recordingNativeDispatcher{manager: newNativeRouteManager(t)})

	// GET /v1beta/openai/models 是 GET-only 内建路由；POST 同路径必须 405。
	rec := doNative(t, mux, http.MethodPost, "/v1beta/openai/models", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

// TestPluginCannotClaimRemovedEndpointFamilies 覆盖 R-W18-01 的 HTTP 面：
// 插件在 routes[] 里声明 /v1/files/x 必须在注册期整插件失败，请求落到
// /v1/ 兜底的 404，且**不经 RelayAuth**（无令牌也 404，不是 401）、不进入
// 任何分发面。否则 AC-8 的「任何方法 404 且不经鉴权链」会被插件路由破坏。
func TestPluginCannotClaimRemovedEndpointFamilies(t *testing.T) {
	// 在既有测试插件源的路由数组开头插入一条保留族路由；其余元数据与钩子
	// 保持有效，因此失败只能归因于保留族冲突（而不是插件本身编译不过）。
	source := strings.Replace(nativeRoutePluginSource, "routes: [",
		"routes: [\n    { method: \"POST\", path: \"/v1/files/x\", kind: \"submit\", decode: \"buildSubmitRequest\", render: \"parseSubmitResponse\" },", 1)
	manager, err := plugin.NewManager(map[string]string{"video": source}, nil, plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	entry := manager.Snapshot().Lookup("video")
	if entry.Status != plugin.StatusCompileFailed {
		t.Fatalf("声明保留族路由的插件必须 compile_failed: %+v", entry)
	}
	dispatcher := &recordingNativeDispatcher{manager: manager}
	mux := newNativeRelayMux(t, dispatcher)
	for _, authenticated := range []bool{false, true} {
		rec := doNative(t, mux, http.MethodPost, "/v1/files/x", `{}`, authenticated)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST /v1/files/x (auth=%v): status = %d, want 404; body=%s",
				authenticated, rec.Code, rec.Body.String())
		}
		assertOpenAIErrorEnvelope(t, rec, "invalid_request_error")
	}
	if dispatcher.callCount() != 0 {
		t.Fatalf("native dispatch calls = %d, want 0", dispatcher.callCount())
	}
}
