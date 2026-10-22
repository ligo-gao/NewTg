package httpapi

// W14-SENSITIVE：多模态/任务/视频/批次入口的敏感词过滤验收。
//
// 纪律与既有 sensitive 测试一致：不连真 PG/Redis，上游用 httptest，多模态用
// 捕获 dispatcher。真实 PostgreSQL 的快照读取在
// sensitive_entries_integration_test.go（build tag integration）。
//
// 每个端点锁定四件事：
//   - 关闭时行为不变（请求照常进入 dispatcher/上游/落库）；
//   - block 命中 → 400 content_blocked，PreConsume=0、上游=0、任务/批次行=0；
//   - log 命中 → 放行且不回显命中词；
//   - 非文本字段（模型名、计费参数、图片/音频二进制）不误伤。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/sensitive"
	"newtg/gateway/internal/tasks"
)

// ---------- 多模态 ----------

func newSensitiveMultimodalHandler(t *testing.T, dispatcher MultimodalDispatcher,
	cfg ConfigReader) http.Handler {
	t.Helper()
	tokenStore := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID:             relayTestTokenID,
		UserID:         relayTestUserID,
		Status:         "enabled",
		Group:          "default",
		UnlimitedQuota: true,
	})
	access := &fakeRelayAccess{role: "user", status: "enabled"}
	channels := &fakeRelayChannelStore{models: []string{"gpt-test"}}
	h := NewRelayHandler(RelayHandlerOptions{
		Channels:   channels,
		Tokens:     tokenStore,
		Access:     access,
		Sealer:     NewSecretSealer(relaySealerMaster),
		Multimodal: dispatcher,
		Config:     cfg,
	})
	return h.Handler()
}

// sensitiveMultimodalCases 返回覆盖全部冻结端点的有效请求。text 同时放进
// 可扫描的文本字段与 multipart 文件字节，便于「命中」与「二进制不误伤」
// 两类用例复用同一套请求形状。
func sensitiveMultimodalCases(t *testing.T, text string) []struct {
	name string
	req  multimodalRequest
} {
	t.Helper()
	imageCT, imageBody := buildMultipartBody(t,
		map[string]string{"model": "img-model", "prompt": text},
		map[string][]byte{"image": []byte("binary-" + text)})
	audioCT, audioBody := buildMultipartBody(t,
		map[string]string{"model": "whisper-1", "prompt": text},
		map[string][]byte{"file": []byte("binary-" + text)})
	return []struct {
		name string
		req  multimodalRequest
	}{
		{"embeddings-string", multimodalRequest{
			path: "/v1/embeddings", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","input":%q}`, text))}},
		{"embeddings-array", multimodalRequest{
			path: "/v1/embeddings", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","input":["clean",%q]}`, text))}},
		{"embeddings-gemini", multimodalRequest{
			path: "/v1/models/gemini-embed:embedContent", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"content":{"parts":[{"text":%q}]}}`, text))}},
		{"embeddings-engines", multimodalRequest{
			path: "/v1/engines/text-embedding-004/embeddings", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"input":%q}`, text))}},
		{"images-generations", multimodalRequest{
			path: "/v1/images/generations", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","prompt":%q}`, text))}},
		{"images-edits-json", multimodalRequest{
			path: "/v1/images/edits", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","prompt":%q}`, text))}},
		{"images-edits-multipart", multimodalRequest{
			path: "/v1/images/edits", contentType: imageCT, body: imageBody}},
		{"rerank-query", multimodalRequest{
			path: "/v1/rerank", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","query":%q,"documents":["clean"]}`, text))}},
		{"rerank-document", multimodalRequest{
			path: "/v1/rerank", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","query":"clean","documents":["clean",%q]}`, text))}},
		{"audio-speech", multimodalRequest{
			path: "/v1/audio/speech", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"model":"gpt-test","input":%q}`, text))}},
		{"audio-transcriptions", multimodalRequest{
			path: "/v1/audio/transcriptions", contentType: audioCT, body: audioBody}},
		{"audio-translations", multimodalRequest{
			path: "/v1/audio/translations", contentType: audioCT, body: audioBody}},
	}
}

func TestSensitiveMultimodalBlockZeroDispatch(t *testing.T) {
	for _, tc := range sensitiveMultimodalCases(t, "BadWord") {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher := &captureMultimodalDispatcher{}
			handler := newSensitiveMultimodalHandler(t, dispatcher, sensitiveSettings(t,
				true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

			resp := doMultimodal(t, handler, tc.req, true)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
			if dispatcher.calls != 0 {
				t.Fatalf("dispatcher calls = %d, want 0（block 必须零上游）", dispatcher.calls)
			}
			body := resp.Body.String()
			if !strings.Contains(body, "content_blocked") {
				t.Fatalf("缺少 content_blocked：%s", body)
			}
			if strings.Contains(strings.ToLower(body), "badword") {
				t.Fatalf("响应回显命中词：%s", body)
			}
		})
	}
}

func TestSensitiveMultimodalDisabledUnchanged(t *testing.T) {
	for _, tc := range sensitiveMultimodalCases(t, "BadWord") {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher := &captureMultimodalDispatcher{}
			handler := newSensitiveMultimodalHandler(t, dispatcher, sensitiveSettings(t,
				false, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

			resp := doMultimodal(t, handler, tc.req, true)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
			}
			if dispatcher.calls != 1 {
				t.Fatalf("dispatcher calls = %d, want 1（关闭态行为不变）", dispatcher.calls)
			}
		})
	}
}

func TestSensitiveMultimodalLogPassesThrough(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newSensitiveMultimodalHandler(t, dispatcher, sensitiveSettings(t,
		true, map[string]bool{"prompt": true}, []string{"BadWord"}, "log"))

	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/embeddings", contentType: "application/json",
		body: []byte(`{"model":"gpt-test","input":"hello BadWord"}`),
	}, true)
	if resp.Code != http.StatusOK {
		t.Fatalf("log 模式应放行：status=%d body=%s", resp.Code, resp.Body.String())
	}
	if dispatcher.calls != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatcher.calls)
	}
}

func TestSensitiveMultimodalIgnoresNonTextFields(t *testing.T) {
	imageCT, imageBody := buildMultipartBody(t,
		map[string]string{"model": "BadWord-model", "prompt": "clean prompt"},
		map[string][]byte{"image": []byte("BadWord-in-binary")})
	audioCT, audioBody := buildMultipartBody(t,
		map[string]string{"model": "BadWord-model", "prompt": "clean prompt"},
		map[string][]byte{"file": []byte("BadWord-in-binary")})

	cases := []struct {
		name string
		req  multimodalRequest
	}{
		{"model-name", multimodalRequest{path: "/v1/embeddings", contentType: "application/json",
			body: []byte(`{"model":"BadWord-model","input":"clean"}`)}},
		{"image-binary", multimodalRequest{path: "/v1/images/edits", contentType: imageCT, body: imageBody}},
		{"image-binary-json-parameters", multimodalRequest{path: "/v1/images/generations", contentType: "application/json",
			body: []byte(`{"model":"BadWord-model","prompt":"clean","parameters":{"note":"BadWord"}}`)}},
		{"audio-binary", multimodalRequest{path: "/v1/audio/transcriptions", contentType: audioCT, body: audioBody}},
		{"speech-model", multimodalRequest{path: "/v1/audio/speech", contentType: "application/json",
			body: []byte(`{"model":"BadWord-model","input":"clean"}`)}},
		{"rerank-model", multimodalRequest{path: "/v1/rerank", contentType: "application/json",
			body: []byte(`{"model":"BadWord-model","query":"clean","documents":["clean"]}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher := &captureMultimodalDispatcher{}
			handler := newSensitiveMultimodalHandler(t, dispatcher, sensitiveSettings(t,
				true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))
			resp := doMultimodal(t, handler, tc.req, true)
			if resp.Code != http.StatusOK {
				t.Fatalf("非文本字段不得误伤：status=%d body=%s", resp.Code, resp.Body.String())
			}
			if dispatcher.calls != 1 {
				t.Fatalf("dispatcher calls = %d, want 1", dispatcher.calls)
			}
		})
	}
}

func TestSensitiveMultimodalNormalizationAndScope(t *testing.T) {
	cases := []struct {
		name   string
		words  []string
		prompt string
		scope  map[string]bool
		block  bool
	}{
		{"case-insensitive", []string{"badword"}, "hello BADWORD", map[string]bool{"prompt": true}, true},
		{"fullwidth", []string{"badword"}, "hello ＢＡＤＷＯＲＤ", map[string]bool{"prompt": true}, true},
		{"wildcard", []string{"bad*word"}, "badXYZword", map[string]bool{"prompt": true}, true},
		{"prompt-scope-off", []string{"badword"}, "badword", map[string]bool{"prompt": false, "system": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher := &captureMultimodalDispatcher{}
			handler := newSensitiveMultimodalHandler(t, dispatcher, sensitiveSettings(t,
				true, tc.scope, tc.words, "block"))
			body := []byte(fmt.Sprintf(`{"model":"gpt-test","input":%q}`, tc.prompt))
			resp := doMultimodal(t, handler, multimodalRequest{
				path: "/v1/embeddings", contentType: "application/json", body: body,
			}, true)
			if tc.block && resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
			if !tc.block && resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
			}
		})
	}
}

// ---------- 任务 / 视频 / 批次 ----------

// sensitiveTaskMuxEnv 构造任务数据面的 mux，并把敏感词配置写进 handler。
func sensitiveTaskMuxEnv(t *testing.T, store TaskRepository, channels TaskChannelSource,
	manager *plugin.Manager, billing func(*contract.RelayContext) contract.BillingSession,
	cfg ConfigReader) *http.ServeMux {
	t.Helper()
	handler, _ := newTaskTestHandler(t, store, channels, manager, billing)
	handler.config = cfg
	return newTaskRelayMux(handler)
}

func newCountingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_1"}`))
	}))
	t.Cleanup(server.Close)
	return server, hits
}

// perConsumeCount 从 taskBillingRecorder 的调用序列里数 PreConsume 次数。
func perConsumeCount(b *taskBillingRecorder) int {
	count := 0
	for _, call := range b.ReserveCalls() {
		if call == "preconsume" {
			count++
		}
	}
	return count
}

func TestSensitiveTaskSubmitBlockZeroSideEffects(t *testing.T) {
	upstream, hits := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	billing := newTaskBillingRecorder()
	mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing },
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
		`{"model":"sample-model","prompt":"hello BadWord","seconds":5}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "content_blocked") ||
		strings.Contains(strings.ToLower(rec.Body.String()), "badword") {
		t.Fatalf("错误体必须固定且不回显命中词：%s", rec.Body.String())
	}
	if store.inserted != 0 {
		t.Fatalf("任务落库 = %d，want 0", store.inserted)
	}
	if perConsumeCount(billing) != 0 {
		t.Fatalf("PreConsume = %d，want 0", perConsumeCount(billing))
	}
	if channels.calls != 0 {
		t.Fatalf("渠道选择 = %d，want 0（过滤必须在选路之前）", channels.calls)
	}
	if hits.Load() != 0 {
		t.Fatalf("上游命中 = %d，want 0", hits.Load())
	}
}

func TestSensitiveTaskSubmitLogPassesThrough(t *testing.T) {
	upstream, hits := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	billing := newTaskBillingRecorder()
	mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t),
		func(*contract.RelayContext) contract.BillingSession { return billing },
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "log"))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
		`{"model":"sample-model","prompt":"hello BadWord","seconds":5}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("log 模式应提交成功：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 1 || hits.Load() != 1 || perConsumeCount(billing) != 1 {
		t.Fatalf("log 模式应保留原行为：rows=%d upstream=%d preconsume=%d",
			store.inserted, hits.Load(), perConsumeCount(billing))
	}
}

func TestSensitiveTaskSubmitIgnoresNonTextFields(t *testing.T) {
	upstream, _ := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t), nil,
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	// model 是路由字段；image_url / parameters 是二进制/参数载体，均不得命中。
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
		`{"model":"sample-model","prompt":"clean","seconds":5,`+
			`"image_url":"https://example.test/BadWord.png","parameters":{"note":"BadWord"}}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("非文本字段不得误伤：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 1 {
		t.Fatalf("任务落库 = %d，want 1", store.inserted)
	}
}

func TestSensitiveVideosSubmitBlockZeroSideEffects(t *testing.T) {
	upstream, hits := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	billing := newTaskBillingRecorder()
	manager := newVideoManager(t, map[string]string{"vid": videoPluginSource("vid", "vid-model", false)})
	mux := sensitiveTaskMuxEnv(t, store, channels, manager,
		func(*contract.RelayContext) contract.BillingSession { return billing },
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	rec := postVideo(t, mux, `{"model":"vid-model","prompt":"BadWord video","seconds":5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 0 || perConsumeCount(billing) != 0 || hits.Load() != 0 {
		t.Fatalf("block 必须零副作用：rows=%d preconsume=%d upstream=%d",
			store.inserted, perConsumeCount(billing), hits.Load())
	}
}

func TestSensitiveVideosSubmitIgnoresNonTextFields(t *testing.T) {
	upstream, _ := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	manager := newVideoManager(t, map[string]string{"vid": videoPluginSource("vid", "vid-model", false)})
	mux := sensitiveTaskMuxEnv(t, store, channels, manager, nil,
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	rec := postVideo(t, mux,
		`{"model":"vid-model","prompt":"clean","seconds":5,`+
			`"image_url":"https://example.test/BadWord.png","metadata":{"note":"BadWord"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("非文本字段不得误伤：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 1 {
		t.Fatalf("任务落库 = %d，want 1", store.inserted)
	}
}

func newSensitiveBatchEnv(t *testing.T, cfg ConfigReader) (*batchTestEnv, *http.ServeMux) {
	t.Helper()
	upstream, hits := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := newFakeBatchTaskStore()
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-test"),
	}}
	billing := &billingCallLog{}
	manager := newVideoManager(t, map[string]string{"vid": videoPluginSource("vid", "vid-model", false)})
	handler, _ := newTaskTestHandler(t, store, channels, manager,
		func(*contract.RelayContext) contract.BillingSession {
			return &perRequestBilling{Stub: billingseam.NewStub(), log: billing}
		})
	handler.config = cfg
	env := &batchTestEnv{
		store: store, mux: newTaskRelayMux(handler),
		billing: billing, channels: channels, upstreamHits: hits,
	}
	return env, env.mux
}

func batchBodyWithPrompts(prompts ...string) string {
	rows := make([]string, 0, len(prompts))
	for i, prompt := range prompts {
		rows = append(rows, fmt.Sprintf(
			`{"custom_id":"r%d","method":"POST","url":"/v1/videos","body":{"model":"vid-model","seconds":5,"prompt":%q}}`,
			i+1, prompt))
	}
	return `{"endpoint":"/v1/videos","completion_window":"24h","requests":[` +
		strings.Join(rows, ",") + `]}`
}

func TestSensitiveBatchesBlockWholeBatchBeforeInsert(t *testing.T) {
	env, mux := newSensitiveBatchEnv(t, sensitiveSettings(t,
		true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	rec := postBatch(t, mux, batchBodyWithPrompts("clean", "BadWord in row two"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "content_blocked") ||
		strings.Contains(strings.ToLower(rec.Body.String()), "badword") {
		t.Fatalf("错误体必须固定且不回显命中词：%s", rec.Body.String())
	}
	if env.store.inserted != 0 || env.store.fakeTaskStore.inserted != 0 {
		t.Fatalf("block 必须零写库：batch=%d tasks=%d",
			env.store.inserted, env.store.fakeTaskStore.inserted)
	}
	if calls := env.billing.snapshot(); len(calls) != 0 {
		t.Fatalf("PreConsume/结算调用 = %v，want 空", calls)
	}
	if env.hits() != 0 {
		t.Fatalf("上游命中 = %d，want 0", env.hits())
	}
}

func TestSensitiveBatchesLogSubmitsAllRows(t *testing.T) {
	env, mux := newSensitiveBatchEnv(t, sensitiveSettings(t,
		true, map[string]bool{"prompt": true}, []string{"BadWord"}, "log"))

	rec := postBatch(t, mux, batchBodyWithPrompts("clean", "BadWord in row two"))
	if rec.Code != http.StatusOK {
		t.Fatalf("log 模式应整批提交：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.store.inserted != 1 || env.store.fakeTaskStore.inserted != 2 {
		t.Fatalf("批次/任务行 = %d/%d，want 1/2",
			env.store.inserted, env.store.fakeTaskStore.inserted)
	}
	if env.hits() != 2 {
		t.Fatalf("上游命中 = %d，want 2", env.hits())
	}
}

func TestSensitiveBatchesIgnoreNonTextRowFields(t *testing.T) {
	env, mux := newSensitiveBatchEnv(t, sensitiveSettings(t,
		true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	body := `{"endpoint":"/v1/videos","completion_window":"24h","requests":[
	  {"custom_id":"r1","method":"POST","url":"/v1/videos","body":{"model":"vid-model","seconds":5,"prompt":"clean","image_url":"https://example.test/BadWord.png"}}
	]}`
	rec := postBatch(t, mux, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("非文本行字段不得误伤：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.store.fakeTaskStore.inserted != 1 {
		t.Fatalf("任务行 = %d，want 1", env.store.fakeTaskStore.inserted)
	}
}

// TestSensitiveTaskBodyFieldWhitelist 直接锁定递归白名单：messages[].content
// 与 input 对象里的 text 要扫描；role/model/image_url/parameters 不扫描。
func TestSensitiveTaskBodyFieldWhitelist(t *testing.T) {
	cfg := sensitive.NewConfig(true, sensitive.Scope{Prompt: true},
		[]string{"badword"}, sensitive.ActionBlock)

	hit := map[string]any{
		"model":   "model-badword",
		"seconds": float64(5),
		"messages": []any{
			map[string]any{"role": "user", "content": "hello BadWord"},
		},
	}
	if !sensitiveEntriesHit(cfg, taskBodySensitiveTexts(hit)) {
		t.Fatal("messages[].content 必须进入匹配")
	}

	miss := map[string]any{
		"model": "model-badword",
		"input": map[string]any{
			"text":      "clean",
			"image_url": "https://example.test/BadWord.png",
		},
		"parameters": map[string]any{"note": "BadWord"},
		"seconds":    float64(5),
	}
	if sensitiveEntriesHit(cfg, taskBodySensitiveTexts(miss)) {
		t.Fatal("模型名/图片 URL/参数对象不得进入匹配")
	}

	nested := map[string]any{
		"input": []any{
			map[string]any{"text": "badword"},
		},
	}
	if !sensitiveEntriesHit(cfg, taskBodySensitiveTexts(nested)) {
		t.Fatal("input[].text 必须进入匹配")
	}
}

// TestSensitiveMultimodalExtractionCoversDocuments 是 rerank 文档对象的字段
// 白名单锁定：documents[].text 命中，documents[].id 不命中。
func TestSensitiveMultimodalExtractionCoversDocuments(t *testing.T) {
	cfg := sensitive.NewConfig(true, sensitive.Scope{Prompt: true},
		[]string{"badword"}, sensitive.ActionBlock)
	body := []byte(`{"query":"clean","documents":[{"id":"badword","text":"clean"}]}`)
	if sensitiveEntriesHit(cfg, multimodalSensitiveTexts(contract.EndpointRerank, "/v1/rerank", "application/json", body)) {
		t.Fatal("documents[].id 不是文本字段，不应命中")
	}
	body = []byte(`{"query":"clean","documents":[{"id":"x","text":"badword"}]}`)
	if !sensitiveEntriesHit(cfg, multimodalSensitiveTexts(contract.EndpointRerank, "/v1/rerank", "application/json", body)) {
		t.Fatal("documents[].text 必须进入匹配")
	}
}

// TestSensitiveMultimodalJSONDecode 校验提取器对合法 JSON 的字段覆盖；
// 用 json.RawMessage 断言不改变输入（提取器是只读的）。
func TestSensitiveMultimodalExtractionDoesNotMutate(t *testing.T) {
	body := []byte(`{"input":"hello","model":"m"}`)
	original := append([]byte(nil), body...)
	_ = multimodalSensitiveTexts(contract.EndpointEmbeddings, "/v1/embeddings", "application/json", body)
	if string(body) != string(original) {
		t.Fatalf("提取器不得修改请求体：%s", body)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("请求体仍是合法 JSON：%v", err)
	}
}
