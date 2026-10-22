package tasks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// hostAPIPluginSource 生成一个只关心宿主 API 契约的合规插件。
func hostAPIPluginSource(capabilities, responseTypes, hooks string) string {
	if capabilities == "" {
		capabilities = "[]"
	}
	responseTypesLine := ""
	if responseTypes != "" {
		responseTypesLine = "  submitResponseTypes: " + responseTypes + ",\n"
	}
	return `globalThis.__NEWTG_PLUGIN__ = {
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
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" }
    }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: ` + capabilities + `,
` + responseTypesLine + hooks + `
};`
}

func submitTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) (*httptest.Server, *[]byte) {
	t.Helper()
	var (
		mu       sync.Mutex
		received []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append([]byte(nil), body...)
		mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &received
}

func TestSubmitInlinesFilePlaceholdersAtAnyDepth(t *testing.T) {
	source := hostAPIPluginSource("", "", `
  buildSubmitRequest: function (ctx) {
    return {
      url: ctx.baseUrl + "/tasks",
      method: "POST",
      body: {
        image: { __fileRef: "request_file:image", encoding: "base64" },
        nested: [{ deep: { __fileRef: "request_file:image#1", encoding: "dataUrl" } }],
        typed: { __fileRef: "request_file:image", encoding: "dataUrl", mimeType: "image/png" },
        filesSeenByPlugin: ctx.files
      }
    };
  },
  parseSubmitResponse: function (resp) { return { taskId: "t1", state: "SUBMITTED" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	srv, received := submitTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"t1","state":"SUBMITTED"}`))
	})

	first := []byte("first-bytes")
	second := []byte("second-bytes")
	_, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin:       compiled,
		Channel:      ChannelTarget{ID: 1, BaseURL: srv.URL},
		Model:        "sample-model",
		Body:         map[string]any{},
		BaseURL:      srv.URL,
		APIKey:       "secret-key",
		AllowPrivate: true,
		Files: []InboundFile{
			{Ref: FileReference("image", 0), Field: "image", Filename: "a.png", MIMEType: "image/png", Size: int64(len(first)), Data: first},
			{Ref: FileReference("image", 1), Field: "image", Filename: "b.png", MIMEType: "image/jpeg", Size: int64(len(second)), Data: second},
		},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(*received, &body); err != nil {
		t.Fatalf("decode request body %q: %v", string(*received), err)
	}
	image := body["image"].(string)
	if image != base64.StdEncoding.EncodeToString(first) {
		t.Fatalf("base64 placeholder mismatch: %q", image)
	}
	deep := body["nested"].([]any)[0].(map[string]any)["deep"].(string)
	if deep != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(second) {
		t.Fatalf("dataUrl (part mime) mismatch: %q", deep)
	}
	typed := body["typed"].(string)
	if typed != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(first) {
		t.Fatalf("dataUrl (override mime) mismatch: %q", typed)
	}
	// 文件字节/路径绝不出现在 JS 侧引用里。
	files := body["filesSeenByPlugin"].([]any)
	if len(files) != 2 {
		t.Fatalf("ctx.files must contain 2 references, got %#v", files)
	}
	ref := files[0].(map[string]any)
	for _, forbidden := range []string{"data", "Data", "path", "reader", "bytes"} {
		if _, exists := ref[forbidden]; exists {
			t.Fatalf("file reference leaked %q: %#v", forbidden, ref)
		}
	}
	if ref["ref"] != "request_file:image" || ref["field"] != "image" || ref["size"] != float64(len(first)) {
		t.Fatalf("unexpected opaque reference: %#v", ref)
	}
	if strings.Contains(string(*received), string(first)) {
		t.Fatalf("raw file bytes leaked into request body")
	}
}

func TestSubmitRejectsInvalidFilePlaceholders(t *testing.T) {
	cases := []struct {
		name    string
		bodyJS  string
		files   []InboundFile
		wantErr string
	}{
		{
			name:   "unknown ref",
			bodyJS: `{ x: { __fileRef: "request_file:missing", encoding: "base64" } }`,
		},
		{
			name:   "extra key",
			bodyJS: `{ x: { __fileRef: "request_file:image", encoding: "base64", extra: true } }`,
		},
		{
			name:   "bad encoding",
			bodyJS: `{ x: { __fileRef: "request_file:image", encoding: "hex" } }`,
		},
		{
			name:   "maxBytes exceeded",
			bodyJS: `{ x: { __fileRef: "request_file:image", encoding: "base64", maxBytes: 3 } }`,
			files:  []InboundFile{{Ref: FileReference("image", 0), Field: "image", Size: 4, Data: []byte("data")}},
		},
		{
			name:   "maxBytes not positive",
			bodyJS: `{ x: { __fileRef: "request_file:image", encoding: "base64", maxBytes: 0 } }`,
			files:  []InboundFile{{Ref: FileReference("image", 0), Field: "image", Data: []byte("data")}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := hostAPIPluginSource("", "", `
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: `+tc.bodyJS+` };
  },
  parseSubmitResponse: function () { return { taskId: "t1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
			compiled := compileTestPlugin(t, source)
			files := tc.files
			if files == nil {
				files = []InboundFile{{Ref: FileReference("image", 0), Field: "image", Data: []byte("data")}}
			}
			_, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
				Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: "http://127.0.0.1:1"},
				Model: "sample-model", Body: map[string]any{}, BaseURL: "http://127.0.0.1:1",
				AllowPrivate: true, Files: files,
			})
			if !errors.Is(err, ErrFilePlaceholder) {
				t.Fatalf("want ErrFilePlaceholder, got %v", err)
			}
			if !errors.Is(err, ErrDecodeRequest) {
				t.Fatalf("placeholder errors must map to ErrDecodeRequest for 4xx, got %v", err)
			}
		})
	}
}

func TestSubmitSnapshotSSE(t *testing.T) {
	source := hostAPIPluginSource("", `["json", "sse"]`, `
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.model }, responseType: "sse" };
  },
  parseSubmitEvent: function (ctx, event, previousState) {
    var parsed = JSON.parse(event.data);
    return { state: { last: parsed.text, count: (previousState ? previousState.count : 0) + 1 }, done: parsed.done === true };
  },
  parseSubmitResponse: function (resp) {
    var state = JSON.parse(resp.body);
    return { taskId: "sse-1", taskData: state, state: "SUBMITTED" };
  },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": heartbeat\n\n")
		_, _ = io.WriteString(w, "event: message\ndata: {\"text\":\"a\",\"done\":false}\n\n")
		_, _ = io.WriteString(w, "id: 2\r\ndata: {\"text\":\"b\",\"done\":true}\r\n\r\n")
	}))
	defer srv.Close()

	result, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: srv.URL},
		Model: "sample-model", Body: map[string]any{}, BaseURL: srv.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("submit sse: %v", err)
	}
	if result.UpstreamTaskID != "sse-1" {
		t.Fatalf("unexpected task id: %#v", result)
	}
	if result.Data["last"] != "b" || fmt.Sprint(result.Data["count"]) != "2" {
		t.Fatalf("snapshot state not accumulated: %#v", result.Data)
	}
}

func TestSubmitDeltaSSE(t *testing.T) {
	source := hostAPIPluginSource(`["submit-sse-delta@1"]`, `["json", "sse"]`, `
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.model }, responseType: "sse" };
  },
  parseSubmitEventDelta: function (ctx, event, previousState) {
    var parsed = JSON.parse(event.data);
    if (parsed.first) {
      return {
        changes: [
          { op: "set", path: [], value: { text: "", images: [] } },
          { op: "appendText", path: ["text"], value: "hello" }
        ],
        state: { n: 1 }, done: false
      };
    }
    return {
      changes: [
        { op: "appendText", path: ["text"], value: " world" },
        { op: "append", path: ["images"], value: { url: "https://cdn.example/i.png" } }
      ],
      state: { n: 2 }, done: true
    };
  },
  parseSubmitResponse: function (resp) {
    var body = JSON.parse(resp.body);
    return { taskId: "delta-1", taskData: body, state: "SUBMITTED" };
  },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"first\":true}\n\n")
		_, _ = io.WriteString(w, "data: {\"first\":false}\n\n")
	}))
	defer srv.Close()

	result, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: srv.URL},
		Model: "sample-model", Body: map[string]any{}, BaseURL: srv.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("submit delta sse: %v", err)
	}
	if result.UpstreamTaskID != "delta-1" {
		t.Fatalf("unexpected task id: %#v", result)
	}
	if result.Data["text"] != "hello world" {
		t.Fatalf("appendText not applied: %#v", result.Data)
	}
	images, ok := result.Data["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("append not applied: %#v", result.Data)
	}
	if images[0].(map[string]any)["url"] != "https://cdn.example/i.png" {
		t.Fatalf("unexpected appended image: %#v", images[0])
	}
}

func TestSubmitDeltaRejectsInvalidChange(t *testing.T) {
	source := hostAPIPluginSource(`["submit-sse-delta@1"]`, `["json", "sse"]`, `
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: {}, responseType: "sse" };
  },
  parseSubmitEventDelta: function () {
    return { changes: [{ op: "append", path: ["missing"], value: 1 }], state: null, done: true };
  },
  parseSubmitResponse: function () { return { taskId: "never" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()

	if _, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: srv.URL},
		Model: "sample-model", Body: map[string]any{}, BaseURL: srv.URL, AllowPrivate: true,
	}); !errors.Is(err, ErrPluginHook) {
		t.Fatalf("invalid delta change must fail submission, got %v", err)
	}
}

func TestSubmitRejectsUndeclaredSSEResponseType(t *testing.T) {
	source := hostAPIPluginSource("", "", `
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: {}, responseType: "sse" };
  },
  parseSubmitResponse: function () { return { taskId: "never" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	_, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: "http://127.0.0.1:1"},
		Model: "sample-model", Body: map[string]any{}, BaseURL: "http://127.0.0.1:1", AllowPrivate: true,
	})
	if !errors.Is(err, ErrUnsupportedResponseType) {
		t.Fatalf("undeclared sse must be rejected, got %v", err)
	}
}

func TestSubmitSSEEOFWithoutDoneFails(t *testing.T) {
	source := hostAPIPluginSource("", `["json", "sse"]`, `
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: {}, responseType: "sse" };
  },
  parseSubmitEvent: function () { return { state: { n: 1 }, done: false }; },
  parseSubmitResponse: function () { return { taskId: "never" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()
	if _, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: srv.URL},
		Model: "sample-model", Body: map[string]any{}, BaseURL: srv.URL, AllowPrivate: true,
	}); err == nil {
		t.Fatal("EOF without done must fail submission")
	}
}

func TestSubmitContextInjectionIsCompleteAndSandboxed(t *testing.T) {
	source := hostAPIPluginSource("", "", `
  buildSubmitRequest: function (ctx) {
    return {
      url: ctx.baseUrl + "/tasks",
      method: "POST",
      body: {
        model: ctx.model,
        upstreamModel: ctx.upstreamModel,
        baseUrlSeen: typeof ctx.baseUrl === "string" && ctx.baseUrl.length > 0,
        apiKeySeen: ctx.apiKey === "channel-key",
        authHeader: ctx.authHeader,
        upstreamKind: ctx.upstream.kind,
        action: ctx.action,
        originTaskId: ctx.originTasks[0].taskId,
        originData: ctx.originTasks[0].data,
        fileFields: Object.keys(ctx.files[0]).sort(),
        fileRef: ctx.files[0].ref,
        hasFetch: typeof globalThis["fetch"],
        hasRequire: typeof globalThis["require"],
        hasProcess: typeof globalThis["process"],
        hasUtilsCapability: utils.hasCapability("json-clone@1")
      }
    };
  },
  parseSubmitResponse: function () { return { taskId: "ctx-1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q", method: "GET" }; },
  parseTaskResult: function () { return { state: "UNKNOWN" }; }`)
	compiled := compileTestPlugin(t, source)
	srv, received := submitTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_, _ = w.Write([]byte(`{"id":"ctx-1"}`))
	})

	_, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled, Channel: ChannelTarget{ID: 1, BaseURL: srv.URL, Type: "60"},
		Model: "sample-model", Body: map[string]any{"prompt": "hi"},
		BaseURL: srv.URL, APIKey: "channel-key", AllowPrivate: true,
		Action:         "text_to_video",
		UpstreamKind:   "",
		RequestHeaders: map[string]string{"X-Test": "1"},
		OriginTasks: []OriginTask{{
			TaskID: "task_original", UpstreamTaskID: "up_1", Action: "remix",
			Status: "SUCCESS", Data: map[string]any{"seconds": float64(5)},
		}},
		Files: []InboundFile{{
			Ref: FileReference("image", 0), Field: "image", Filename: "a.png",
			MIMEType: "image/png", Size: 4, Data: []byte("data"),
		}},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(*received, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["model"] != "sample-model" || body["upstreamModel"] != "sample-model" {
		t.Fatalf("model identities wrong: %#v", body)
	}
	if body["baseUrlSeen"] != true {
		// 插件用固定字面量比较，只为覆盖写入；实际 baseUrl 由 srv.URL 提供。
		t.Fatalf("baseUrl not injected: %#v", body)
	}
	if body["apiKeySeen"] != true {
		t.Fatalf("apiKey not injected: %#v", body)
	}
	if body["authHeader"] != "Bearer channel-key" {
		t.Fatalf("authHeader wrong: %#v", body["authHeader"])
	}
	if body["upstreamKind"] != "new_api" {
		t.Fatalf("type 60 channel must map to new_api: %#v", body["upstreamKind"])
	}
	if body["action"] != "text_to_video" || body["originTaskId"] != "task_original" {
		t.Fatalf("action/originTasks missing: %#v", body)
	}
	originData := body["originData"].(map[string]any)
	if originData["seconds"] != float64(5) {
		t.Fatalf("origin task data missing: %#v", originData)
	}
	fields := body["fileFields"].([]any)
	wantFields := []string{"field", "filename", "mimeType", "ref", "size"}
	if len(fields) != len(wantFields) {
		t.Fatalf("file view must expose exactly opaque fields, got %#v", fields)
	}
	for i, want := range wantFields {
		if fields[i] != want {
			t.Fatalf("file view fields = %#v, want %v", fields, wantFields)
		}
	}
	if body["hasFetch"] != "undefined" || body["hasRequire"] != "undefined" || body["hasProcess"] != "undefined" {
		t.Fatalf("sandbox globals leaked: %#v", body)
	}
	if body["hasUtilsCapability"] != true {
		t.Fatalf("utils.hasCapability(json-clone@1) = %#v", body["hasUtilsCapability"])
	}
}
