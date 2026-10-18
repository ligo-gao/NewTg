package httpapi

// 本文件是 Task 8 phase 8B（/v1 中继端点 + 中继鉴权 + 流式接线）的行为测试。
//
// 纪律（R-P10 / phase 8A 同口径）：不连真 PG/Redis；令牌/用户/渠道全部用内存 fake，
// 上游用 httptest + 真实 adapters.OpenAI（经 adapters.Dispatch 分派），
// 从而覆盖「鉴权 → 归一化 → 路由 → 执行 → 流式桥接」的完整装配面。
//
// 安全断言（GC-6）：任何错误响应都不得出现 base_url、上游凭据或适配器原始错误文本。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/store"
)

const (
	relayTestChannelID int64 = 1
	relayTestTokenID   int64 = 7
	relayTestUserID    int64 = 1
	// upstreamCredential 是 fake 渠道里封装的凭据明文；所有错误/日志断言都
	// 检查它不出现在响应体中（GC-6）。
	upstreamCredential = "upstream-secret-key"
	// relaySealerMaster 与 phase 8A 的测试主密钥同口径（任意非空串即可）。
	relaySealerMaster = "relay-endpoint-test-master-key-0123456789"
)

var relayTestToken = "nkt-" + strings.Repeat("a", 48)

// ---------- 内存 fake：令牌 / 用户权限 / 渠道 ----------

type fakeRelayTokenStore struct {
	mu         sync.Mutex
	byHash     map[string]*auth.TokenMeta
	resolveErr error
	calls      int
}

func newFakeRelayTokenStore(token string, meta *auth.TokenMeta) *fakeRelayTokenStore {
	cp := *meta
	return &fakeRelayTokenStore{byHash: map[string]*auth.TokenMeta{auth.HashTokenKey(token): &cp}}
}

func (f *fakeRelayTokenStore) Resolve(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	meta, ok := f.byHash[keyHash]
	if !ok {
		return nil, store.ErrTokenNotFound
	}
	cp := *meta
	return &cp, nil
}

type fakeRelayAccess struct {
	mu          sync.Mutex
	role        string
	permissions []string
	status      string
	err         error
	lastUserID  int64
	calls       int
}

func (f *fakeRelayAccess) UserAccess(_ context.Context, userID int64) (string, []string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastUserID = userID
	if f.err != nil {
		return "", nil, "", f.err
	}
	return f.role, f.permissions, f.status, nil
}

type fakeRelayChannelStore struct {
	mu sync.Mutex

	candidates    []contract.ChannelCandidate
	candidatesErr error

	channels map[int64]*store.Channel
	getErr   error

	models    []string
	modelsErr error

	lastModel  string
	lastGroup  string
	getCalls   int
	candCalls  int
	modelCalls int
}

func (f *fakeRelayChannelStore) Candidates(_ context.Context, model, group string) ([]contract.ChannelCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candCalls++
	f.lastModel, f.lastGroup = model, group
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
	return append([]contract.ChannelCandidate(nil), f.candidates...), nil
}

func (f *fakeRelayChannelStore) Get(_ context.Context, id int64) (*store.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	ch, ok := f.channels[id]
	if !ok {
		return nil, store.ErrChannelNotFound
	}
	cp := *ch
	return &cp, nil
}

func (f *fakeRelayChannelStore) EnabledModels(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelCalls++
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return append([]string(nil), f.models...), nil
}

// ---------- 记录计费会话 ----------

// recordingBilling 包装 billingseam.Stub 并记录 Terminate 的事实，
// 用于断言「客户端断开只终结一次」与 partial UsageFact 的字段。
type recordingBilling struct {
	mu         sync.Mutex
	stub       *billingseam.Stub
	terminated []contract.UsageFact
}

func newRecordingBilling() *recordingBilling {
	return &recordingBilling{stub: billingseam.NewStub()}
}

func (b *recordingBilling) PreConsume(est contract.QuotaEstimate) error {
	return b.stub.PreConsume(est)
}
func (b *recordingBilling) Reserve(delta int64) error { return b.stub.Reserve(delta) }
func (b *recordingBilling) ChargeIncrement(delta int64, fact contract.UsageFact) error {
	return b.stub.ChargeIncrement(delta, fact)
}
func (b *recordingBilling) Terminate(fact contract.UsageFact) error {
	b.mu.Lock()
	b.terminated = append(b.terminated, fact)
	b.mu.Unlock()
	return b.stub.Terminate(fact)
}
func (b *recordingBilling) Refund() error                 { return b.stub.Refund() }
func (b *recordingBilling) Reserved() int64               { return b.stub.Reserved() }
func (b *recordingBilling) NeedsRefund() bool             { return b.stub.NeedsRefund() }
func (b *recordingBilling) RecoverAbandoned(string) error { return b.stub.RecoverAbandoned("") }

func (b *recordingBilling) terminateCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.terminated)
}

func (b *recordingBilling) lastFact() contract.UsageFact {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.terminated) == 0 {
		return contract.UsageFact{}
	}
	return b.terminated[len(b.terminated)-1]
}

// ---------- 装配 helper ----------

type relayTestEnv struct {
	handler  *RelayHandler
	channels *fakeRelayChannelStore
	tokens   *fakeRelayTokenStore
	access   *fakeRelayAccess
	billing  *recordingBilling
	sealer   *SecretSealer
	accessMu sync.Mutex
}

func sealedTestCredentials(t *testing.T, sealer *SecretSealer) []byte {
	t.Helper()
	raw, err := json.Marshal(ChannelCredentials{APIKey: upstreamCredential})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	sealed, err := sealer.Seal(raw)
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	return sealed
}

// newRelayTestEnv 构造完整依赖：一个启用的 openai 渠道指向 upstream。
func newRelayTestEnv(t *testing.T, upstream *httptest.Server, channels *fakeRelayChannelStore) *relayTestEnv {
	t.Helper()
	sealer := NewSecretSealer(relaySealerMaster)
	if channels == nil {
		channels = &fakeRelayChannelStore{}
	}
	// 仅当调用方没有显式给出候选（nil）时才填默认候选；显式空切片表示
	// 「无可用渠道」场景，必须保留为空（relay-01 §2 阶段 3 的 503 分支）。
	if channels.candidates == nil && channels.candidatesErr == nil {
		channels.candidates = []contract.ChannelCandidate{{
			ChannelID: int(relayTestChannelID),
			Priority:  10,
			Weight:    1,
		}}
	}
	if channels.channels == nil {
		baseURL := upstream.URL
		channels.channels = map[int64]*store.Channel{
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
		}
	}
	if channels.models == nil {
		channels.models = []string{"gpt-test"}
	}

	tokenStore := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID:             relayTestTokenID,
		UserID:         relayTestUserID,
		Status:         "enabled",
		UnlimitedQuota: true,
	})
	access := &fakeRelayAccess{role: "admin", status: "enabled"}
	billing := newRecordingBilling()

	h := NewRelayHandler(RelayHandlerOptions{
		Channels: channels,
		Tokens:   tokenStore,
		Access:   access,
		Sealer:   sealer,
		Client:   upstream.Client(),
		Billing:  func() contract.BillingSession { return billing },
	})
	return &relayTestEnv{handler: h, channels: channels, tokens: tokenStore, access: access, billing: billing, sealer: sealer}
}

// do 发送一个经 RelayHandler mux 的请求。
func (e *relayTestEnv) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.handler.Handler().ServeHTTP(w, req)
	return w
}

func (e *relayTestEnv) doAuthed(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, method, path, body, map[string]string{"Authorization": "Bearer " + relayTestToken})
}

// ---------- (a) 非流式 ----------

func TestRelayChatCompletionsNonStream(t *testing.T) {
	const upstreamJSON = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

	var mu sync.Mutex
	var gotAuth, gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamJSON)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != upstreamJSON {
		t.Fatalf("body = %q, want upstream body verbatim", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID header is empty")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer "+upstreamCredential {
		t.Fatalf("upstream Authorization = %q, want channel credential (GC-8 decrypt path)", gotAuth)
	}
	if gotBody["model"] != "gpt-test" {
		t.Fatalf("upstream model = %v, want gpt-test", gotBody["model"])
	}
	if stream, _ := gotBody["stream"].(bool); stream {
		t.Fatal("non-stream request must send stream=false upstream")
	}

	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want exactly 1", got)
	}
	if fact := env.billing.lastFact(); fact.Stream {
		t.Fatal("non-stream UsageFact.Stream = true, want false")
	}
}

// ---------- (b) 流式 SSE ----------

func TestRelayChatCompletionsStream(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	mu.Lock()
	stream, _ := upstreamBody["stream"].(bool)
	options, _ := upstreamBody["stream_options"].(map[string]any)
	includeUsage, _ := options["include_usage"].(bool)
	mu.Unlock()
	if !stream {
		t.Fatal("upstream body stream = false, want true")
	}
	if !includeUsage {
		t.Fatal("upstream body stream_options.include_usage = false; R-P14 要求注入 true")
	}

	out := w.Body.String()
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Fatalf("streamed token content missing; body=%s", out)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] count = %d, want exactly 1; body=%s", got, out)
	}
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Fatalf("missing chat.completion.chunk frames; body=%s", out)
	}
	if got := strings.Count(out, `"usage":`); got != 1 {
		t.Fatalf("usage chunk count = %d, want exactly 1; body=%s", got, out)
	}
	if !strings.Contains(out, `"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}`) {
		t.Fatalf("final usage chunk missing accumulated usage; body=%s", out)
	}
	// finish_reason 必须到达客户端（stop/length/tool_calls），且只出现一次：
	// delta chunk 的 finish_reason 是 null，不能把 EventFinish 只记成布尔。
	if got := strings.Count(out, `"finish_reason":"stop"`); got != 1 {
		t.Fatalf("finish_reason stop count = %d, want exactly 1; body=%s", got, out)
	}

	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want exactly 1", got)
	}
	fact := env.billing.lastFact()
	if !fact.Stream {
		t.Fatal("stream UsageFact.Stream = false, want true")
	}
	if fact.Usage.Total != 6 {
		t.Fatalf("UsageFact.Usage.Total = %d, want 6", fact.Usage.Total)
	}
	if fact.FirstTokenMS == nil {
		t.Fatal("stream UsageFact.FirstTokenMS = nil, want non-nil")
	}
}

// ---------- (c) 缺失/非法令牌 → 401 OpenAI 信封，无 URL 泄漏 ----------

func TestRelayAuthErrorsUseOpenAIEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called on auth failure")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`

	t.Run("missing token", func(t *testing.T) {
		w := env.do(t, http.MethodPost, "/v1/chat/completions", body, nil)
		assertRelayAuthError(t, w, http.StatusUnauthorized, upstream.URL)
	})

	t.Run("invalid token", func(t *testing.T) {
		w := env.do(t, http.MethodPost, "/v1/chat/completions", body,
			map[string]string{"Authorization": "Bearer nkt-" + strings.Repeat("z", 48)})
		assertRelayAuthError(t, w, http.StatusUnauthorized, upstream.URL)
	})

	t.Run("disabled user", func(t *testing.T) {
		env.access.mu.Lock()
		env.access.status = "disabled"
		env.access.mu.Unlock()
		defer func() {
			env.access.mu.Lock()
			env.access.status = "enabled"
			env.access.mu.Unlock()
		}()
		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
		assertRelayAuthError(t, w, http.StatusForbidden, upstream.URL)
	})
}

func assertRelayAuthError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, upstreamURL string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, wantStatus, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json (RFC7807 is forbidden on /v1)", ct)
	}
	if requestID := w.Header().Get("X-Request-ID"); requestID == "" {
		t.Fatal("X-Request-ID header is empty")
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"new_api_error"`) {
		t.Fatalf("body = %s, want new_api_error envelope", body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("body = %s, want error object", body)
	}
	if strings.Contains(body, upstreamURL) || strings.Contains(body, upstreamCredential) {
		t.Fatalf("error body leaks upstream URL/credential: %s", body)
	}
	if strings.Contains(body, "newtg.dev/problems") {
		t.Fatalf("error body uses RFC7807 on /v1: %s", body)
	}
}

// ---------- (d) 无候选渠道 → 502/503 信封 + requestId ----------

func TestRelayNoCandidatesReturnsEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when there are no candidates")
	}))
	defer upstream.Close()

	channels := &fakeRelayChannelStore{candidates: []contract.ChannelCandidate{}}
	env := newRelayTestEnv(t, upstream, channels)
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (relay-01 §2 阶段 3 的 get_channel_failed); body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"new_api_error"`) {
		t.Fatalf("body = %s, want new_api_error envelope", body)
	}
	if !strings.Contains(body, `"code":"model_not_found"`) {
		t.Fatalf("body = %s, want code model_not_found", body)
	}
	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Fatal("X-Request-ID header is empty")
	}
	if !strings.Contains(body, requestID) {
		t.Fatalf("body = %s, want requestId %q echoed (relay-01 §3.6)", body, requestID)
	}
	if strings.Contains(body, upstream.URL) || strings.Contains(body, upstreamCredential) {
		t.Fatalf("error body leaks upstream URL/credential: %s", body)
	}
	if env.billing.terminateCount() != 0 {
		t.Fatal("no billing session should be opened when there are no candidates")
	}
}

// ---------- (e) 模型列表聚合 ----------

func TestRelayModelsList(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("model list must be served from the channel repo, not upstream")
	}))
	defer upstream.Close()

	channels := &fakeRelayChannelStore{models: []string{"alpha", "beta"}}
	env := newRelayTestEnv(t, upstream, channels)
	w := env.doAuthed(t, http.MethodGet, "/v1/models", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode model list: %v; body=%s", err, w.Body.String())
	}
	if payload.Object != "list" {
		t.Fatalf("object = %q, want list", payload.Object)
	}
	if len(payload.Data) != 2 || payload.Data[0].ID != "alpha" || payload.Data[1].ID != "beta" {
		t.Fatalf("data = %+v, want alpha,beta in order", payload.Data)
	}
	for _, item := range payload.Data {
		if item.Object != "model" || item.OwnedBy != "newtg" {
			t.Fatalf("model item = %+v, want object=model owned_by=newtg", item)
		}
	}
}

// ---------- (f) 客户端断开：partial UsageFact 且只终结一次 ----------

func TestRelayStreamingClientDisconnectTerminatesOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"par"},"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 保持流打开，直到客户端断开级联取消上游请求 context。
		<-r.Context().Done()
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	gateway := httptest.NewServer(env.handler.Handler())
	defer gateway.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+relayTestToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	buf := make([]byte, 256)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && n == 0 {
		t.Fatalf("read first frame: %v", readErr)
	}
	if !strings.Contains(string(buf[:n]), "par") {
		t.Fatalf("first frame = %q, want forwarded token content", buf[:n])
	}

	// 客户端断开：网关必须停止转发、关闭上游 body，并以 partial fact 终结会话。
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for env.billing.terminateCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billing session was not terminated after client disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 给潜在的双重终结留出观察窗口。
	time.Sleep(100 * time.Millisecond)
	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want exactly 1 after disconnect", got)
	}
	fact := env.billing.lastFact()
	if !fact.Stream {
		t.Fatal("disconnect UsageFact.Stream = false, want true")
	}
	if fact.Usage.Source != contract.UsageUpstreamPartial {
		t.Fatalf("disconnect UsageFact.Source = %d, want UsageUpstreamPartial", fact.Usage.Source)
	}
}

// ---------- 渠道连通性测试器（deliverable 3） ----------

func TestRelayChannelConnectivityTester(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	ch := &store.Channel{
		ID:           1,
		Type:         string(contract.ChannelTypeOpenAI),
		BaseURL:      &baseURL,
		Credentials:  sealedTestCredentials(t, sealer),
		ModelMapping: []byte(`{"client-model":"mapped-model"}`),
		CreatedAt:    time.Now().UTC(),
	}
	tester := NewChannelConnectivityTester(sealer, upstream.Client())
	latency, err := tester(context.Background(), ch, "client-model")
	if err != nil {
		t.Fatalf("tester returned error: %v", err)
	}
	if latency < 0 {
		t.Fatalf("latency = %d, want >= 0", latency)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer "+upstreamCredential {
		t.Fatalf("upstream Authorization = %q, want channel credential", gotAuth)
	}

	t.Run("non-2xx error is non-sensitive", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key sk-xyz"}}`)
		}))
		defer bad.Close()
		badURL := bad.URL
		badCh := &store.Channel{
			ID:          1,
			Type:        string(contract.ChannelTypeOpenAI),
			BaseURL:     &badURL,
			Credentials: sealedTestCredentials(t, sealer),
		}
		_, err := NewChannelConnectivityTester(sealer, bad.Client())(context.Background(), badCh, "gpt-test")
		if err == nil {
			t.Fatal("want error for non-2xx upstream")
		}
		msg := err.Error()
		if strings.Contains(msg, badURL) || strings.Contains(msg, upstreamCredential) || strings.Contains(msg, "sk-xyz") {
			t.Fatalf("tester error leaks sensitive data: %q", msg)
		}
	})
}

// ---------- 请求体上限 ----------

func TestRelayRejectsOversizedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for oversized body")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	huge := strings.Repeat("x", maxRelayBodyBytes+1)
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		fmt.Sprintf(`{"model":"gpt-test","messages":[{"role":"user","content":"%s"}]}`, huge))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"new_api_error"`) {
		t.Fatalf("body = %s, want OpenAI error envelope", w.Body.String())
	}
}

// ---------- include_usage：客户端显式 false 优先 ----------

func TestRelayStreamRespectsExplicitIncludeUsageFalse(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		// 即使上游仍返回 usage，客户端显式 false 时也不得合成/转发 usage 块。
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	mu.Lock()
	options, _ := upstreamBody["stream_options"].(map[string]any)
	includeUsage, present := options["include_usage"].(bool)
	mu.Unlock()
	if !present || includeUsage {
		t.Fatalf("upstream include_usage = %v (present=%v), want explicit false preserved", includeUsage, present)
	}

	out := w.Body.String()
	if strings.Contains(out, `"usage":`) {
		t.Fatalf("explicit include_usage=false must not synthesize a usage chunk; body=%s", out)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] count = %d, want exactly 1; body=%s", got, out)
	}
	if got := strings.Count(out, `"finish_reason":"stop"`); got != 1 {
		t.Fatalf("finish_reason count = %d, want 1; body=%s", got, out)
	}
}

// ---------- 软/硬流错误（relay-03 §4.3/§5.2/§10.1/§10.3） ----------

// scriptedStreamAdapter 直接产出预设事件，用于覆盖真实上游难以复现的
// soft（坏块）/hard（读取失败）流错误路径。
type scriptedStreamAdapter struct {
	url    string
	events []contract.StreamEvent
}

func (a *scriptedStreamAdapter) Type() contract.ChannelType  { return contract.ChannelTypeOpenAI }
func (a *scriptedStreamAdapter) Caps() contract.Capabilities { return contract.Capabilities{} }
func (a *scriptedStreamAdapter) BuildRequest(*contract.RelayContext, *contract.Request) (*http.Request, error) {
	return http.NewRequest(http.MethodPost, a.url, nil)
}
func (a *scriptedStreamAdapter) ParseResponse(*contract.RelayContext, *http.Response) (*contract.Response, contract.Usage, error) {
	return nil, contract.Usage{}, fmt.Errorf("scripted adapter: ParseResponse not used")
}
func (a *scriptedStreamAdapter) ClassifyError(int, []byte) contract.ErrorClass {
	return contract.ErrUnknown
}
func (a *scriptedStreamAdapter) StreamEvents(*contract.RelayContext, io.ReadWriter) iter.Seq[contract.StreamEvent] {
	return func(yield func(contract.StreamEvent) bool) {
		for _, ev := range a.events {
			if !yield(ev) {
				return
			}
		}
	}
}

func newScriptedStreamEnv(t *testing.T, adapter contract.Adapter) (*relayTestEnv, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	env := newRelayTestEnv(t, upstream, nil)
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil }
	return env, upstream
}

func TestRelayStreamSoftErrorContinues(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	adapter := &scriptedStreamAdapter{
		url: upstream.URL + "/v1/chat/completions",
		events: []contract.StreamEvent{
			{Kind: contract.EventDelta, Delta: "ok"},
			// 软错误：Raw 非空（坏 JSON 块/上游 error 事件）→ 记录后继续。
			{Kind: contract.EventError, Raw: []byte(`data: {bad json}`)},
			{Kind: contract.EventFinish, Raw: []byte(`data: {"choices":[{"finish_reason":"stop"}]}`)},
		},
	}
	env := newRelayTestEnv(t, upstream, nil)
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil }

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("soft error must not abort the stream; body=%s", out)
	}
	if strings.Contains(out, `"error"`) {
		t.Fatalf("soft errors must not inject an error frame; body=%s", out)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] count = %d, want 1; body=%s", got, out)
	}
	if env.billing.terminateCount() != 1 {
		t.Fatalf("Terminate calls = %d, want 1", env.billing.terminateCount())
	}
}

func TestRelayStreamHardErrorStopsWithoutErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	adapter := &scriptedStreamAdapter{
		url: upstream.URL + "/v1/chat/completions",
		events: []contract.StreamEvent{
			{Kind: contract.EventDelta, Delta: "part"},
			// 硬错误：Raw 为空（读取失败/事件超限）→ 终止，不注入错误对象/[DONE]。
			{Kind: contract.EventError},
		},
	}
	env := newRelayTestEnv(t, upstream, nil)
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil }

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already sent); body=%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"content":"part"`) {
		t.Fatalf("partial content must be forwarded; body=%s", out)
	}
	if strings.Contains(out, `"error"`) {
		t.Fatalf("hard stream error must not inject an error frame; body=%s", out)
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("hard stream error must not forge a normal [DONE]; body=%s", out)
	}
	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want exactly 1", got)
	}
	if fact := env.billing.lastFact(); fact.Usage.Source != contract.UsageUpstreamPartial {
		t.Fatalf("hard error UsageFact.Source = %d, want partial", fact.Usage.Source)
	}
}

// ---------- 错误映射（429 / AttemptsError） ----------

func TestRelayErrorStatusMapping(t *testing.T) {
	upstream := &relay.UpstreamStatusError{StatusCode: http.StatusTooManyRequests, Class: contract.ErrUpstreamRateLimit}
	if status, code, _ := mapRelayError(&relay.FastFailError{Err: upstream}); status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
		t.Fatalf("FastFail 429 => (%d,%q), want (429, rate_limit_exceeded)", status, code)
	}

	attempts := &relay.AttemptsError{Attempts: 4, Last: &relay.RetryableError{Err: upstream}}
	status, code, message := mapRelayError(attempts)
	if status != http.StatusBadGateway || code != "upstream_error" {
		t.Fatalf("AttemptsError => (%d,%q), want (502, upstream_error)", status, code)
	}
	if !strings.Contains(message, "4 attempt") {
		t.Fatalf("AttemptsError message = %q, want attempt count", message)
	}
}

// ---------- 令牌状态 / pin 403 / x-api-key ----------

func TestRelayTokenStateRejections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for rejected tokens")
	}))
	defer upstream.Close()

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`
	cases := []struct {
		name   string
		mutate func(*auth.TokenMeta)
	}{
		{"expired", func(m *auth.TokenMeta) {
			past := time.Now().Add(-time.Minute)
			m.ExpiresAt = &past
		}},
		{"zero quota", func(m *auth.TokenMeta) {
			m.UnlimitedQuota = false
			m.RemainQuota = 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRelayTestEnv(t, upstream, nil)
			env.tokens.mu.Lock()
			meta := env.tokens.byHash[auth.HashTokenKey(relayTestToken)]
			tc.mutate(meta)
			env.tokens.mu.Unlock()

			w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"type":"new_api_error"`) {
				t.Fatalf("body = %s, want OpenAI envelope", w.Body.String())
			}
		})
	}
}

func TestRelayPinForbiddenForNonAdmin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when pin is forbidden")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	// legacy pin 载体：查询 key 是 sk-pintest，末段 1 是渠道 id。
	env.handler.tokens = newFakeRelayTokenStore("sk-pintest", &auth.TokenMeta{
		ID:             9,
		UserID:         relayTestUserID,
		Status:         "enabled",
		UnlimitedQuota: true,
	})
	env.handler.access = &fakeRelayAccess{role: "user", status: "enabled"}

	w := env.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"Authorization": "Bearer sk-pintest-1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"channel_pin_forbidden"`) {
		t.Fatalf("body = %s, want channel_pin_forbidden", w.Body.String())
	}
}

func TestRelayModelsAcceptsXAPIKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("model list must not hit upstream")
	}))
	defer upstream.Close()

	channels := &fakeRelayChannelStore{models: []string{"alpha"}}
	env := newRelayTestEnv(t, upstream, channels)
	w := env.do(t, http.MethodGet, "/v1/models", "",
		map[string]string{"x-api-key": relayTestToken})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (x-api-key carrier); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"alpha"`) {
		t.Fatalf("body = %s, want model list", w.Body.String())
	}
}

// ---------- usage 合并：非零覆盖、零值不擦除 ----------

func TestRelayMergeUsageNeverErases(t *testing.T) {
	dst := contract.Usage{Prompt: 5, Completion: 2, Total: 7, CacheRead: 4, Reasoning: 1}
	mergeUsage(&dst, contract.Usage{Completion: 3})
	if dst.Prompt != 5 || dst.Completion != 3 || dst.Total != 7 || dst.CacheRead != 4 || dst.Reasoning != 1 {
		t.Fatalf("merge erased existing dimensions: %+v", dst)
	}
	mergeUsage(&dst, contract.Usage{Total: 9, CacheRead: 0})
	if dst.Total != 9 || dst.CacheRead != 4 {
		t.Fatalf("non-zero merge failed: %+v", dst)
	}
}
