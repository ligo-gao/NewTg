package httpapi

// 本文件是 M4 Task 1（多模态端点族路由骨架）的行为测试。
//
// 纪律与 M2 中继测试一致：不连真 PG/Redis；令牌/用户/渠道用内存 fake；
// Dispatcher 用捕获 fake，从而只断言「路由、鉴权、入口校验、上下文组装」
// 四个 T1 责任面。
//
// 覆盖（task-1-brief 测试清单）：
//   - 全部端点未鉴权 401；鉴权后非 404（T1 注入 fake 后为 200）；
//   - /v1/images/variations 与 legacy /v1/edits 为 404；
//   - GET /v1/models 仍由 M2 提供且为 200（不重复注册）；
//   - 图像 n/size/quality/prompt 越界 400（JSON 与 multipart 各一组）；
//   - 音频 multipart 缺文件/缺 model 400；整体 body 超 32 MiB 413；
//   - 未注入 dispatcher 时 501（仅 T1 测试态）。

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/multimodal"
)

// captureMultimodalDispatcher 记录分派参数并返回 200。
type captureMultimodalDispatcher struct {
	mu       sync.Mutex
	calls    int
	endpoint contract.Endpoint
	rc       *contract.RelayContext
	body     []byte
}

func (d *captureMultimodalDispatcher) Handle(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, endpoint contract.Endpoint) {
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.calls++
	d.endpoint = endpoint
	d.rc = rc
	d.body = body
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func newMultimodalTestHandler(t *testing.T, dispatcher MultimodalDispatcher) http.Handler {
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
	})
	return h.Handler()
}

type multimodalRequest struct {
	method      string
	path        string
	body        []byte
	contentType string
}

func doMultimodal(t *testing.T, handler http.Handler, req multimodalRequest, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	method := req.method
	if method == "" {
		method = http.MethodPost
	}
	httpReq := httptest.NewRequest(method, req.path, bytes.NewReader(req.body))
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	if authenticated {
		httpReq.Header.Set("Authorization", "Bearer "+relayTestToken)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httpReq)
	return w
}

func buildMultipartBody(t *testing.T, fields map[string]string, files map[string][]byte) (string, []byte) {
	t.Helper()
	ordered := make([][2]string, 0, len(fields))
	for name, value := range fields {
		ordered = append(ordered, [2]string{name, value})
	}
	return buildMultipartOrdered(t, ordered, files)
}

// buildMultipartOrdered 允许重复字段，用于固化「重复计费标量必须 400」。
// multipart 测试文件的魔数与扩展名（T4 的 F4/F5：扩展名 ↔ MIME ↔ 魔数
// 三者一致）。helper 自动为文件 part 注入真实魔数，避免测试数据本身
// 触发安全校验；业务测试仍可显式构造非法文件覆盖拒绝路径。
var (
	testPNGMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
	testMP3Magic = []byte("ID3\x04\x00\x00\x00\x00\x00\x00")
)

func testFileFor(name string, value []byte) (string, []byte) {
	if len(value) == 0 {
		return "upload.bin", value
	}
	switch name {
	case "file":
		return "upload.mp3", append(append([]byte(nil), testMP3Magic...), value...)
	case "image", "mask":
		return "upload.png", append(append([]byte(nil), testPNGMagic...), value...)
	default:
		return "upload.bin", value
	}
}

func buildMultipartOrdered(t *testing.T, fields [][2]string, files map[string][]byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			t.Fatalf("write field %s: %v", field[0], err)
		}
	}
	for name, value := range files {
		filename, content := testFileFor(name, value)
		part, err := writer.CreateFormFile(name, filename)
		if err != nil {
			t.Fatalf("create file %s: %v", name, err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write file %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return writer.FormDataContentType(), buf.Bytes()
}

func validImageJSON() []byte {
	return []byte(`{"model":"img-model","prompt":"a cat","n":1,"size":"1024x1024","quality":"hd"}`)
}

func validAudioMultipart(t *testing.T, path, model string) multimodalRequest {
	t.Helper()
	contentType, body := buildMultipartBody(t, map[string]string{"model": model}, map[string][]byte{"file": []byte("audio-bytes")})
	return multimodalRequest{path: path, body: body, contentType: contentType}
}

// TestMultimodal_RoutesAuthAndDispatch 覆盖路由表本身：每个端点都经过鉴权，
// 鉴权后到达 Dispatcher，且 endpoint 键正确。
func TestMultimodal_RoutesAuthAndDispatch(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)

	imageContentType, imageMultipartBody := buildMultipartBody(t,
		map[string]string{"model": "img-model", "prompt": "a cat"}, map[string][]byte{"image": []byte("fake-image")})
	audioContentType, audioBody := buildMultipartBody(t,
		map[string]string{"model": "whisper-1"}, map[string][]byte{"file": []byte("audio-bytes")})

	requests := []struct {
		name     string
		req      multimodalRequest
		endpoint contract.Endpoint
	}{
		{"image-generation", multimodalRequest{path: "/v1/images/generations", body: validImageJSON(), contentType: "application/json"}, contract.EndpointImageGenerate},
		{"image-edit-json", multimodalRequest{path: "/v1/images/edits", body: validImageJSON(), contentType: "application/json"}, contract.EndpointImageEdit},
		{"image-edit-multipart", multimodalRequest{path: "/v1/images/edits", body: imageMultipartBody, contentType: imageContentType}, contract.EndpointImageEdit},
		{"embeddings", multimodalRequest{path: "/v1/embeddings", body: []byte(`{"model":"emb","input":"hello"}`), contentType: "application/json"}, contract.EndpointEmbeddings},
		{"rerank", multimodalRequest{path: "/v1/rerank", body: []byte(`{"model":"re","query":"q","documents":["d"]}`), contentType: "application/json"}, contract.EndpointRerank},
		{"audio-speech", multimodalRequest{path: "/v1/audio/speech", body: []byte(`{"model":"tts","input":"hi","speed":1.0}`), contentType: "application/json"}, contract.EndpointAudioSpeech},
		{"audio-transcriptions", multimodalRequest{path: "/v1/audio/transcriptions", body: audioBody, contentType: audioContentType}, contract.EndpointAudioTranscribe},
		{"audio-translations", multimodalRequest{path: "/v1/audio/translations", body: audioBody, contentType: audioContentType}, contract.EndpointAudioTranslate},
		{"moderations", multimodalRequest{path: "/v1/moderations", body: []byte(`{"input":"hello"}`), contentType: "application/json"}, contract.EndpointModerations},
	}

	for _, tc := range requests {
		t.Run(tc.name+"-unauthorized", func(t *testing.T) {
			resp := doMultimodal(t, handler, tc.req, false)
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", resp.Code, resp.Body.String())
			}
		})
		t.Run(tc.name, func(t *testing.T) {
			before := dispatcher.calls
			resp := doMultimodal(t, handler, tc.req, true)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
			}
			dispatcher.mu.Lock()
			defer dispatcher.mu.Unlock()
			if dispatcher.calls != before+1 {
				t.Fatalf("dispatcher calls = %d, want %d", dispatcher.calls, before+1)
			}
			if dispatcher.endpoint != tc.endpoint {
				t.Fatalf("endpoint = %q, want %q", dispatcher.endpoint, tc.endpoint)
			}
			if dispatcher.rc == nil || dispatcher.rc.RequestPath != tc.req.path {
				t.Fatalf("RelayContext.RequestPath = %+v, want %q", dispatcher.rc, tc.req.path)
			}
		})
	}
}

// TestMultimodal_RemovedRoutesAre404 固化 R-W18-01 / 裁决 H：明确不提供的
// 端点族不注册路由 → 任何方法都 404，且**不经 RelayAuth**（无令牌也是 404，
// 不能是 401），不进入 dispatcher（零预扣、零上游）。
func TestMultimodal_RemovedRoutesAre404(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)
	paths := []string{
		"/v1/files", "/v1/files/file-1", "/v1/files/file-1/content",
		"/v1/fine-tunes", "/v1/fine-tunes/job-1", "/v1/fine-tunes/job-1/events",
		"/v1/images/variations", "/v1/edits",
	}
	methods := []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch}
	for _, path := range paths {
		for _, method := range methods {
			for _, authenticated := range []bool{false, true} {
				resp := doMultimodal(t, handler, multimodalRequest{method: method, path: path, body: validImageJSON(), contentType: "application/json"}, authenticated)
				if resp.Code != http.StatusNotFound {
					t.Fatalf("%s %s (auth=%v): status = %d, want 404; body=%s",
						method, path, authenticated, resp.Code, resp.Body.String())
				}
			}
		}
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0（不提供的端点不得进入分发/计费）", dispatcher.calls)
	}
}

// TestMultimodal_RealtimeIsGone 固化 R-W18-12：GET /v1/realtime 的 501 半死桩
// 已删除，任何方法都落 404（不注册路由），且不触上游/不进入 dispatcher。
func TestMultimodal_RealtimeIsGone(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, authenticated := range []bool{false, true} {
			resp := doMultimodal(t, handler, multimodalRequest{method: method, path: "/v1/realtime"}, authenticated)
			if resp.Code != http.StatusNotFound {
				t.Fatalf("%s /v1/realtime (auth=%v): status = %d, want 404; body=%s",
					method, authenticated, resp.Code, resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), "not_implemented") {
				t.Fatalf("%s /v1/realtime: body = %s, want no 501 stub", method, resp.Body.String())
			}
		}
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// TestMultimodal_GeminiWildcardUnknownActionIsJSON404 覆盖 P3：Gemini 通配
// 路径上的非嵌入动作必须是 OpenAI JSON 信封的 404，而不是 text/plain。
func TestMultimodal_GeminiWildcardUnknownActionIsJSON404(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)
	for _, path := range []string{
		"/v1/models/gemini-pro:generateContent",
		"/v1beta/models/gemini-pro:countTokens",
	} {
		resp := doMultimodal(t, handler, multimodalRequest{
			path: path, body: []byte(`{"contents":[]}`), contentType: "application/json",
		}, true)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404; body=%s", path, resp.Code, resp.Body.String())
		}
		if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s Content-Type = %q, want application/json", path, ct)
		}
		if !strings.Contains(resp.Body.String(), `"code":"invalid_request_error"`) {
			t.Fatalf("%s body = %s, want OpenAI envelope", path, resp.Body.String())
		}
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// TestMultimodal_ModerationEntryValidation 覆盖 relay-04 §5.2/§9 的入口校验：
// input 必填/非空、形态、字节上界、max_tokens 族上界；缺省 model 取审核默认模型。
func TestMultimodal_ModerationEntryValidation(t *testing.T) {
	t.Run("default-model", func(t *testing.T) {
		dispatcher := &captureMultimodalDispatcher{}
		handler := newMultimodalTestHandler(t, dispatcher)
		resp := doMultimodal(t, handler, multimodalRequest{
			path: "/v1/moderations", body: []byte(`{"input":"hello"}`), contentType: "application/json",
		}, true)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
		}
		if dispatcher.rc == nil || dispatcher.rc.OriginModel != multimodal.DefaultModerationModel {
			t.Fatalf("OriginModel = %+v, want %q", dispatcher.rc, multimodal.DefaultModerationModel)
		}
	})

	t.Run("explicit-model-and-array-input", func(t *testing.T) {
		dispatcher := &captureMultimodalDispatcher{}
		handler := newMultimodalTestHandler(t, dispatcher)
		resp := doMultimodal(t, handler, multimodalRequest{
			path: "/v1/moderations", body: []byte(`{"model":"omni-x","input":["a","b"]}`), contentType: "application/json",
		}, true)
		if resp.Code != http.StatusOK || dispatcher.rc == nil || dispatcher.rc.OriginModel != "omni-x" {
			t.Fatalf("status=%d rc=%+v, want 200 / omni-x", resp.Code, dispatcher.rc)
		}
	})

	badBodies := map[string]string{
		"missing-input":   `{}`,
		"null-input":      `{"input":null}`,
		"empty-string":    `{"input":""}`,
		"empty-array":     `{"input":[]}`,
		"wrong-type":      `{"input":42}`,
		"negative-tokens": `{"input":"x","max_tokens":-1}`,
		"tokens-overflow": `{"input":"x","max_completion_tokens":2000000000}`,
		"oversize-input":  `{"input":"` + strings.Repeat("a", multimodal.MaxModerationInputBytes+1) + `"}`,
	}
	for name, body := range badBodies {
		t.Run(name, func(t *testing.T) {
			dispatcher := &captureMultimodalDispatcher{}
			handler := newMultimodalTestHandler(t, dispatcher)
			resp := doMultimodal(t, handler, multimodalRequest{
				path: "/v1/moderations", body: []byte(body), contentType: "application/json",
			}, true)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
			if dispatcher.calls != 0 {
				t.Fatalf("dispatcher calls = %d, want 0（入口拒绝不得进入分发/计费）", dispatcher.calls)
			}
		})
	}
}

// TestMultimodal_ModelsStillServedByM2 确认 R-09：多模态注册不重复注册
// GET /v1/models（重复注册会让 ServeMux 在装配期 panic），且该端点仍为 200。
func TestMultimodal_ModelsStillServedByM2(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "gpt-test" {
		t.Fatalf("models = %+v, want gpt-test", list.Data)
	}
}

// TestMultimodal_ImageJSONValidation 覆盖 JSON 图像入口的全部上界。
func TestMultimodal_ImageJSONValidation(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing-model", `{"prompt":"a cat"}`, http.StatusBadRequest},
		{"blank-model", `{"model":"  ","prompt":"a cat"}`, http.StatusBadRequest},
		{"missing-prompt", `{"model":"img"}`, http.StatusBadRequest},
		{"blank-prompt", `{"model":"img","prompt":"   "}`, http.StatusBadRequest},
		{"prompt-too-long", `{"model":"img","prompt":"` + strings.Repeat("a", MaxImagePromptBytes+1) + `"}`, http.StatusBadRequest},
		{"n-zero", `{"model":"img","prompt":"a cat","n":0}`, http.StatusBadRequest},
		{"n-too-large", `{"model":"img","prompt":"a cat","n":129}`, http.StatusBadRequest},
		{"n-float", `{"model":"img","prompt":"a cat","n":1.5}`, http.StatusBadRequest},
		{"n-negative", `{"model":"img","prompt":"a cat","n":-1}`, http.StatusBadRequest},
		{"n-string", `{"model":"img","prompt":"a cat","n":"2"}`, http.StatusBadRequest},
		{"n-integral-float", `{"model":"img","prompt":"a cat","n":2.0}`, http.StatusOK},
		{"n-max", `{"model":"img","prompt":"a cat","n":128}`, http.StatusOK},
		{"size-garbage", `{"model":"img","prompt":"a cat","size":"large"}`, http.StatusBadRequest},
		{"size-side-limit", `{"model":"img","prompt":"a cat","size":"10000x100"}`, http.StatusBadRequest},
		{"size-side-max-plus-one", `{"model":"img","prompt":"a cat","size":"8193x1"}`, http.StatusBadRequest},
		{"size-zero", `{"model":"img","prompt":"a cat","size":"0x100"}`, http.StatusBadRequest},
		{"size-missing-half", `{"model":"img","prompt":"a cat","size":"100x"}`, http.StatusBadRequest},
		{"size-multiplication-sign", `{"model":"img","prompt":"a cat","size":"100×100"}`, http.StatusBadRequest},
		{"size-exponent", `{"model":"img","prompt":"a cat","size":"8e3x100"}`, http.StatusBadRequest},
		{"size-auto", `{"model":"img","prompt":"a cat","size":"auto"}`, http.StatusOK},
		{"size-max", `{"model":"img","prompt":"a cat","size":"8192x8192"}`, http.StatusOK},
		{"quality-invalid", `{"model":"img","prompt":"a cat","quality":"ultra"}`, http.StatusBadRequest},
		{"quality-case", `{"model":"img","prompt":"a cat","quality":"HD"}`, http.StatusBadRequest},
		{"quality-valid", `{"model":"img","prompt":"a cat","quality":"low"}`, http.StatusOK},
		{"partial-images-too-large", `{"model":"img","prompt":"a cat","partial_images":9}`, http.StatusBadRequest},
		{"parameters-n-too-large", `{"model":"img","prompt":"a cat","parameters":{"n":129}}`, http.StatusBadRequest},
		{"parameters-n-zero-provider", `{"model":"img","prompt":"a cat","parameters":{"n":0,"useProviderParameters":true}}`, http.StatusBadRequest},
		{"parameters-n-valid", `{"model":"img","prompt":"a cat","parameters":{"n":2}}`, http.StatusOK},
		{"invalid-json", `not-json`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doMultimodal(t, handler, multimodalRequest{
				path: "/v1/images/generations", body: []byte(tc.body), contentType: "application/json",
			}, true)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, tc.want, resp.Body.String())
			}
			if tc.want == http.StatusBadRequest && !strings.Contains(resp.Body.String(), "invalid_request") {
				t.Fatalf("400 body missing invalid_request: %s", resp.Body.String())
			}
		})
	}
}

// TestMultimodal_ImageMultipartValidation 覆盖 multipart 与 JSON 同强度的上界。
func TestMultimodal_ImageMultipartValidation(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})

	base := map[string]string{"model": "img-model", "prompt": "a cat"}
	cases := []struct {
		name    string
		fields  map[string]string
		ordered [][2]string
		files   map[string][]byte
		want    int
	}{
		{"valid", base, nil, map[string][]byte{"image": []byte("fake-image")}, http.StatusOK},
		{"missing-model", map[string]string{"prompt": "a cat"}, nil, nil, http.StatusBadRequest},
		{"missing-prompt", map[string]string{"model": "img-model"}, nil, nil, http.StatusBadRequest},
		{"n-zero", map[string]string{"model": "img-model", "prompt": "a cat", "n": "0"}, nil, nil, http.StatusBadRequest},
		{"n-too-large", map[string]string{"model": "img-model", "prompt": "a cat", "n": "129"}, nil, nil, http.StatusBadRequest},
		{"n-not-integer", map[string]string{"model": "img-model", "prompt": "a cat", "n": "1.5"}, nil, nil, http.StatusBadRequest},
		{"quality-invalid", map[string]string{"model": "img-model", "prompt": "a cat", "quality": "ultra"}, nil, nil, http.StatusBadRequest},
		{"size-invalid", map[string]string{"model": "img-model", "prompt": "a cat", "size": "10000x100"}, nil, nil, http.StatusBadRequest},
		{"prompt-too-long", map[string]string{"model": "img-model", "prompt": strings.Repeat("a", MaxImagePromptBytes+1)}, nil, nil, http.StatusBadRequest},
		{"parameters-n-too-large", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": `{"n":129}`}, nil, nil, http.StatusBadRequest},
		{"parameters-n-1000", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": `{"n":1000}`}, nil, nil, http.StatusBadRequest},
		{"parameters-n-zero-provider", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": `{"n":0,"useProviderParameters":true}`}, nil, nil, http.StatusBadRequest},
		{"parameters-non-json", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": "not-json"}, nil, nil, http.StatusBadRequest},
		{"parameters-empty", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": ""}, nil, nil, http.StatusBadRequest},
		{"parameters-file-non-json", base, nil, map[string][]byte{"parameters": []byte("not-json")}, http.StatusBadRequest},
		{"parameters-valid", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": `{"n":2}`}, nil, map[string][]byte{"image": []byte("fake-image")}, http.StatusOK},
		// 重复计费标量：本层 last-wins、标准库 first-wins，必须 fail-closed（F9）。
		{"duplicate-n-out-of-bounds-first", nil, [][2]string{{"model", "img-model"}, {"prompt", "a cat"}, {"n", "1000"}, {"n", "2"}}, nil, http.StatusBadRequest},
		{"duplicate-n-valid-both", nil, [][2]string{{"model", "img-model"}, {"prompt", "a cat"}, {"n", "2"}, {"n", "2"}}, nil, http.StatusBadRequest},
		{"duplicate-parameters-out-of-bounds-first", nil, [][2]string{{"model", "img-model"}, {"prompt", "a cat"}, {"parameters", `{"n":1000}`}, {"parameters", `{"n":2}`}}, nil, http.StatusBadRequest},
		{"duplicate-model", nil, [][2]string{{"model", "img-model"}, {"model", "other-model"}, {"prompt", "a cat"}}, nil, http.StatusBadRequest},
		{"duplicate-prompt", nil, [][2]string{{"model", "img-model"}, {"prompt", "a cat"}, {"prompt", "a dog"}}, nil, http.StatusBadRequest},
		{"duplicate-size", nil, [][2]string{{"model", "img-model"}, {"prompt", "a cat"}, {"size", "1024x1024"}, {"size", "512x512"}}, nil, http.StatusBadRequest},
		{"duplicate-quality", nil, [][2]string{{"model", "img-model"}, {"prompt", "a cat"}, {"quality", "hd"}, {"quality", "low"}}, nil, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var contentType string
			var body []byte
			if tc.ordered != nil {
				contentType, body = buildMultipartOrdered(t, tc.ordered, tc.files)
			} else {
				contentType, body = buildMultipartBody(t, tc.fields, tc.files)
			}
			resp := doMultimodal(t, handler, multimodalRequest{
				path: "/v1/images/edits", body: body, contentType: contentType,
			}, true)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

// TestMultimodal_AudioMultipartValidation 覆盖音频入口：缺文件/缺 model 400，
// 整体请求体超过 32 MiB 时由 MaxBytesReader 映射 413。
func TestMultimodal_AudioMultipartValidation(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})

	t.Run("valid", func(t *testing.T) {
		resp := doMultimodal(t, handler, validAudioMultipart(t, "/v1/audio/transcriptions", "whisper-1"), true)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
		}
	})

	t.Run("empty-file", func(t *testing.T) {
		contentType, body := buildMultipartBody(t, map[string]string{"model": "whisper-1"}, map[string][]byte{"file": {}})
		resp := doMultimodal(t, handler, multimodalRequest{path: "/v1/audio/transcriptions", body: body, contentType: contentType}, true)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
		}
	})

	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
		t.Run("missing-file"+path, func(t *testing.T) {
			contentType, body := buildMultipartBody(t, map[string]string{"model": "whisper-1"}, nil)
			resp := doMultimodal(t, handler, multimodalRequest{path: path, body: body, contentType: contentType}, true)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
		})
		t.Run("missing-model"+path, func(t *testing.T) {
			contentType, body := buildMultipartBody(t, nil, map[string][]byte{"file": []byte("audio")})
			resp := doMultimodal(t, handler, multimodalRequest{path: path, body: body, contentType: contentType}, true)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
		})
		t.Run("duplicate-model"+path, func(t *testing.T) {
			contentType, body := buildMultipartOrdered(t,
				[][2]string{{"model", "whisper-1"}, {"model", "other"}, {"file", ""}},
				map[string][]byte{"file": []byte("audio")})
			resp := doMultimodal(t, handler, multimodalRequest{path: path, body: body, contentType: contentType}, true)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
		})
	}

	t.Run("body-too-large", func(t *testing.T) {
		// 32 MiB + 1 的惰性 reader：MaxBytesReader 应在读超限的第一时间中止，
		// 不需要先在测试里分配整块切片。
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", io.LimitReader(repeatByteReader{}, maxRelayBodyBytes+1))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
		req.Header.Set("Authorization", "Bearer "+relayTestToken)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", resp.Code, resp.Body.String())
		}
	})
}

// repeatByteReader 无限产出 'a'，配合 io.LimitReader 构造超大请求体。
type repeatByteReader struct{}

func (repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestMultimodal_DefaultDispatcherWired 固化 Task 4 的装配要求：生产
// NewRelayHandler 不再返回 501，而是注入真实分派器。测试的 fake 渠道库没有
// 候选，因此合法请求在派发阶段得到 503（no available channel）——这证明
// 请求确实进入了真实执行路径，而不是停在 T1 的 501 桩。
func TestMultimodal_DefaultDispatcherWired(t *testing.T) {
	handler := newMultimodalTestHandler(t, nil)
	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/images/generations", body: validImageJSON(), contentType: "application/json",
	}, true)
	if resp.Code == http.StatusNotImplemented {
		t.Fatalf("production wiring still returns 501: %s", resp.Body.String())
	}
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no candidate channel); body=%s", resp.Code, resp.Body.String())
	}
}

// TestMultimodal_RequestContextAndBodyRestored 确认 Dispatcher 拿到的是完整
// 请求体与已组装的 RelayContext（multipart 校验不能吞掉 body）。
func TestMultimodal_RequestContextAndBodyRestored(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)

	contentType, body := buildMultipartBody(t,
		map[string]string{"model": "whisper-1"}, map[string][]byte{"file": []byte("audio-payload")})
	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/audio/transcriptions", body: body, contentType: contentType,
	}, true)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if !bytes.Equal(dispatcher.body, body) {
		t.Fatalf("dispatcher body was not restored (got %d bytes, want %d)", len(dispatcher.body), len(body))
	}
	if dispatcher.rc == nil || dispatcher.rc.OriginModel != "whisper-1" {
		t.Fatalf("OriginModel = %+v, want whisper-1", dispatcher.rc)
	}
	if dispatcher.rc.RequestID == "" || dispatcher.rc.TokenID != int(relayTestTokenID) {
		t.Fatalf("RelayContext identity not populated: %+v", dispatcher.rc)
	}
}

// ============================================================================
// M4 Task 4 补充：入口一致性（F2/F4/F5）与原生渠道 chat usage/finish 接线
// ============================================================================

// newMultimodalEnv 允许定制令牌模型白名单与模型停用状态存储。
func newMultimodalEnv(t *testing.T, dispatcher MultimodalDispatcher, modelLimits []string, modelStatus RelayModelStatusStore) http.Handler {
	t.Helper()
	tokenStore := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID:             relayTestTokenID,
		UserID:         relayTestUserID,
		Status:         "enabled",
		Group:          "default",
		UnlimitedQuota: true,
	})
	if len(modelLimits) > 0 {
		tokenStore.byHash[auth.HashTokenKey(relayTestToken)].ModelLimits = modelLimits
	}
	access := &fakeRelayAccess{role: "user", status: "enabled"}
	channels := &fakeRelayChannelStore{models: []string{"gpt-test"}}
	h := NewRelayHandler(RelayHandlerOptions{
		Channels:    channels,
		Tokens:      tokenStore,
		Access:      access,
		Sealer:      NewSecretSealer(relaySealerMaster),
		ModelStatus: modelStatus,
		Multimodal:  dispatcher,
	})
	return h.Handler()
}

// TestMultimodal_TokenModelWhitelistRejects 覆盖 T1 复审 F2：多模态端点不得
// 绕过令牌模型白名单（与 chat 同一 403 语义）。
func TestMultimodal_TokenModelWhitelistRejects(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalEnv(t, dispatcher, []string{"other-model"}, nil)
	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/images/generations", body: validImageJSON(), contentType: "application/json",
	}, true)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.Code, resp.Body.String())
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// TestMultimodal_DisabledModelReturns404 覆盖 F2：status=2 的模型必须在
// 派发与预扣之前 404（R-84 同口径）。
func TestMultimodal_DisabledModelReturns404(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	status := &fakeRelayModelStatus{status: map[string]int16{"img-model": modelPricingStatusDisabled}}
	handler := newMultimodalEnv(t, dispatcher, nil, status)
	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/images/generations", body: validImageJSON(), contentType: "application/json",
	}, true)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.Code, resp.Body.String())
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// TestMultimodal_EditsRequiresInputImage 覆盖 F4：multipart 编辑必须包含
// 非空 image 文件。
func TestMultimodal_EditsRequiresInputImage(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)
	contentType, body := buildMultipartBody(t, map[string]string{"model": "img-model", "prompt": "a cat"}, nil)
	resp := doMultimodal(t, handler, multimodalRequest{path: "/v1/images/edits", body: body, contentType: contentType}, true)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// TestMultimodal_AudioMIMEMagicMismatch 覆盖 F5：扩展名/声明 MIME/魔数三者
// 必须一致（security-04 §3.2）。
func TestMultimodal_AudioMIMEMagicMismatch(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "whisper-1")
	part, err := writer.CreateFormFile("file", "voice.mp3")
	if err != nil {
		t.Fatal(err)
	}
	// .mp3 扩展名 + WAV 魔数：伪造类型必须 400。
	_, _ = part.Write([]byte("RIFF\x00\x00\x00\x00WAVEfmt "))
	_ = writer.Close()

	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/audio/transcriptions", body: buf.Bytes(), contentType: writer.FormDataContentType(),
	}, true)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// staticJSONAdapter 让 chat 非流式路径拿到可控的上游原始响应体。
type staticJSONAdapter struct {
	kind contract.ChannelType
	url  string
	body []byte
}

func (a *staticJSONAdapter) Type() contract.ChannelType  { return a.kind }
func (a *staticJSONAdapter) Caps() contract.Capabilities { return contract.Capabilities{} }
func (a *staticJSONAdapter) BuildRequest(*contract.RelayContext, *contract.Request) (*http.Request, error) {
	return http.NewRequest(http.MethodPost, a.url, nil)
}
func (a *staticJSONAdapter) ParseResponse(*contract.RelayContext, *http.Response) (*contract.Response, contract.Usage, error) {
	return &contract.Response{Raw: a.body}, contract.Usage{}, nil
}
func (a *staticJSONAdapter) ClassifyError(int, []byte) contract.ErrorClass {
	return contract.ErrUnknown
}

// TestRelayNativeChatUsageNonStream 验证原生渠道（Anthropic/Gemini）的
// 非流式 chat 走 T3 的 ExtractUsageMultimodal，而不是 OpenAI 形状解析。
func TestRelayNativeChatUsageNonStream(t *testing.T) {
	cases := []struct {
		name     string
		kind     contract.ChannelType
		body     string
		prompt   int64
		complete int64
		cache    int64
		reason   int64
	}{
		{
			name: "anthropic", kind: contract.ChannelTypeAnthropic,
			body:   `{"id":"msg","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3}}`,
			prompt: 11, complete: 7, cache: 3,
		},
		{
			name: "gemini", kind: contract.ChannelTypeGemini,
			body:   `{"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4,"thoughtsTokenCount":2,"cachedContentTokenCount":1}}`,
			prompt: 9, complete: 6, cache: 1, reason: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			env := newRelayTestEnv(t, upstream, nil)
			env.channels.channels[relayTestChannelID].Type = string(tc.kind)
			env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) {
				return &staticJSONAdapter{kind: tc.kind, url: upstream.URL, body: []byte(tc.body)}, nil
			}
			resp := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
				`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", resp.Code, resp.Body.String())
			}
			fact := env.billing.lastFact()
			if fact.Usage.Prompt != tc.prompt || fact.Usage.Completion != tc.complete ||
				fact.Usage.CacheRead != tc.cache || fact.Usage.Reasoning != tc.reason {
				t.Fatalf("native usage = %+v, want prompt=%d completion=%d cache=%d reasoning=%d",
					fact.Usage, tc.prompt, tc.complete, tc.cache, tc.reason)
			}
			if fact.Usage.PromptIncludesCache {
				t.Fatal("native channels must not report PromptIncludesCache=true")
			}
			if env.billing.terminateCount() != 1 || env.billing.refundCount() != 0 {
				t.Fatalf("billing: terminate=%d refund=%d, want 1/0", env.billing.terminateCount(), env.billing.refundCount())
			}
		})
	}
}

// TestRelayNativeStreamFinishReason 验证 T2 交接：原生 EventFinish.Raw 里的
// Anthropic stop_reason / Gemini finishReason 必须被归一为 OpenAI 的
// finish_reason 并下发（而不是静默丢失）。
func TestRelayNativeStreamFinishReason(t *testing.T) {
	cases := []struct {
		name string
		kind contract.ChannelType
		raw  string
	}{
		{"anthropic-max-tokens", contract.ChannelTypeAnthropic, `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":2}}`},
		{"gemini-max-tokens", contract.ChannelTypeGemini, `data: {"candidates":[{"finishReason":"MAX_TOKENS"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageEvent := &contract.Usage{Prompt: 5, Completion: 2, CacheRead: 3, Total: 10, Source: contract.UsageUpstreamExact}
			adapter := &scriptedStreamAdapter{events: []contract.StreamEvent{
				{Kind: contract.EventUsage, Usage: usageEvent},
				{Kind: contract.EventFinish, Raw: []byte(tc.raw)},
			}}
			env, upstream := newScriptedStreamEnv(t, adapter)
			adapter.url = upstream.URL
			env.channels.channels[relayTestChannelID].Type = string(tc.kind)
			resp := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
				`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", resp.Code, resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), `"finish_reason":"length"`) {
				t.Fatalf("finish_reason not surfaced; body=%s", resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), "data: [DONE]") {
				t.Fatalf("missing [DONE]; body=%s", resp.Body.String())
			}
			fact := env.billing.lastFact()
			if fact.Usage.Prompt != 5 || fact.Usage.Completion != 2 {
				t.Fatalf("stream usage = %+v, want prompt=5 completion=2", fact.Usage)
			}
			// H2：客户端可见的 include_usage 末块必须是 OpenAI 语义——
			// 原生 input(5) 不含缓存(3)，故 prompt_tokens=8，且
			// cached_tokens(3) 不得大于 prompt_tokens。
			body := resp.Body.String()
			if !strings.Contains(body, `"prompt_tokens":8`) {
				t.Fatalf("usage chunk did not add cache to prompt_tokens; body=%s", body)
			}
			if !strings.Contains(body, `"cached_tokens":3`) {
				t.Fatalf("usage chunk missing cached_tokens; body=%s", body)
			}
			if !strings.Contains(body, `"total_tokens":10`) {
				t.Fatalf("usage chunk total_tokens should be >= prompt+completion; body=%s", body)
			}
		})
	}
}

// TestMultimodal_SpeechBounds（F6）：response_format 枚举、max_new_tokens、
// metadata 字节上界必须在入口 400。
func TestMultimodal_SpeechBounds(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})
	cases := []string{
		`{"model":"tts","input":"hi","response_format":"ogg"}`,
		`{"model":"tts","input":"hi","max_new_tokens":5000}`,
		`{"model":"tts","input":"hi","max_new_tokens":1.5}`,
		`{"model":"tts","input":"hi","metadata":{"x":"` + strings.Repeat("a", MaxAudioMetadataBytes) + `"}}`,
	}
	for _, body := range cases {
		resp := doMultimodal(t, handler, multimodalRequest{path: "/v1/audio/speech", body: []byte(body), contentType: "application/json"}, true)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, resp.Code)
		}
	}
}

// TestMultimodal_AudioFormatCannotMeasure（F12）：ogg/aiff/aac 无法从文件头
// 测得时长，转写/翻译必须显式 400，不得静默按 0 秒计费。
func TestMultimodal_AudioFormatCannotMeasure(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "whisper-1")
	part, err := writer.CreateFormFile("file", "voice.ogg")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("OggS\x00\x02\x00\x00"))
	_ = writer.Close()
	resp := doMultimodal(t, handler, multimodalRequest{path: "/v1/audio/transcriptions", body: buf.Bytes(), contentType: writer.FormDataContentType()}, true)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if dispatcher.calls != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
	}
}

// TestMultimodal_GeminiEmbeddingRoutes（F8/R-11）：三条 Gemini 语义嵌入
// 路径归入 embeddings；其它动作后缀回到标准 404。
func TestMultimodal_GeminiEmbeddingRoutes(t *testing.T) {
	dispatcher := &captureMultimodalDispatcher{}
	handler := newMultimodalTestHandler(t, dispatcher)
	cases := []struct {
		path     string
		model    string
		body     string
		wantCode int
	}{
		{"/v1/engines/text-embedding-004/embeddings", "text-embedding-004", `{"input":"hi"}`, http.StatusOK},
		{"/v1/models/text-embedding-004:embedContent", "text-embedding-004", `{"content":{"parts":[{"text":"hi"}]}}`, http.StatusOK},
		{"/v1beta/models/text-embedding-004:batchEmbedContents", "text-embedding-004", `{"requests":[]}`, http.StatusOK},
		{"/v1/models/text-embedding-004:countTokens", "", `{}`, http.StatusNotFound},
		{"/v1beta/models/gemini-pro:generateContent", "", `{}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		resp := doMultimodal(t, handler, multimodalRequest{path: tc.path, body: []byte(tc.body), contentType: "application/json"}, true)
		if resp.Code != tc.wantCode {
			t.Fatalf("%s: status = %d, want %d; body=%s", tc.path, resp.Code, tc.wantCode, resp.Body.String())
		}
		if tc.wantCode == http.StatusOK {
			dispatcher.mu.Lock()
			model, endpoint := dispatcher.rc.OriginModel, dispatcher.endpoint
			dispatcher.mu.Unlock()
			if model != tc.model || endpoint != contract.EndpointEmbeddings {
				t.Fatalf("%s: model=%q endpoint=%q, want %q/%q", tc.path, model, endpoint, tc.model, contract.EndpointEmbeddings)
			}
		}
	}
}

// TestMultimodal_GeminiEmbeddingLimits（N5）：requests[] 批量与单段文本上界。
func TestMultimodal_GeminiEmbeddingLimits(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})
	requests := make([]map[string]any, multimodal.MaxEmbeddingBatch+1)
	for i := range requests {
		requests[i] = map[string]any{"content": map[string]any{"parts": []map[string]string{{"text": "x"}}}}
	}
	batchBody, _ := json.Marshal(map[string]any{"requests": requests})
	largeBody, _ := json.Marshal(map[string]any{
		"content": map[string]any{"parts": []map[string]string{{"text": strings.Repeat("a", multimodal.MaxEmbeddingInputBytes+1)}}},
	})
	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"batch-too-large", batchBody, http.StatusBadRequest},
		{"text-too-large", largeBody, http.StatusBadRequest},
		{"valid", []byte(`{"content":{"parts":[{"text":"hi"}]}}`), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doMultimodal(t, handler, multimodalRequest{
				path: "/v1beta/models/text-embedding-004:embedContent", body: tc.body, contentType: "application/json",
			}, true)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

// TestMultimodal_GeminiEnginesEntryBounds（N3 残余）：engines 路由必须与
// 主嵌入入口同强度校验 input（非空、批量、单条字节）。
func TestMultimodal_GeminiEnginesEntryBounds(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})
	batch := make([]string, multimodal.MaxEmbeddingBatch+1)
	for i := range batch {
		batch[i] = "x"
	}
	batchBody, _ := json.Marshal(map[string]any{"input": batch})
	oversized, _ := json.Marshal(map[string]any{"input": strings.Repeat("a", multimodal.MaxEmbeddingInputBytes+1)})
	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"empty-input", []byte(`{"input":""}`), http.StatusBadRequest},
		{"missing-input", []byte(`{"model":"m"}`), http.StatusBadRequest},
		{"batch-too-large", batchBody, http.StatusBadRequest},
		{"item-too-large", oversized, http.StatusBadRequest},
		{"valid", []byte(`{"input":["a","b"],"dimensions":256}`), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doMultimodal(t, handler, multimodalRequest{
				path: "/v1/engines/text-embedding-004/embeddings", body: tc.body, contentType: "application/json",
			}, true)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}
