package httpapi

// 本文件是 T2（`POST /v1/responses`）的 HTTP 面验收：
//   - P-01：插件原生路由命中 → 走任务插件提交，且**不产生**通用中继的预扣
//     （计费只发生一次）；
//   - P-01 回退：未命中插件路由 → 回落 relay.FormatOpenAIResponses 的通用中继，
//     上游 Responses 形态的响应体逐字节返回；
//   - R-03：分发复用 T1 的 RouteIndex（这里用真实 plugin.Manager 的快照驱动）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// upstreamResponsesBody 是上游返回的 Responses 对象（带 input/output 形状字段，
// 刻意不含 choices/usage——它必须被逐字节送回客户端，而不是被 chat 编解码器改写）。
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
	// 身份必须与 newTaskTestHandler 的 fakeTaskTokens（ID 1 / UserID 42）一致：
	// BuildRelayContext 会拒绝两次解析指向不同行的令牌。
	relayHandler := NewRelayHandler(RelayHandlerOptions{
		Channels: relayChannels,
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: 1, UserID: 42, Status: "enabled", UnlimitedQuota: true,
		}),
		Access:       &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:       sealer,
		Client:       upstream.Client(),
		Billing:      func(*contract.RelayContext) contract.BillingSession { return relayBilling },
		NativeRoutes: taskHandler,
	})
	mux := http.NewServeMux()
	relayHandler.Register(mux)
	taskHandler.RegisterRelay(mux, relayHandler.AuthMiddleware())
	return &responsesEnv{mux: mux, store: taskStore, taskBilling: taskBilling, relayBilling: relayBilling}
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

	var resp taskSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	if resp.PublicTaskID == "" {
		t.Fatalf("public_task_id must not be empty: %s", rec.Body.String())
	}
}

// TestResponsesFallbackKeepsResponsesShape 是 brief Step 2：索引存在但没有声明
// `/v1/responses` 时，请求必须回落通用中继（而不是 404），且上游的 Responses
// 响应体原样返回（不被 chat 编解码器改写）。同时断言计费只经由中继会话发生一次。
func TestResponsesFallbackKeepsResponsesShape(t *testing.T) {
	// T1 的 video 插件只声明 /v1/videos：索引非空，但 Match 未命中。
	env := newResponsesEnv(t, newNativeRouteManager(t))

	rec := env.postResponses(t, `{"model":"gpt-test","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (generic relay); body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got := rec.Body.String(); got != upstreamResponsesBody {
		t.Fatalf("body = %s, want upstream Responses body verbatim %s", got, upstreamResponsesBody)
	}

	if env.store.inserted != 0 {
		t.Fatalf("tasks inserted = %d, want 0 (fallback must not create task rows)", env.store.inserted)
	}
	if calls := env.taskBilling.ReserveCalls(); len(calls) != 0 {
		t.Fatalf("task billing calls = %v, want none on the fallback path", calls)
	}
	if n := env.relayBilling.preConsumeCount(); n != 1 {
		t.Fatalf("generic relay pre-consume count = %d, want 1", n)
	}
	if n := env.relayBilling.terminateCount(); n != 1 {
		t.Fatalf("generic relay terminate count = %d, want 1", n)
	}
	if fact := env.relayBilling.lastFact(); fact.Model != "gpt-test" {
		t.Fatalf("usage fact model = %q, want gpt-test (OriginModel must come from the Responses body)", fact.Model)
	}
}

// TestResponsesFallbackWithoutDispatcher：未装配插件分发面（探针/无任务数据面）
// 时，通用回退仍必须工作，不得因为索引缺失变成 404/panic。
func TestResponsesFallbackWithoutDispatcher(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamResponsesBody))
	}))
	t.Cleanup(upstream.Close)

	env := newRelayTestEnv(t, upstream, nil)
	rec := env.doAuthed(t, http.MethodPost, "/v1/responses", `{"model":"gpt-test","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamResponsesBody {
		t.Fatalf("body = %s, want %s", got, upstreamResponsesBody)
	}
}

// TestResponsesRejectsInvalidBody：入口校验必须在任何计费副作用之前完成。
func TestResponsesRejectsInvalidBody(t *testing.T) {
	cases := []string{
		`[]`,
		`not-json`,
		`{"model":"gpt-test","max_output_tokens":-1}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("upstream must not be called for an invalid request body")
			}))
			t.Cleanup(upstream.Close)

			env := newRelayTestEnv(t, upstream, nil)
			rec := env.doAuthed(t, http.MethodPost, "/v1/responses", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertOpenAIErrorEnvelope(t, rec, "invalid_request")
			if n := env.billing.preConsumeCount(); n != 0 {
				t.Fatalf("pre-consume count = %d, want 0 for an invalid body", n)
			}
		})
	}
}
