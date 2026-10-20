package httpapi

// 本文件是 T2（`POST /v1/responses`）的 HTTP 面验收：
//   - P-01：插件原生路由命中 → 走任务插件提交，且**不产生**通用中继的预扣
//     （计费只发生一次）；
//   - R-13 回退：未命中插件路由 → 501 problem+json（fail-closed，零计费、
//     零上游调用）；
//   - R-03：分发复用 T1 的 RouteIndex（这里用真实 plugin.Manager 的快照驱动）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tasks"
)

// responsesPluginSource 声明 `POST /v1/responses` 为原生 submit 路由。
// 元数据与 T1 的测试插件同形，只把路由换成 Responses 创建端点。
const responsesPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
    { method: "POST", path: "/v1/responses", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" }
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

// upstreamResponsesBody 是假上游在 /chat/completions 返回的 Responses 形态对象
// （刻意不含 choices/usage）。它**不是** Responses 端点的合法出口：R-13 之后
// 未命中插件路由一律 501，通用中继绝不把这份 body 送到客户端。保留它是因为
// 「旧实现尽力转发」的回归会以 200 + 这份 body 立刻暴露在 fail-closed 用例里。
const upstreamResponsesBody = `{"id":"resp_up","object":"response","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`

func newResponsesPluginManager(t *testing.T) *plugin.Manager {
	t.Helper()
	manager, err := plugin.NewManager(map[string]string{"video": responsesPluginSource}, nil,
		plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return manager
}

// responsesEnv 是「中继面（含 POST /v1/responses）+ 真实 TasksHandler」的装配。
// 两套计费记录器刻意分开：taskBilling 是任务提交路径的会话，relayBilling 是
// 通用中继的会话——P-01 的「只扣一次」就是断言同一请求只出现在其中一边。
type responsesEnv struct {
	mux          *http.ServeMux
	store        *fakeTaskStore
	taskBilling  *taskBillingRecorder
	relayBilling *recordingBilling
	transport    *countingTransport
}

// countingTransport 记录通用中继实际发往上游的请求（C1/R-13 的证据面）：
// fail-closed 分支必须在任何上游调用之前拒绝，计数必须为 0。
// 任务提交路径走 TasksHandler 自己的客户端，不经这里——两套计费/执行面
// 互斥正是 P-01 要证明的。
type countingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen []string
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.seen = append(t.seen, req.Method+" "+req.URL.Path)
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}

func (t *countingTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}

func (t *countingTransport) paths() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.seen...)
}

func newResponsesEnv(t *testing.T, manager *plugin.Manager) *responsesEnv {
	t.Helper()
	// 同一个上游服务两种角色：任务提交（/tasks）与通用中继（/chat/completions）。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/chat/completions") {
			_, _ = w.Write([]byte(upstreamResponsesBody))
			return
		}
		_, _ = w.Write([]byte(`{"id":"up_native"}`))
	}))
	t.Cleanup(upstream.Close)

	taskSealer := NewSecretSealer("task-handler-test-key")
	taskStore := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, taskSealer, "sk-secret"),
	}}
	taskBilling := newTaskBillingRecorder()
	taskHandler, _ := newTaskTestHandler(t, taskStore, channels, manager,
		func(*contract.RelayContext) contract.BillingSession { return taskBilling })

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	relayChannels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{ChannelID: int(relayTestChannelID), Priority: 10, Weight: 1}},
		channels: map[int64]*store.Channel{
			relayTestChannelID: {
				ID:              relayTestChannelID,
				Name:            "test-openai",
				Type:            string(contract.ChannelTypeOpenAI),
				BaseURL:         &baseURL,
				Credentials:     sealedTestCredentials(t, sealer),
				SupportedModels: []byte(`["gpt-test"]`),
				ModelMapping:    []byte(`{}`),
				Status:          1,
				CreatedAt:       time.Now().UTC(),
			},
		},
		models: []string{"gpt-test"},
	}
	relayBilling := newRecordingBilling()
	transport := &countingTransport{base: upstream.Client().Transport}
	// 身份必须与 newTaskTestHandler 的 fakeTaskTokens（ID 1 / UserID 42）一致：
	// BuildRelayContext 会拒绝两次解析指向不同行的令牌。
	relayHandler := NewRelayHandler(RelayHandlerOptions{
		Channels: relayChannels,
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: 1, UserID: 42, Status: "enabled", UnlimitedQuota: true,
		}),
		Access:       &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:       sealer,
		Client:       &http.Client{Transport: transport},
		Billing:      func(*contract.RelayContext) contract.BillingSession { return relayBilling },
		NativeRoutes: taskHandler,
	})
	mux := http.NewServeMux()
	relayHandler.Register(mux)
	taskHandler.RegisterRelay(mux, relayHandler.AuthMiddleware())
	return &responsesEnv{mux: mux, store: taskStore, taskBilling: taskBilling,
		relayBilling: relayBilling, transport: transport}
}

func (e *responsesEnv) postResponses(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// TestResponsesPluginHitSkipsGenericPreConsume 是 brief Step 1：
// 插件声明 POST /v1/responses 时，请求必须走任务插件提交（任务行 +1、任务
// 计费发生一次），并且**不得**进入通用中继——通用中继的预扣次数必须为 0。
func TestResponsesPluginHitSkipsGenericPreConsume(t *testing.T) {
	env := newResponsesEnv(t, newResponsesPluginManager(t))

	rec := env.postResponses(t, `{"model":"video-model","input":"hello","seconds":5}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (task submit); body=%s", rec.Code, rec.Body.String())
	}

	if env.store.inserted != 1 {
		t.Fatalf("tasks inserted = %d, want 1", env.store.inserted)
	}
	if got := env.store.rows[0].PluginKey; got != "video" {
		t.Fatalf("plugin key = %q, want video", got)
	}
	calls := env.taskBilling.ReserveCalls()
	if len(calls) == 0 || calls[0] != "preconsume" {
		t.Fatalf("task billing calls = %v, want preconsume", calls)
	}

	// P-01 的核心断言：插件分支不得再进入通用中继的预扣。
	if n := env.relayBilling.preConsumeCount(); n != 0 {
		t.Fatalf("generic relay pre-consume count = %d, want 0 (billing must happen exactly once)", n)
	}
	if n := env.relayBilling.terminateCount(); n != 0 {
		t.Fatalf("generic relay terminate count = %d, want 0", n)
	}
	if n := env.relayBilling.refundCount(); n != 0 {
		t.Fatalf("generic relay refund count = %d, want 0", n)
	}
	if n := env.transport.count(); n != 0 {
		t.Fatalf("generic relay upstream calls = %d, want 0; calls=%v", n, env.transport.paths())
	}

	var resp taskSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	if resp.PublicTaskID == "" {
		t.Fatalf("public_task_id must not be empty: %s", rec.Body.String())
	}
}

// TestResponsesFallbackFailsClosedWithoutBilling 是 R-13 的验收：插件未命中时
// 通用回退必须 fail-closed（501 problem+json），且在**任何计费/上游调用之前**
// 拒绝。假上游在 /chat/completions 返回 chat 对象，若旧实现（尽力转发）回归，
// 本用例会以 200 + 非零计费/上游调用失败。
func TestResponsesFallbackFailsClosedWithoutBilling(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-test","input":"hello"}`,
		`{"model":"gpt-test","input":"hello","stream":true}`,
		`not-json`,
	} {
		t.Run(body, func(t *testing.T) {
			// T1 的 video 插件只声明 /v1/video-native：索引非空，但 /v1/responses 未命中。
			env := newResponsesEnv(t, newNativeRouteManager(t))

			rec := env.postResponses(t, body)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501 (fail-closed); body=%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			var problem struct {
				Type   string `json:"type"`
				Title  string `json:"title"`
				Status int    `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v; body=%s", err, rec.Body.String())
			}
			if problem.Type != problemTypeEndpointNotSupported || problem.Status != http.StatusNotImplemented {
				t.Fatalf("problem = %+v, want type=%s status=501", problem, problemTypeEndpointNotSupported)
			}
			if !strings.Contains(problem.Title, "plugin task channel") {
				t.Fatalf("title = %q, want it to point at plugin task channels", problem.Title)
			}
			if rec.Header().Get("X-Request-ID") == "" {
				t.Fatal("X-Request-ID must be set on the fail-closed response")
			}

			// 零副作用：两套计费会话都没有任何调用、没有任务行、上游零请求。
			if n := env.relayBilling.preConsumeCount(); n != 0 {
				t.Fatalf("generic relay pre-consume = %d, want 0", n)
			}
			if n := env.relayBilling.terminateCount(); n != 0 {
				t.Fatalf("generic relay terminate = %d, want 0", n)
			}
			if n := env.relayBilling.refundCount(); n != 0 {
				t.Fatalf("generic relay refund = %d, want 0", n)
			}
			if calls := env.taskBilling.ReserveCalls(); len(calls) != 0 {
				t.Fatalf("task billing calls = %v, want none", calls)
			}
			if env.store.inserted != 0 {
				t.Fatalf("tasks inserted = %d, want 0", env.store.inserted)
			}
			if n := env.transport.count(); n != 0 {
				t.Fatalf("upstream requests = %d, want 0 (reject before any upstream call); calls=%v",
					n, env.transport.paths())
			}
		})
	}
}

// TestResponsesWithoutDispatcherStillFailsClosed：未装配插件分发面（探针/无任务
// 数据面）时走同一条 fail-closed 分支——501 且不触上游，不 panic、不 404。
func TestResponsesWithoutDispatcherStillFailsClosed(t *testing.T) {
	transport := &countingTransport{base: http.DefaultTransport}
	h := NewRelayHandler(RelayHandlerOptions{
		Channels: &fakeRelayChannelStore{models: []string{"gpt-test"}},
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: relayTestTokenID, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
		}),
		Access: &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer: NewSecretSealer(relaySealerMaster),
		Client: &http.Client{Transport: transport},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	if n := transport.count(); n != 0 {
		t.Fatalf("upstream requests = %d, want 0", n)
	}
}
