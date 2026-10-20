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
    { method: "POST", path: "/v1/videos", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" },
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
// 之前一致（根 mux 原本就 404），不因缺少令牌变成 401。
func TestPluginRouteUnknownPathWithoutTokenStill404(t *testing.T) {
	mux := newNativeRelayMux(t, nil)

	rec := doNative(t, mux, http.MethodGet, "/v1/plugin-not-declared", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertOpenAIErrorEnvelope(t, rec, "invalid_request_error")
}

// TestPluginNativeRouteDispatches 证明声明的精确路由被兜底交给原生分发面。
func TestPluginNativeRouteDispatches(t *testing.T) {
	dispatcher := &recordingNativeDispatcher{manager: newNativeRouteManager(t)}
	mux := newNativeRelayMux(t, dispatcher)

	rec := doNative(t, mux, http.MethodPost, "/v1/videos", `{"model":"video-model"}`, true)
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

	rec := doNative(t, mux, http.MethodPost, "/v1/videos", `{"model":"video-model"}`, false)
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

// TestPluginNativeRouteReusesTaskSubmitPath 是「复用提交流程」的端到端证据：
// 原生路由命中后走 TasksHandler 的 handleSubmit（建任务行 + 预扣 + 提交上游），
// 而不是任何单独的转发实现。
func TestPluginNativeRouteReusesTaskSubmitPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_native"}`))
	}))
	defer upstream.Close()

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

	rec := doNative(t, mux, http.MethodPost, "/v1/videos",
		`{"model":"video-model","prompt":"hi","seconds":5}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 1 {
		t.Fatalf("inserted = %d, want 1", store.inserted)
	}
	row := store.rows[0]
	if row.PluginKey != "video" {
		t.Fatalf("plugin key = %q, want video (route index must drive submission)", row.PluginKey)
	}
	if row.UserID != 42 {
		t.Fatalf("user id = %d, want 42 (RelayAuth identity must survive dispatch)", row.UserID)
	}
	calls := billing.ReserveCalls()
	if len(calls) == 0 || calls[0] != "preconsume" {
		t.Fatalf("billing calls = %v, want preconsume as the first step", calls)
	}
}

// TestPluginFallbackPreservesMethodNotAllowed：`/v1/` 匹配所有方法，兜底必须
// 自己补回 ServeMux 的 405 语义（否则 POST /v1/realtime 会退化成 404）。
func TestPluginFallbackPreservesMethodNotAllowed(t *testing.T) {
	mux := newNativeRelayMux(t, &recordingNativeDispatcher{manager: newNativeRouteManager(t)})

	// GET /v1/realtime 是多模态族注册的 GET-only 路由；POST 同路径必须 405。
	rec := doNative(t, mux, http.MethodPost, "/v1/realtime", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}
