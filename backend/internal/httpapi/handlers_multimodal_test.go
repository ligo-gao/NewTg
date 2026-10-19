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
		part, err := writer.CreateFormFile(name, "upload.bin")
		if err != nil {
			t.Fatalf("create file %s: %v", name, err)
		}
		if _, err := part.Write(value); err != nil {
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
		map[string]string{"model": "img-model", "prompt": "a cat"}, nil)
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
		{"realtime", multimodalRequest{method: http.MethodGet, path: "/v1/realtime"}, contract.EndpointRealtime},
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

// TestMultimodal_RemovedRoutesAre404 固化裁决 H：变体与 legacy /v1/edits 不注册。
func TestMultimodal_RemovedRoutesAre404(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})
	for _, path := range []string{"/v1/images/variations", "/v1/edits"} {
		for _, authenticated := range []bool{false, true} {
			resp := doMultimodal(t, handler, multimodalRequest{path: path, body: validImageJSON(), contentType: "application/json"}, authenticated)
			if resp.Code != http.StatusNotFound {
				t.Fatalf("%s (auth=%v): status = %d, want 404", path, authenticated, resp.Code)
			}
		}
	}
}

// TestMultimodal_RealtimeIsGETOnly 锁定 relay-07 G-06 的 GET-only 裁决：
// WebSocket 握手是 GET，POST 必须由 ServeMux 以 405 拒绝。
func TestMultimodal_RealtimeIsGETOnly(t *testing.T) {
	handler := newMultimodalTestHandler(t, &captureMultimodalDispatcher{})
	resp := doMultimodal(t, handler, multimodalRequest{path: "/v1/realtime"}, true)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/realtime status = %d, want 405; body=%s", resp.Code, resp.Body.String())
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
		{"parameters-valid", map[string]string{"model": "img-model", "prompt": "a cat", "parameters": `{"n":2}`}, nil, nil, http.StatusOK},
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

// TestMultimodal_NoDispatcherReturns501 固化 T1 测试态：未注入分派器时，
// 合法请求在校验与上下文组装之后返回 501（Task 4 装配后必须替换）。
func TestMultimodal_NoDispatcherReturns501(t *testing.T) {
	handler := newMultimodalTestHandler(t, nil)
	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/images/generations", body: validImageJSON(), contentType: "application/json",
	}, true)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "not_implemented") {
		t.Fatalf("501 body missing not_implemented: %s", resp.Body.String())
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
