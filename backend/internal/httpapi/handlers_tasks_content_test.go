package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/tasks"
)

// 本文件验收产物 content 能力 URL 端点（M6d T6b / relay-06 §10.5）：
// 签名校验、HEAD、未知产物、出站守卫与 access 脱敏后的错误路径。

const contentPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function (resp) { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "IN_PROGRESS" }; },
  listArtifacts: function (ctx) { return []; },
  buildContentRequest: function (ctx) {
    return { url: ctx.baseUrl + "/content/" + ctx.artifactKey, method: "GET", headers: { "Authorization": ctx.auth } };
  }
};`

const evilContentPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function (resp) { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "IN_PROGRESS" }; },
  listArtifacts: function (ctx) { return []; },
  buildContentRequest: function (ctx) {
    return { url: "https://evil.example.com/content", method: "GET" };
  }
};`

func newContentTestTask(t *testing.T, store *fakeTaskStore) *tasks.Task {
	t.Helper()
	task := &tasks.Task{
		PublicTaskID:   "task_" + strings.Repeat("a", 32),
		UpstreamTaskID: "up_1",
		Family:         "plugin",
		Action:         "generate",
		PluginKey:      "sample",
		FetchMode:      "poll",
		State:          tasks.StateSuccess,
		UserID:         42,
		ChannelID:      7,
		Quota:          100,
		Properties:     map[string]any{"OriginModelName": "sample-model"},
		PrivateData:    map[string]any{"request_id": "req_content"},
		Data: map[string]any{"artifacts": []any{
			map[string]any{"key": "v1", "type": "video", "mimeType": "video/mp4"},
		}},
	}
	if err := store.Insert(context.Background(), task); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return task
}

func contentTestDeps(t *testing.T, upstreamURL string, pluginSource string) (*TasksHandler, *fakeTaskStore, *tasks.Task) {
	t.Helper()
	store := &fakeTaskStore{}
	task := newContentTestTask(t, store)
	sealer := NewSecretSealer("task-handler-test-key")
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID:          7,
		BaseURL:     upstreamURL,
		Credentials: sealedCredential(t, sealer, "sk-upstream"),
	}}
	manager := newTaskManager(t, map[string]string{"sample": pluginSource})
	handler, _ := newTaskTestHandler(t, store, channels, manager, nil)
	return handler, store, task
}

func signedContentURL(t *testing.T, task *tasks.Task, artifactKey string) string {
	t.Helper()
	signer := tasks.NewArtifactSigner("task-handler-test-key", 0)
	return signer.ContentURL("", "sample", task, artifactKey, time.Now())
}

func TestArtifactsListIncludesSignedContentURL(t *testing.T) {
	handler, _, task := contentTestDeps(t, "https://up.example.com", contentPluginSource)
	mux := newTaskRelayMux(handler)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/tasks/sample/artifacts?task_id="+task.PublicTaskID, nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content_url":"http://example.com/v1/tasks/sample/artifacts/v1/content?`) {
		t.Fatalf("content_url missing or unsigned: %s", body)
	}
	if !strings.Contains(body, "access=v1.") {
		t.Fatalf("content_url missing access ticket: %s", body)
	}
}

func TestContentGETStreamsArtifact(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "artifact-bytes")
	}))
	defer upstream.Close()

	handler, _, task := contentTestDeps(t, upstream.URL, contentPluginSource)
	mux := newTaskRelayMux(handler)
	req := httptest.NewRequest(http.MethodGet, signedContentURL(t, task, "v1"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "artifact-bytes" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	if gotPath != "/content/v1" || gotAuth != "Bearer sk-upstream" || gotMethod != http.MethodGet {
		t.Fatalf("upstream call: path=%q auth=%q method=%q", gotPath, gotAuth, gotMethod)
	}
}

func TestContentHEADDoesNotWriteBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("upstream method = %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "14")
	}))
	defer upstream.Close()

	handler, _, task := contentTestDeps(t, upstream.URL, contentPluginSource)
	mux := newTaskRelayMux(handler)
	req := httptest.NewRequest(http.MethodHead, signedContentURL(t, task, "v1"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "14" {
		t.Fatalf("content-length = %q", rec.Header().Get("Content-Length"))
	}
}

func TestContentRejectsBadOrMissingTicket(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer upstream.Close()
	handler, _, task := contentTestDeps(t, upstream.URL, contentPluginSource)
	mux := newTaskRelayMux(handler)

	base := "/v1/tasks/sample/artifacts/v1/content?task_id=" + task.PublicTaskID
	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"missing access", base, http.StatusBadRequest},
		{"tampered access", base + "&access=v1.0.bogus", http.StatusForbidden},
		// 为 v1 签发的票据拿到 v2 路径：签名绑定 artifactKey，必须 403。
		{"wrong artifact", strings.Replace(signedContentURL(t, task, "v1"), "/v1/content", "/v2/content", 1), http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.target, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("invalid tickets must not reach upstream, got %d calls", calls)
	}
}

func TestContentUnknownArtifactIs404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unknown artifact must not hit upstream")
	}))
	defer upstream.Close()
	handler, _, task := contentTestDeps(t, upstream.URL, contentPluginSource)
	mux := newTaskRelayMux(handler)

	req := httptest.NewRequest(http.MethodGet, signedContentURL(t, task, "nope"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestContentOutboundGuardReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	handler, _, task := contentTestDeps(t, upstream.URL, evilContentPluginSource)
	mux := newTaskRelayMux(handler)

	req := httptest.NewRequest(http.MethodGet, signedContentURL(t, task, "v1"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ev"+"il.example.com") {
		t.Fatalf("error body leaked upstream URL: %s", rec.Body.String())
	}
}
