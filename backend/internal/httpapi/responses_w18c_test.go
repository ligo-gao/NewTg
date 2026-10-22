package httpapi

// W18-C 的 HTTP 面验收（R-W18-07）：
//   - protocols 派生绑定（routes[] 为空）即可命中 create；
//   - create 返回 Responses 对象（不是 202 task 形状）；
//   - stream:true 返回 SSE，事件顺序 created → delta → completed，completed 带 usage；
//   - supports 收窄：只声明 retrieve 的插件不得接管 create。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/tasks"
)

// responsesW18CPluginSource 演示「只声明 protocols、不写 routes[]」：
// create 与 retrieve 都由 supports 派生。
const responsesW18CPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "video",
  name: "Video",
  version: "1.1.0",
  author: { name: "NewTG" },
  description: { en: "Video task plugin", zh: "视频任务插件" },
  models: ["video-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [],
  routes: [],
  protocols: { openai_responses: { supports: ["create", "retrieve"] } },
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  decodeRequest: function (ctx) { return ctx.body; },
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel } };
  },
  parseSubmitResponse: function () {
    return {
      taskId: "up_w18c",
      immediate: { status: "SUCCESS" },
      taskData: { usage: { input_tokens: 5, output_tokens: 7 } }
    };
  },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; },
  renderFinal: function () {
    return {
      output: [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "hello" }] }],
      usage: { input_tokens: 5, output_tokens: 7 }
    };
  },
  renderEvents: function () {
    return [{ type: "response.output_text.delta", delta: "hello", output_index: 0, content_index: 0 }];
  }
};`

type responsesW18CEnv struct {
	mux     http.Handler
	store   *fakeTaskStore
	billing *taskBillingRecorder
}

func newResponsesW18CEnv(t *testing.T, source string) *responsesW18CEnv {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_w18c"}`))
	}))
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-secret"),
	}}
	billing := newTaskBillingRecorder()
	manager := newTaskManager(t, map[string]string{"video": source})
	taskHandler, _ := newTaskTestHandler(t, store, channels, manager,
		func(*contract.RelayContext) contract.BillingSession { return billing })

	relayHandler := NewRelayHandler(RelayHandlerOptions{
		Channels: &fakeRelayChannelStore{models: []string{"gpt-test"}},
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: 1, UserID: 42, Status: "enabled", UnlimitedQuota: true,
		}),
		Access:       &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:       sealer,
		NativeRoutes: taskHandler,
	})
	mux := http.NewServeMux()
	relayHandler.Register(mux)
	taskHandler.RegisterRelay(mux, relayHandler.AuthMiddleware())
	return &responsesW18CEnv{mux: mux, store: store, billing: billing}
}

func (e *responsesW18CEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestResponsesCreateReturnsResponsesObject(t *testing.T) {
	env := newResponsesW18CEnv(t, responsesW18CPluginSource)
	rec := env.post(t, `{"model":"video-model","input":"hello","seconds":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var object struct {
		ID        string           `json:"id"`
		Object    string           `json:"object"`
		Status    string           `json:"status"`
		Model     string           `json:"model"`
		CreatedAt int64            `json:"created_at"`
		Output    []map[string]any `json:"output"`
		Usage     map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode Responses object: %v; body=%s", err, rec.Body.String())
	}
	if object.Object != "response" {
		t.Fatalf("object = %q, want response; body=%s", object.Object, rec.Body.String())
	}
	if !strings.HasPrefix(object.ID, "resp_") {
		t.Fatalf("id = %q, want resp_ prefix", object.ID)
	}
	if object.Status != "completed" {
		t.Fatalf("status = %q, want completed", object.Status)
	}
	if object.CreatedAt == 0 || object.Model != "video-model" {
		t.Fatalf("identity fields = %#v", object)
	}
	if len(object.Output) != 1 {
		t.Fatalf("output = %#v, want renderFinal array", object.Output)
	}
	if object.Usage["input_tokens"] != float64(5) || object.Usage["output_tokens"] != float64(7) ||
		object.Usage["total_tokens"] != float64(12) {
		t.Fatalf("usage = %#v, want input/output/total token usage", object.Usage)
	}
	// 不得回退到 task 202 形状。
	if strings.Contains(rec.Body.String(), "public_task_id") {
		t.Fatalf("create must not return the task shape: %s", rec.Body.String())
	}

	// 提交/结算仍只发生一次（decode/render 不改计费语义）。
	if env.store.inserted != 1 {
		t.Fatalf("tasks inserted = %d, want 1", env.store.inserted)
	}
	calls := env.billing.ReserveCalls()
	preConsumes := 0
	for _, call := range calls {
		if call == "preconsume" {
			preConsumes++
		}
	}
	if preConsumes != 1 {
		t.Fatalf("preconsume calls = %v, want exactly one", calls)
	}
}

func TestResponsesCreateStreamReturnsSSE(t *testing.T) {
	env := newResponsesW18CEnv(t, responsesW18CPluginSource)
	rec := env.post(t, `{"model":"video-model","input":"hello","seconds":5,"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.completed",
		`"usage"`,
		`"total_tokens":12`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	// 顺序不可乱：created 第一、completed 最后。
	created := strings.Index(body, "event: response.created")
	delta := strings.Index(body, "event: response.output_text.delta")
	completed := strings.Index(body, "event: response.completed")
	if !(created >= 0 && created < delta && delta < completed) {
		t.Fatalf("event order wrong: created=%d delta=%d completed=%d\n%s", created, delta, completed, body)
	}
}

// TestResponsesCreateParameterErrorsUseRelayEnvelope：create 的参数错误必须是
// 400 OpenAI 信封（不是 task 形状，也不是 problem+json），且零计费/零落库。
func TestResponsesCreateParameterErrorsUseRelayEnvelope(t *testing.T) {
	env := newResponsesW18CEnv(t, responsesW18CPluginSource)
	for _, body := range []string{
		`{"input":"hi","seconds":5}`, // model 缺失
		`not-json`,
	} {
		rec := env.post(t, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("body %q: Content-Type = %q, want application/json", body, ct)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("body %q: decode error envelope: %v; body=%s", body, err, rec.Body.String())
		}
		if envelope.Error.Code != "invalid_request_error" {
			t.Fatalf("body %q: code = %q, want invalid_request_error", body, envelope.Error.Code)
		}
	}
	if env.store.inserted != 0 {
		t.Fatalf("tasks inserted = %d, want 0", env.store.inserted)
	}
}

// TestResponsesCreateOnlyPluginDoesNotClaimCreate 是 supports 收窄的 HTTP 面：
// 只声明 retrieve 的插件注册成功，但 POST /v1/responses 不得由它接管。
func TestResponsesCreateOnlyPluginDoesNotClaimCreate(t *testing.T) {
	retrieveOnly := strings.Replace(responsesW18CPluginSource,
		`supports: ["create", "retrieve"]`, `supports: ["retrieve"]`, 1)
	env := newResponsesW18CEnv(t, retrieveOnly)
	rec := env.post(t, `{"model":"video-model","input":"hello","seconds":5}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (create not claimed); body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if envelope.Error.Code != "endpoint_not_supported" {
		t.Fatalf("code = %q, want endpoint_not_supported", envelope.Error.Code)
	}
	if env.store.inserted != 0 {
		t.Fatalf("tasks inserted = %d, want 0", env.store.inserted)
	}
}
