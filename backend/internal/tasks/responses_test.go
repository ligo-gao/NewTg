package tasks

// W18-C 渲染层与 decodeRequest 执行段的单测（R-W18-07）。
//
// 这些用例覆盖「钩子真的被执行」而不是只被校验存在：变异测试里删掉
// renderFinal/renderEvents 的执行会直接让本文件红。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"newtg/gateway/internal/plugin"
)

func compileResponsesTestPlugin(t *testing.T, key, source string) *plugin.CompiledPlugin {
	t.Helper()
	compiled, err := plugin.Compile(context.Background(), source, plugin.CompileOptions{ExpectedKey: key})
	if err != nil {
		t.Fatalf("compile plugin: %v", err)
	}
	return compiled
}

// responsesRenderPluginSource 的 renderFinal/renderEvents 返回可预测的载荷，
// 用于断言宿主如何合并身份字段与事件顺序。
const responsesRenderPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
  routes: [],
  protocols: { openai_responses: { supports: ["create", "retrieve"] } },
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel } }; },
  parseSubmitResponse: function () { return { taskId: "up_1", immediate: { status: "SUCCESS" }, taskData: { usage: { prompt_tokens: 5, completion_tokens: 7 } } }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; },
  decodeRequest: function (ctx) { return ctx.body; },
  renderFinal: function (ctx) {
    return {
      id: "plugin-id-must-not-win",
      object: "not-response",
      status: "failed",
      output: [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "hello" }] }],
      usage: { prompt_tokens: 5, completion_tokens: 7 }
    };
  },
  renderEvents: function () {
    return [
      { type: "response.created", response: { id: "plugin-created" } },
      { type: "response.output_text.delta", delta: "hel", output_index: 0, content_index: 0 },
      { type: "response.custom_vendor_event", payload: 1 },
      { type: "response.completed", response: { id: "plugin-completed" } }
    ];
  }
};`

func TestRenderResponsesFinalOwnsIdentityFields(t *testing.T) {
	compiled := compileResponsesTestPlugin(t, "video", responsesRenderPluginSource)
	in := ResponsesRenderInput{
		Plugin:     compiled,
		ResponseID: "resp_abc",
		Model:      "video-model",
		CreatedAt:  1700000000,
		Status:     ResponsesStatusCompleted,
		State:      StateSuccess,
		Data:       map[string]any{"usage": map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(7)}},
		Request:    map[string]any{"model": "video-model", "input": "hi"},
	}
	object, err := RenderResponsesFinal(context.Background(), in)
	if err != nil {
		t.Fatalf("RenderResponsesFinal: %v", err)
	}
	// 身份字段由宿主写死：插件返回的 id/object/status 一律不生效。
	if object["id"] != "resp_abc" || object["object"] != ResponsesObjectType || object["status"] != ResponsesStatusCompleted {
		t.Fatalf("host identity fields overwritten: %#v", object)
	}
	if object["created_at"] != int64(1700000000) || object["model"] != "video-model" {
		t.Fatalf("created_at/model = %#v, want host values", object)
	}
	output, ok := object["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %#v, want plugin array", object["output"])
	}
	usage, ok := object["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(7) || usage["total_tokens"] != float64(12) {
		t.Fatalf("usage = %#v, want normalized token usage", object["usage"])
	}
}

func TestRenderResponsesFinalFallsBackWithoutHooks(t *testing.T) {
	// 无 renderFinal 的插件仍得到合法 Responses 对象（必测的「无钩子回退」）。
	in := ResponsesRenderInput{
		ResponseID: "resp_abc", Model: "video-model", CreatedAt: 1,
		Status: ResponsesStatusQueued, State: StateSubmitted,
	}
	object, err := RenderResponsesFinal(context.Background(), in)
	if err != nil {
		t.Fatalf("RenderResponsesFinal: %v", err)
	}
	if object["object"] != ResponsesObjectType {
		t.Fatalf("object = %#v", object["object"])
	}
	if output, ok := object["output"].([]any); !ok || len(output) != 0 {
		t.Fatalf("output = %#v, want empty array", object["output"])
	}
}

func TestRenderResponsesFinalAsyncKeepsOutputEmpty(t *testing.T) {
	compiled := compileResponsesTestPlugin(t, "video", responsesRenderPluginSource)
	in := ResponsesRenderInput{
		Plugin: compiled, ResponseID: "resp_abc", Model: "video-model",
		Status: ResponsesStatusQueued, State: StateSubmitted,
	}
	object, err := RenderResponsesFinal(context.Background(), in)
	if err != nil {
		t.Fatalf("RenderResponsesFinal: %v", err)
	}
	// R-W18-07：异步语义保留时 status=in_progress/queued + output=[]。
	if output, ok := object["output"].([]any); !ok || len(output) != 0 {
		t.Fatalf("async output = %#v, want empty array", object["output"])
	}
}

func TestRenderResponsesEventsOrderingAndTermination(t *testing.T) {
	compiled := compileResponsesTestPlugin(t, "video", responsesRenderPluginSource)
	in := ResponsesRenderInput{
		Plugin: compiled, ResponseID: "resp_abc", Model: "video-model",
		CreatedAt: 1700000000, Status: ResponsesStatusCompleted, State: StateSuccess,
		Usage:   NormalizeResponsesUsage(map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(7)}),
		Request: map[string]any{"model": "video-model"},
	}
	events, err := RenderResponsesEvents(context.Background(), in)
	if err != nil {
		t.Fatalf("RenderResponsesEvents: %v", err)
	}
	if len(events) < 3 {
		t.Fatalf("events = %#v, want created + delta(s) + completed", events)
	}
	if events[0]["type"] != ResponsesEventCreated {
		t.Fatalf("first event = %#v, want response.created", events[0])
	}
	// 插件的 response.created 必须被丢弃，不能打乱顺序。
	if len(events) > 1 && events[1]["type"] == ResponsesEventCreated {
		t.Fatalf("plugin created event must be dropped: %#v", events)
	}
	// 未知事件保留（raw 透传语义）。
	foundUnknown := false
	for _, event := range events {
		if event["type"] == "response.custom_vendor_event" {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatalf("unknown event must pass through: %#v", events)
	}
	last := events[len(events)-1]
	if last["type"] != ResponsesEventCompleted {
		t.Fatalf("last event = %#v, want response.completed", last)
	}
	response, ok := last["response"].(map[string]any)
	if !ok {
		t.Fatalf("completed event response = %#v", last["response"])
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(12) {
		t.Fatalf("completed usage = %#v, want total_tokens=12", response["usage"])
	}
	// 终止事件里的身份字段仍是宿主值，而不是插件自报。
	if response["id"] != "resp_abc" {
		t.Fatalf("completed response id = %#v, want host resp_abc", response["id"])
	}
}

func TestRenderResponsesEventsFailureEndsWithFailed(t *testing.T) {
	compiled := compileResponsesTestPlugin(t, "video", responsesRenderPluginSource)
	in := ResponsesRenderInput{
		Plugin: compiled, ResponseID: "resp_abc", Model: "video-model",
		Status: ResponsesStatusFailed, State: StateFailure,
	}
	events, err := RenderResponsesEvents(context.Background(), in)
	if err != nil {
		t.Fatalf("RenderResponsesEvents: %v", err)
	}
	last := events[len(events)-1]
	if last["type"] != ResponsesEventFailed {
		t.Fatalf("last event = %#v, want response.failed", last)
	}
}

func TestRenderResponsesEventsAsyncHasNoTerminalEvent(t *testing.T) {
	compiled := compileResponsesTestPlugin(t, "video", responsesRenderPluginSource)
	in := ResponsesRenderInput{
		Plugin: compiled, ResponseID: "resp_abc", Model: "video-model",
		Status: ResponsesStatusInProgress, State: StateInProgress,
	}
	events, err := RenderResponsesEvents(context.Background(), in)
	if err != nil {
		t.Fatalf("RenderResponsesEvents: %v", err)
	}
	last := events[len(events)-1]
	if last["type"] != ResponsesEventInProgress {
		t.Fatalf("last event = %#v, want response.in_progress (no terminal)", last)
	}
}

func TestNormalizeResponsesUsage(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"openai", map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(7)},
			map[string]any{"input_tokens": float64(5), "output_tokens": float64(7), "total_tokens": float64(12)}},
		{"responses", map[string]any{"input_tokens": float64(2), "output_tokens": float64(3), "total_tokens": float64(9)},
			map[string]any{"input_tokens": float64(2), "output_tokens": float64(3), "total_tokens": float64(9)}},
		{"seconds_only", map[string]any{"seconds": float64(5)}, nil},
		{"empty", map[string]any{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeResponsesUsage(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("usage = %#v, want nil", got)
				}
				return
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("usage[%s] = %#v, want %#v (all=%#v)", key, got[key], want, got)
				}
			}
		})
	}
}

// TestSubmitExecutesDecodeRequest 证明 decodeRequest 在提交管线里真正执行：
// 上游收到的 body 必须是解码后的载荷，而不是原始 Responses 请求。
func TestSubmitExecutesDecodeRequest(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	defer upstream.Close()

	source := `globalThis.__NEWTG_PLUGIN__ = {
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
  routes: [],
  protocols: { openai_responses: { supports: ["create"] } },
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  decodeRequest: function (ctx) { return { decoded: ctx.body.input, model: ctx.model }; },
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel, payload: ctx.decoded } }; },
  parseSubmitResponse: function () { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; },
  renderFinal: function () { return { output: [] }; }
};`
	compiled := compileResponsesTestPlugin(t, "video", source)

	result, err := Submit(context.Background(), nil, SubmitInput{
		Plugin:       compiled,
		Channel:      ChannelTarget{ModelMapping: map[string]string{}},
		Model:        "video-model",
		Body:         map[string]any{"model": "video-model", "input": "hello"},
		BaseURL:      upstream.URL,
		APIKey:       "sk-test",
		AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.UpstreamTaskID != "up_1" {
		t.Fatalf("taskId = %q", result.UpstreamTaskID)
	}
	if gotBody["payload"] != "hello" {
		t.Fatalf("upstream body.payload = %#v, want decoded \"hello\" (decodeRequest not executed?)", gotBody["payload"])
	}
	if gotBody["input"] != nil {
		t.Fatalf("raw Responses input must not reach upstream: %#v", gotBody)
	}
}

// TestSubmitWithoutDecodeRequestFallsBackToBody 覆盖插件兼容回退：没有
// decodeRequest 的历史插件仍然把原始 body 字段交给 buildSubmitRequest。
func TestSubmitWithoutDecodeRequestFallsBackToBody(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	defer upstream.Close()

	source := `globalThis.__NEWTG_PLUGIN__ = {
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
  routes: [],
  protocols: {},
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel, prompt: ctx.prompt } }; },
  parseSubmitResponse: function () { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`
	compiled := compileResponsesTestPlugin(t, "video", source)
	if _, err := Submit(context.Background(), nil, SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ModelMapping: map[string]string{}},
		Model: "video-model", Body: map[string]any{"model": "video-model", "prompt": "keep-me"},
		BaseURL: upstream.URL, APIKey: "sk-test", AllowPrivate: true,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if gotBody["prompt"] != "keep-me" {
		t.Fatalf("fallback body.prompt = %#v, want keep-me", gotBody["prompt"])
	}
}
