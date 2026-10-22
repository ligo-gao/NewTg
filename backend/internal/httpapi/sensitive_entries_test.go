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
	"log/slog"
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

// ---------- 命中日志断言 ----------

// captureSensitiveLogs 把 slog.Default 临时指向缓冲。
//
// logSensitiveHitEvent 在 logger=nil 时走 slog.Default（与任务面既有的
// slog.Warn 一致），因此任务/视频/批次的命中日志只能从默认 logger 观察。
// t.Cleanup 保证恢复，避免污染同包其它用例。
func captureSensitiveLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// sensitiveHitLogLines 只返回 security.sensitive_word_hit 事件行。
//
// 每行同时包含 msg 与 event 两份事件名，因此按 `event=` 字段计数，
// 不用 strings.Count 以免每条日志被算两次。
func sensitiveHitLogLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "event=security.sensitive_word_hit") {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertSensitiveHitLog 断言命中日志字段齐全且不含原文/命中词
// （F-08-18「不落明文命中词」）。
func assertSensitiveHitLog(t *testing.T, line, action, surface, plaintext string) {
	t.Helper()
	for _, field := range []string{
		"action=" + action,
		"surface=" + surface,
		"request_id=",
		"user_id=",
	} {
		if !strings.Contains(line, field) {
			t.Fatalf("命中日志缺少字段 %q：%s", field, line)
		}
	}
	if strings.Contains(strings.ToLower(line), strings.ToLower(plaintext)) {
		t.Fatalf("命中日志泄露原文/命中词：%s", line)
	}
}

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
		// M2：Gemini 原生 /v1beta 路径与 /v1/models 同实现，但路由/校验分支
		// 不同，必须有独立用例防止只覆盖其中一条。
		{"embeddings-gemini-v1beta", multimodalRequest{
			path: "/v1beta/models/gemini-embed:embedContent", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"content":{"parts":[{"text":%q}]}}`, text))}},
		// M1：报告曾把 requests[].content.parts[].text（:batchEmbedContents）
		// 声明为「单测+集成」，此前并无用例；这里补上真实覆盖。
		{"embeddings-gemini-batch", multimodalRequest{
			path: "/v1beta/models/gemini-embed:batchEmbedContents", contentType: "application/json",
			body: []byte(fmt.Sprintf(
				`{"requests":[{"content":{"parts":[{"text":"clean"}]}},{"content":{"parts":[{"text":%q}]}}]}`, text))}},
		// M3：Gemini 原生 body 原样出站，顶层 input 此前不扫描；
		// 复核给出的绕过样例就是 {"input":"BadWord"}。
		{"embeddings-gemini-input", multimodalRequest{
			path: "/v1beta/models/gemini-embed:embedContent", contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"input":%q}`, text))}},
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
	logs := captureSensitiveLogs(t)
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
	lines := sensitiveHitLogLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("log 模式命中日志 = %d 条，want 1：%s", len(lines), logs.String())
	}
	assertSensitiveHitLog(t, lines[0], "log", "prompt", "BadWord")
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

// TestSensitiveVideosSubmitLogPassesThrough 覆盖 I2 的 videos log 态：
// 命中 → 200 放行、上游 1 次、恰好 1 条命中日志。
func TestSensitiveVideosSubmitLogPassesThrough(t *testing.T) {
	logs := captureSensitiveLogs(t)
	upstream, hits := newCountingUpstream(t)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	manager := newVideoManager(t, map[string]string{"vid": videoPluginSource("vid", "vid-model", false)})
	mux := sensitiveTaskMuxEnv(t, store, channels, manager, nil,
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "log"))

	rec := postVideo(t, mux, `{"model":"vid-model","prompt":"BadWord video","seconds":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("log 模式应放行：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.inserted != 1 || hits.Load() != 1 {
		t.Fatalf("log 模式原行为：rows=%d upstream=%d", store.inserted, hits.Load())
	}
	lines := sensitiveHitLogLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("命中日志 = %d 条，want 1：%s", len(lines), logs.String())
	}
	assertSensitiveHitLog(t, lines[0], "log", "prompt", "BadWord")
}

// TestSensitiveTaskEntriesDisabledPassThrough 覆盖 I2 的关闭态：
// enabled=false 时命中文本仍放行、上游照常调用、不产生命中日志。
func TestSensitiveTaskEntriesDisabledPassThrough(t *testing.T) {
	t.Run("tasks", func(t *testing.T) {
		logs := captureSensitiveLogs(t)
		upstream, hits := newCountingUpstream(t)
		sealer := NewSecretSealer("task-handler-test-key")
		store := &fakeTaskStore{}
		channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
			ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
		}}
		billing := newTaskBillingRecorder()
		mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t),
			func(*contract.RelayContext) contract.BillingSession { return billing },
			sensitiveSettings(t, false, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
			`{"model":"sample-model","prompt":"hello BadWord","seconds":5}`))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("关闭态应放行：status=%d body=%s", rec.Code, rec.Body.String())
		}
		if store.inserted != 1 || hits.Load() != 1 || perConsumeCount(billing) != 1 {
			t.Fatalf("关闭态原行为：rows=%d upstream=%d preconsume=%d",
				store.inserted, hits.Load(), perConsumeCount(billing))
		}
		if lines := sensitiveHitLogLines(logs.String()); len(lines) != 0 {
			t.Fatalf("关闭态不得记录命中日志：%v", lines)
		}
	})

	t.Run("videos", func(t *testing.T) {
		logs := captureSensitiveLogs(t)
		upstream, hits := newCountingUpstream(t)
		sealer := NewSecretSealer("task-handler-test-key")
		store := &fakeTaskStore{}
		channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
			ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
		}}
		manager := newVideoManager(t, map[string]string{"vid": videoPluginSource("vid", "vid-model", false)})
		mux := sensitiveTaskMuxEnv(t, store, channels, manager, nil,
			sensitiveSettings(t, false, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

		rec := postVideo(t, mux, `{"model":"vid-model","prompt":"BadWord video","seconds":5}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("关闭态应放行：status=%d body=%s", rec.Code, rec.Body.String())
		}
		if store.inserted != 1 || hits.Load() != 1 {
			t.Fatalf("关闭态原行为：rows=%d upstream=%d", store.inserted, hits.Load())
		}
		if lines := sensitiveHitLogLines(logs.String()); len(lines) != 0 {
			t.Fatalf("关闭态不得记录命中日志：%v", lines)
		}
	})

	t.Run("batches", func(t *testing.T) {
		logs := captureSensitiveLogs(t)
		env, mux := newSensitiveBatchEnv(t, sensitiveSettings(t,
			false, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

		rec := postBatch(t, mux, batchBodyWithPrompts("clean", "BadWord in row two"))
		if rec.Code != http.StatusOK {
			t.Fatalf("关闭态应放行：status=%d body=%s", rec.Code, rec.Body.String())
		}
		if env.store.inserted != 1 || env.store.fakeTaskStore.inserted != 2 || env.hits() != 2 {
			t.Fatalf("关闭态原行为：batch=%d tasks=%d upstream=%d",
				env.store.inserted, env.store.fakeTaskStore.inserted, env.hits())
		}
		if lines := sensitiveHitLogLines(logs.String()); len(lines) != 0 {
			t.Fatalf("关闭态不得记录命中日志：%v", lines)
		}
	})
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

// TestSensitiveBatchesBlockScansRowsBeforeInsert 是 I1 的整批拒绝证据：
// 3 行里 2 行命中 → 400、零计费/零任务行/零上游。
//
// 父行（batch 级）字段只有 endpoint/completion_window，不含命中词；命中只
// 能来自 requests[].body。父行预扫在第一个命中行短路（S2 裁决：任一行命中
// 即整批 400，与 validateBatchItems 的整批参数错误口径一致），所以 block
// 模式只产生 1 条预扫日志；「逐行各一条」由下面的 log 模式用例独立锁定。
func TestSensitiveBatchesBlockScansRowsBeforeInsert(t *testing.T) {
	logs := captureSensitiveLogs(t)
	env, mux := newSensitiveBatchEnv(t, sensitiveSettings(t,
		true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"))

	rec := postBatch(t, mux, batchBodyWithPrompts("clean", "BadWord in row two", "BadWord in row three"))
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
	lines := sensitiveHitLogLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("block 预扫日志 = %d 条，want 1（父行预扫短路）：%s", len(lines), logs.String())
	}
	assertSensitiveHitLog(t, lines[0], "block", "prompt", "BadWord")
}

// TestSensitiveBatchesLogScansEachRowIndependently 是变异 D 的判别用例。
//
// 变异 D（去掉 handleSubmit 的逐行敏感词检查，只留父行预扫）在 block 模式
// 下不可观测：父行预扫会先短路整批。log 模式下父行预扫主动跳过
// （action=log），命中只能由每行 handleSubmit 的逐行检查记录——去掉它，
// 这里的命中日志会从 2 条降到 0 条，用例转红。
func TestSensitiveBatchesLogScansEachRowIndependently(t *testing.T) {
	logs := captureSensitiveLogs(t)
	env, mux := newSensitiveBatchEnv(t, sensitiveSettings(t,
		true, map[string]bool{"prompt": true}, []string{"BadWord"}, "log"))

	// 父行（batch 级）字段不含命中词，只有 requests[].body 内含命中词：
	// 命中只能来自逐行扫描。
	rec := postBatch(t, mux, batchBodyWithPrompts("clean", "BadWord in row two", "BadWord in row three"))
	if rec.Code != http.StatusOK {
		t.Fatalf("log 模式应整批提交：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.store.inserted != 1 || env.store.fakeTaskStore.inserted != 3 || env.hits() != 3 {
		t.Fatalf("log 模式行列数：batch=%d tasks=%d upstream=%d，want 1/3/3",
			env.store.inserted, env.store.fakeTaskStore.inserted, env.hits())
	}
	lines := sensitiveHitLogLines(logs.String())
	if len(lines) != 2 {
		t.Fatalf("逐行命中日志 = %d 条，want 2（2 个命中行各一条）：%s",
			len(lines), logs.String())
	}
	for _, line := range lines {
		assertSensitiveHitLog(t, line, "log", "prompt", "BadWord")
	}
}

// TestSensitiveOpaqueMessagesScanUnderSystemOnly 锁定 S1 裁决二：
// 任务/视频/批次这类 opaque 插件 JSON 无法区分 messages 的角色，因此
// scope.prompt=false && scope.system=true 时仍按安全向扫描 messages[].content；
// 而 embeddings 这类透明输入保持 prompt 面，同一 scope 下放行。
func TestSensitiveOpaqueMessagesScanUnderSystemOnly(t *testing.T) {
	systemOnly := sensitiveSettings(t, true,
		map[string]bool{"prompt": false, "system": true}, []string{"BadWord"}, "block")

	t.Run("tasks", func(t *testing.T) {
		logs := captureSensitiveLogs(t)
		upstream, hits := newCountingUpstream(t)
		sealer := NewSecretSealer("task-handler-test-key")
		store := &fakeTaskStore{}
		channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
			ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
		}}
		billing := newTaskBillingRecorder()
		mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t),
			func(*contract.RelayContext) contract.BillingSession { return billing }, systemOnly)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
			`{"model":"sample-model","seconds":5,"messages":[{"role":"user","content":"hello BadWord"}]}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "content_blocked") {
			t.Fatalf("system-only 应扫描 messages：status=%d body=%s", rec.Code, rec.Body.String())
		}
		if store.inserted != 0 || hits.Load() != 0 || perConsumeCount(billing) != 0 {
			t.Fatalf("system-only block 必须零副作用：rows=%d upstream=%d preconsume=%d",
				store.inserted, hits.Load(), perConsumeCount(billing))
		}
		lines := sensitiveHitLogLines(logs.String())
		if len(lines) != 1 {
			t.Fatalf("命中日志 = %d 条，want 1：%s", len(lines), logs.String())
		}
		assertSensitiveHitLog(t, lines[0], "block", "system", "BadWord")
	})

	t.Run("batches", func(t *testing.T) {
		env, mux := newSensitiveBatchEnv(t, systemOnly)
		body := `{"endpoint":"/v1/videos","completion_window":"24h","requests":[
		  {"custom_id":"r1","method":"POST","url":"/v1/videos","body":{"model":"vid-model","seconds":5,"messages":[{"role":"user","content":"BadWord"}]}}
		]}`
		rec := postBatch(t, mux, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "content_blocked") {
			t.Fatalf("system-only 批次应扫描 messages：status=%d body=%s", rec.Code, rec.Body.String())
		}
		if env.store.inserted != 0 || env.store.fakeTaskStore.inserted != 0 || env.hits() != 0 {
			t.Fatalf("system-only block 必须零副作用：batch=%d tasks=%d upstream=%d",
				env.store.inserted, env.store.fakeTaskStore.inserted, env.hits())
		}
	})

	t.Run("transparent-embeddings-stay-prompt", func(t *testing.T) {
		dispatcher := &captureMultimodalDispatcher{}
		handler := newSensitiveMultimodalHandler(t, dispatcher, systemOnly)
		resp := doMultimodal(t, handler, multimodalRequest{
			path: "/v1/embeddings", contentType: "application/json",
			body: []byte(`{"model":"gpt-test","input":"BadWord"}`),
		}, true)
		if resp.Code != http.StatusOK || dispatcher.calls != 1 {
			t.Fatalf("透明输入只属于 prompt 面：status=%d calls=%d body=%s",
				resp.Code, dispatcher.calls, resp.Body.String())
		}
	})
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

// TestSensitiveGeminiInputShapesAreScanned 锁定 M3 扩展的扫描面：
// 顶层 input 的 string / []string / []{text|content} 与 contents[].parts[].text
// 都要命中；未知形态与非文本载荷不扫描（保持白名单语义）。
func TestSensitiveGeminiInputShapesAreScanned(t *testing.T) {
	cfg := sensitive.NewConfig(true, sensitive.Scope{Prompt: true},
		[]string{"badword"}, sensitive.ActionBlock)
	path := "/v1beta/models/gemini-embed:embedContent"
	cases := []string{
		`{"input":"badword"}`,
		`{"input":["clean","badword"]}`,
		`{"input":[{"text":"badword"}]}`,
		`{"input":[{"content":"badword"}]}`,
		`{"contents":[{"parts":[{"text":"badword"}]}]}`,
	}
	for _, body := range cases {
		if !sensitiveEntriesHit(cfg,
			multimodalSensitiveTexts(contract.EndpointEmbeddings, path, "application/json", []byte(body))) {
			t.Fatalf("%s 必须进入匹配", body)
		}
	}
	// 不递归任意字符串叶子：未知形态与非文本载荷保持放行。
	for _, body := range []string{
		`{"input":123}`,
		`{"input":{"image":"badword.png"}}`,
		`{"unknown":{"text":"badword"}}`,
	} {
		if sensitiveEntriesHit(cfg,
			multimodalSensitiveTexts(contract.EndpointEmbeddings, path, "application/json", []byte(body))) {
			t.Fatalf("%s 不应进入匹配（M3 不递归扫描任意字符串叶子）", body)
		}
	}
}

// TestSensitiveMultimodalExtractionFailOpenDoesNotPanic 锁定 M4：
// 扫描器二次解析失败是 fail-open（入口校验已先成功解析，此分支只在
// 校验器与扫描器口径漂移时可达），不得 panic 或改变拒绝语义。
func TestSensitiveMultimodalExtractionFailOpenDoesNotPanic(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"general-invalid-json", "/v1/embeddings", `{not json`},
		{"gemini-invalid-json", "/v1/models/gemini-embed:embedContent", `{not json`},
		{"gemini-structure-mismatch", "/v1/models/gemini-embed:embedContent",
			`{"content":{"parts":"oops"}}`},
		{"gemini-input-unrecognised-shape", "/v1beta/models/gemini-embed:embedContent",
			`{"input":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			texts := multimodalSensitiveTexts(contract.EndpointEmbeddings, tc.path,
				"application/json", []byte(tc.body))
			if len(texts) != 0 {
				t.Fatalf("解析失败应返回空文本（放行），得到 %v", texts)
			}
		})
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
