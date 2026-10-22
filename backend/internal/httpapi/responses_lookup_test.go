package httpapi

// 本文件是 T3 + W18-C 的 HTTP 面验收：
//   - P-02：`GET /v1/responses/{id}` 只服务插件任务产物（`resp_<x>` → `task_<x>`）；
//     非插件来源 / 归属不符 / 产物缺失一律 404（归属不符**不是** 403，
//     避免把 resp_ 空间变成存在性 oracle）；
//   - R-W18-07：检索面要求插件声明 openai_responses 且 supports 含 retrieve；
//     只声明 create 的插件不接管检索；
//   - R-W18-12：`POST /v1/responses/compact` 与 `GET /v1/responses`（WS）本波
//     不提供：不注册路由 → 404（501 半死桩已删除），不得被插件抢占；
//   - R-10：`GET /v1/responses/{}` 需进 `pluginDispatchedBuiltinShapes`
//     （插件路由可达），本波不提供的形状进 unprovidedRouteShapes。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/tasks"
)

// responsesRetrievePluginSource 声明 `openai_responses` 协议但**不**注册任何
// 原生路由：GET /v1/responses/{id} 必须落到内建的任务产物回放。
// 声明该协议时加载期要求 decodeRequest + renderEvents/renderFinal（relay-06 §3.1）。
const responsesRetrievePluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function () { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; },
  decodeRequest: function () { return {}; },
  renderFinal: function () { return { object: "response" }; }
};`

// responsesPlainPluginSource 是不声明任何宿主协议的同一插件：它的任务
// **不得**由 Responses 检索面回放（P-02 的「要求插件声明 openai_responses」）。
const responsesPlainPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function () { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`

// responsesNativeGetPluginSource 声明 `GET /v1/responses/:response_id` 为原生
// query 路由：R-10 允许它注册，且请求必须由插件接管（内建回放不得抢答）。
const responsesNativeGetPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
    { method: "GET", path: "/v1/responses/:response_id", kind: "query", taskIdParam: "response_id", render: "parseTaskResult" }
  ],
  protocols: {},
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function () { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`

// responsesLookupTaskID 是一个合法的 task_ id（32 位 hex，与 NewPublicTaskID 同形）。
const responsesLookupTaskID = "task_0123456789abcdef0123456789abcdef"

// responsesLookupResponseID 是上面任务对应的 resp_ id。
const responsesLookupResponseID = "resp_0123456789abcdef0123456789abcdef"

func newResponsesLookupManager(t *testing.T, source string) *plugin.Manager {
	t.Helper()
	return newTaskManager(t, map[string]string{"video": source})
}

// newResponsesLookupMux 用指定的令牌 user 装配任务数据面（含 GET responses 回放）。
// userID 与任务行的 UserID 不同即模拟「他人任务」。
func newResponsesLookupMux(t *testing.T, handler *TasksHandler, tokenUserID int64) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	tokens := &fakeTaskTokens{meta: &auth.TokenMeta{
		ID: 1, UserID: tokenUserID, Status: "enabled", Group: "default", UnlimitedQuota: true,
	}}
	handler.RegisterRelay(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	return mux
}

func newResponsesLookupHandler(t *testing.T, store TaskRepository, manager *plugin.Manager) *TasksHandler {
	t.Helper()
	return NewTasksHandler(TasksHandlerConfig{
		Store:                store,
		Channels:             &fakeTaskChannels{},
		Plugins:              manager,
		Sealer:               NewSecretSealer("task-handler-test-key"),
		Tokens:               &fakeTaskTokens{meta: &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}},
		ArtifactSigner:       tasks.NewArtifactSigner("task-handler-test-key", 0),
		AllowPrivateUpstream: true,
	})
}

// insertResponsesLookupTask 写入一条可回放的任务（用户 42、video 插件、一个产物）。
func insertResponsesLookupTask(t *testing.T, store *fakeTaskStore, mutate func(*tasks.Task)) *tasks.Task {
	t.Helper()
	submitted := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	task := &tasks.Task{
		PublicTaskID:   responsesLookupTaskID,
		UpstreamTaskID: "up_1",
		Family:         "plugin",
		Action:         "generate",
		PluginKey:      "video",
		FetchMode:      "poll",
		State:          tasks.StateSuccess,
		UserID:         42,
		ChannelID:      7,
		Quota:          100,
		SubmitTime:     &submitted,
		Properties:     map[string]any{"OriginModelName": "video-model"},
		PrivateData:    map[string]any{"request_id": "req_responses"},
		Data: map[string]any{"artifacts": []any{
			map[string]any{"key": "v1", "type": "video", "mimeType": "video/mp4"},
		}},
	}
	if mutate != nil {
		mutate(task)
	}
	if err := store.Insert(context.Background(), task); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return task
}

func getResponseObject(t *testing.T, mux http.Handler, responseID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/"+responseID, nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// requireRelayErrorCode 断言响应是 /v1 的 OpenAI 错误信封且 code 匹配。
func requireRelayErrorCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (/v1 面)", ct)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q; body=%s", envelope.Error.Code, code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID must be set")
	}
}

// TestResolveTaskFromResponseID 冻结「只做前缀替换」的映射语义。
func TestResolveTaskFromResponseID(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"mapped", "resp_0123456789abcdef", "task_0123456789abcdef", true},
		{"trimmed", " resp_abc ", "task_abc", true},
		{"task_prefix", "task_abc", "", false},
		{"empty", "", "", false},
		{"bare_prefix", "resp_", "", false},
		{"case_sensitive", "RESP_abc", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolvetaskFromResponseID(tc.raw)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("resolvetaskFromResponseID(%q) = (%q, %v), want (%q, %v)",
					tc.raw, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestResponsesGetRejectsForeignTask 是 brief Step 1：A 用户的任务由 B 令牌
// 请求必须 404（不是 403）——403 会告诉调用方「这个 id 存在」。
func TestResponsesGetRejectsForeignTask(t *testing.T) {
	store := &fakeTaskStore{}
	insertResponsesLookupTask(t, store, nil)
	handler := newResponsesLookupHandler(t, store, newResponsesLookupManager(t, responsesRetrievePluginSource))

	owner := newResponsesLookupMux(t, handler, 42)
	foreign := newResponsesLookupMux(t, handler, 43)

	// 本人可取（证明任务、插件、产物三项前置都成立，404 只由归属引起）。
	if rec := getResponseObject(t, owner, responsesLookupResponseID); rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec := getResponseObject(t, foreign, responsesLookupResponseID)
	requireRelayErrorCode(t, rec, http.StatusNotFound, "not_found")

	// 归属不符与「根本不存在」的响应必须无法区分（同状态码、同 code、同文案）。
	missing := getResponseObject(t, foreign, "resp_ffffffffffffffffffffffffffffffff")
	if missing.Code != rec.Code {
		t.Fatalf("missing status = %d, foreign status = %d: must be indistinguishable",
			missing.Code, rec.Code)
	}
	if stripRequestID(missing.Body.String()) != stripRequestID(rec.Body.String()) {
		t.Fatalf("foreign/missing bodies differ:\n%s\n%s", rec.Body.String(), missing.Body.String())
	}
}

// stripRequestID 去掉错误信封里每次请求都不同的 requestId，便于逐字比较其余字段。
func stripRequestID(body string) string {
	start := strings.Index(body, " (requestId: ")
	if start < 0 {
		return body
	}
	end := strings.Index(body[start:], ")")
	if end < 0 {
		return body
	}
	return body[:start] + body[start+end+1:]
}

// TestResponsesGetRejectsNonPluginSource 是 brief Step 2：非插件来源、插件未
// 声明协议、插件已卸载、产物缺失都必须 404（同一文案，不区分原因）。
func TestResponsesGetRejectsNonPluginSource(t *testing.T) {
	claimsProtocol := newResponsesLookupManager(t, responsesRetrievePluginSource)
	plain := newResponsesLookupManager(t, responsesPlainPluginSource)

	cases := []struct {
		name       string
		manager    *plugin.Manager
		mutate     func(*tasks.Task)
		responseID string
	}{
		{
			// 没有 resp_ 前缀：即便去掉前缀后恰好指向一个本用户的任务也不得回放。
			name: "bad_prefix", manager: claimsProtocol,
			responseID: "task_0123456789abcdef0123456789abcdef",
		},
		{
			name: "unknown_task", manager: claimsProtocol,
			responseID: "resp_ffffffffffffffffffffffffffffffff",
		},
		{
			// 插件不声明 openai_responses：该任务没有 Responses 语义可回放。
			name: "plugin_without_claim", manager: plain,
			responseID: responsesLookupResponseID,
		},
		{
			// 插件已卸载：无法确认协议声明，按不存在处理（而不是 503）。
			name: "plugin_uninstalled", manager: claimsProtocol,
			mutate:     func(task *tasks.Task) { task.PluginKey = "ghost" },
			responseID: responsesLookupResponseID,
		},
		{
			// 任务还在跑/失败/结果已丢弃：没有可检索产物 → 404。
			name: "no_artifacts", manager: claimsProtocol,
			mutate:     func(task *tasks.Task) { task.Data = nil },
			responseID: responsesLookupResponseID,
		},
		{
			// 只有非法 artifact key（不签发能力 URL）→ 同样视为不可检索。
			name: "unretrievable_artifact", manager: claimsProtocol,
			mutate: func(task *tasks.Task) {
				task.Data = map[string]any{"artifacts": []any{
					map[string]any{"key": "bad/key", "type": "video"},
				}}
			},
			responseID: responsesLookupResponseID,
		},
		{
			// 保留期已过（retention_deadline）→ 产物不可检索。
			name: "retention_expired", manager: claimsProtocol,
			mutate: func(task *tasks.Task) {
				past := time.Now().Add(-time.Hour)
				task.RetentionDeadline = &past
			},
			responseID: responsesLookupResponseID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeTaskStore{}
			insertResponsesLookupTask(t, store, tc.mutate)
			handler := newResponsesLookupHandler(t, store, tc.manager)
			rec := getResponseObject(t, newResponsesLookupMux(t, handler, 42), tc.responseID)
			requireRelayErrorCode(t, rec, http.StatusNotFound, "not_found")
		})
	}
}

// TestResponsesGetRequiresRetrieveSupport 是 R-W18-07 的能力收窄：插件只声明
// create 时没有接管检索面，即便任务有产物也必须 404（不得由内建回放代管）。
func TestResponsesGetRequiresRetrieveSupport(t *testing.T) {
	createOnly := strings.Replace(responsesRetrievePluginSource,
		`supports: ["create", "retrieve"]`, `supports: ["create"]`, 1)
	store := &fakeTaskStore{}
	insertResponsesLookupTask(t, store, nil)
	handler := newResponsesLookupHandler(t, store, newResponsesLookupManager(t, createOnly))

	rec := getResponseObject(t, newResponsesLookupMux(t, handler, 42), responsesLookupResponseID)
	requireRelayErrorCode(t, rec, http.StatusNotFound, "not_found")
}

// TestResponsesGetReturnsPluginArtifacts 是 brief Step 3：可检索产物按 Responses
// 形态回放（resp_ id 原样、状态、模型、产物的签名能力 URL）。
func TestResponsesGetReturnsPluginArtifacts(t *testing.T) {
	store := &fakeTaskStore{}
	task := insertResponsesLookupTask(t, store, nil)
	handler := newResponsesLookupHandler(t, store, newResponsesLookupManager(t, responsesRetrievePluginSource))

	rec := getResponseObject(t, newResponsesLookupMux(t, handler, 42), responsesLookupResponseID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var object struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		CreatedAt int64  `json:"created_at"`
		Status    string `json:"status"`
		Model     string `json:"model"`
		Output    []struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			Status   string `json:"status"`
			MIMEType string `json:"mime_type"`
			URL      string `json:"url"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode response object: %v; body=%s", err, rec.Body.String())
	}
	if object.ID != responsesLookupResponseID {
		t.Fatalf("id = %q, want %q (resp_ id 必须原样回显)", object.ID, responsesLookupResponseID)
	}
	if object.Object != "response" {
		t.Fatalf("object = %q, want response", object.Object)
	}
	if object.Status != "completed" {
		t.Fatalf("status = %q, want completed", object.Status)
	}
	if object.Model != "video-model" {
		t.Fatalf("model = %q, want video-model", object.Model)
	}
	if want := task.SubmitTime.Unix(); object.CreatedAt != want {
		t.Fatalf("created_at = %d, want %d (submit_time)", object.CreatedAt, want)
	}
	if len(object.Output) != 1 {
		t.Fatalf("output len = %d, want 1; body=%s", len(object.Output), rec.Body.String())
	}
	item := object.Output[0]
	if item.Type != "video" || item.ID != "v1" || item.Status != "completed" || item.MIMEType != "video/mp4" {
		t.Fatalf("output item = %+v, want {video v1 completed video/mp4}", item)
	}
	// URL 必须是宿主签发的 HMAC 能力 URL，且真的能通过验签（否则回放出的链接不可用）。
	if !strings.Contains(item.URL, "/v1/tasks/video/artifacts/v1/content") ||
		!strings.Contains(item.URL, "task_id="+responsesLookupTaskID) ||
		!strings.Contains(item.URL, "access=") {
		t.Fatalf("content url = %q, want a signed artifact content url", item.URL)
	}
	signer := tasks.NewArtifactSigner("task-handler-test-key", 0)
	parsed, err := url.Parse(item.URL)
	if err != nil {
		t.Fatalf("content url is not parseable: %v", err)
	}
	if !signer.Verify("video", task.PublicTaskID, "v1", parsed.Query().Get("access"), time.Now()) {
		t.Fatalf("content url signature does not verify: %q", item.URL)
	}
}

// TestResponsesGetDispatchesPluginRouteFirst 覆盖 R-10 的 pluginDispatched 例外：
// 插件声明 GET /v1/responses/{id} 时请求必须由插件接管（这里以 handleGet 的
// task_not_found 作为「已进入原生 query 分发」的证据），内建回放不得抢答。
func TestResponsesGetDispatchesPluginRouteFirst(t *testing.T) {
	store := &fakeTaskStore{}
	insertResponsesLookupTask(t, store, nil)
	handler := newResponsesLookupHandler(t, store, newResponsesLookupManager(t, responsesNativeGetPluginSource))

	rec := getResponseObject(t, newResponsesLookupMux(t, handler, 42), responsesLookupResponseID)
	// 插件路由把 response_id 当作 task_id 交给 handleGet；库里没有这个 id
	// （行 key 是 task_...），因此是 task_not_found，而不是内建回放的 not_found。
	requireRelayErrorCode(t, rec, http.StatusNotFound, "task_not_found")
}

// TestResponsesUnprovidedEndpointsReturn404 是 R-W18-12 的验收：compact 与
// Responses WebSocket 本波不提供，**不注册路由** → 404，不得返回 501 半死桩，
// 也不得产生任何计费或上游调用。
func TestResponsesUnprovidedEndpointsReturn404(t *testing.T) {
	transport := &countingTransport{base: http.DefaultTransport}
	relayBilling := newRecordingBilling()
	handler := NewRelayHandler(RelayHandlerOptions{
		Channels: &fakeRelayChannelStore{models: []string{"gpt-test"}},
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID: relayTestTokenID, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
		}),
		Access:  &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:  NewSecretSealer(relaySealerMaster),
		Client:  &http.Client{Transport: transport},
		Billing: func(*contract.RelayContext) contract.BillingSession { return relayBilling },
	})
	mux := handler.Handler()

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/responses/compact", `{"model":"gpt-test","input":[{"type":"message","role":"user","content":"hi"}]}`},
		{http.MethodPost, "/v1/responses/compact", `not-json`},
		{http.MethodGet, "/v1/responses", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+relayTestToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s %s Content-Type = %q, want application/json", tc.method, tc.path, ct)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
		}
		if envelope.Error.Code != "invalid_request_error" {
			t.Fatalf("%s %s code = %q, want invalid_request_error; body=%s",
				tc.method, tc.path, envelope.Error.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Fatalf("%s %s must set X-Request-ID", tc.method, tc.path)
		}
	}

	// 零副作用：不读请求体（`not-json` 也是 404）、不建会话、不调上游。
	if n := relayBilling.preConsumeCount(); n != 0 {
		t.Fatalf("pre-consume = %d, want 0", n)
	}
	if n := transport.count(); n != 0 {
		t.Fatalf("upstream requests = %d, want 0; calls=%v", n, transport.paths())
	}
}

// TestResponsesRouteShapesFollowR10 维护 R-10 契约（形状表本身在 plugin 包，
// 这里断言 HTTP 面可见的行为）：内建处理器先查 RouteIndex 的
// `GET /v1/responses/{id}` 对插件开放；本波不提供的 `GET /v1/responses`
// 与 `POST /v1/responses/compact`（R-W18-12）不得被插件抢占。
func TestResponsesRouteShapesFollowR10(t *testing.T) {
	// 插件声明 GET /v1/responses/{id}：注册成功且可匹配。
	queryRoute := `[ { method: "GET", path: "/v1/responses/:response_id", kind: "query", render: "parseTaskResult" } ]`
	m := newTaskManager(t, map[string]string{"video": responsesQueryRoutePluginSource(queryRoute)})
	if entry := m.Snapshot().Lookup("video"); entry.Status != plugin.StatusRegistered {
		t.Fatalf("GET /v1/responses/{id} 插件路由必须注册成功: %+v", entry)
	}
	if match, ok := m.Snapshot().Routes().Match("GET", "/v1/responses/resp_x"); !ok || match.PluginKey != "video" {
		t.Fatalf("plugin GET responses route must be matchable: %+v ok=%v", match, ok)
	}

	// R-W18-12：本波不提供的形状必须整插件失败（没有 handler，插件也抢不到）。
	for _, reserved := range []string{
		`[ { method: "POST", path: "/v1/responses/compact", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" } ]`,
		`[ { method: "GET", path: "/v1/responses", kind: "query", render: "parseTaskResult" } ]`,
	} {
		m2 := newTaskManager(t, map[string]string{"video": responsesQueryRoutePluginSource(reserved)})
		if entry := m2.Snapshot().Lookup("video"); entry.Status != plugin.StatusCompileFailed {
			t.Fatalf("本波不提供的 Responses 形状必须让插件 compile_failed: %+v", entry)
		}
		if _, ok := m2.Snapshot().Routes().Match("POST", "/v1/responses/compact"); ok {
			t.Fatal("rejected compact route must not enter the route index")
		}
	}
}

// responsesQueryRoutePluginSource 生成一个 routes[] 可定制的插件源（用于 R-10 断言）。
func responsesQueryRoutePluginSource(routes string) string {
	return `globalThis.__NEWTG_PLUGIN__ = {
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
  routes: ` + routes + `,
  protocols: {},
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function () { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; }
};`
}
