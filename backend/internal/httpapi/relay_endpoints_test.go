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
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
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
	// byGroup 非 nil 时按分组返回候选（M2 跨组用例）；candidates 仅在
	// byGroup 为空时使用，保证既有单组用例的行为逐字节不变。
	byGroup map[string][]contract.ChannelCandidate

	channels map[int64]*store.Channel
	getErr   error

	models    []string
	modelsErr error

	lastModel     string
	lastGroup     string
	groupsQueried []string
	getCalls      int
	candCalls     int
	modelCalls    int
}

func (f *fakeRelayChannelStore) Candidates(_ context.Context, model, group string) ([]contract.ChannelCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candCalls++
	f.lastModel, f.lastGroup = model, group
	f.groupsQueried = append(f.groupsQueried, group)
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
	if f.byGroup != nil {
		return append([]contract.ChannelCandidate(nil), f.byGroup[group]...), nil
	}
	return append([]contract.ChannelCandidate(nil), f.candidates...), nil
}

func (f *fakeRelayChannelStore) queriedGroups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.groupsQueried...)
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

// recordingBilling 包装 billingseam.Stub 并记录 Terminate/Refund 的事实。
// 成功出口走 Terminate（可断言最终 UsageFact）；失败/取消/panic 出口走
// Refund（R-85），只记录次数。
type recordingBilling struct {
	mu          sync.Mutex
	stub        *billingseam.Stub
	terminated  []contract.UsageFact
	preConsumed []contract.QuotaEstimate
	refunds     int
}

func newRecordingBilling() *recordingBilling {
	return &recordingBilling{stub: billingseam.NewStub()}
}

func (b *recordingBilling) PreConsume(est contract.QuotaEstimate) error {
	b.mu.Lock()
	b.preConsumed = append(b.preConsumed, est)
	b.mu.Unlock()
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
func (b *recordingBilling) Refund() error {
	b.mu.Lock()
	b.refunds++
	b.mu.Unlock()
	return b.stub.Refund()
}
func (b *recordingBilling) Reserved() int64               { return b.stub.Reserved() }
func (b *recordingBilling) NeedsRefund() bool             { return b.stub.NeedsRefund() }
func (b *recordingBilling) RecoverAbandoned(string) error { return b.stub.RecoverAbandoned("") }

func (b *recordingBilling) terminateCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.terminated)
}

func (b *recordingBilling) preConsumeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.preConsumed)
}

func (b *recordingBilling) refundCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refunds
}

func (b *recordingBilling) lastFact() contract.UsageFact {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.terminated) == 0 {
		return contract.UsageFact{}
	}
	return b.terminated[len(b.terminated)-1]
}

func (b *recordingBilling) preConsumeCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.preConsumed)
}

func (b *recordingBilling) lastEstimate() (contract.QuotaEstimate, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.preConsumed) == 0 {
		return contract.QuotaEstimate{}, false
	}
	return b.preConsumed[len(b.preConsumed)-1], true
}

// observingStreamAdapter 包装真实适配器，用于：
//   - usageSeen：在 EventUsage 真正通过桥接时关闭，让测试能确定「上游已给出
//     部分用量」后再断开客户端，消除时序竞态；
//   - rc：记录执行层写入的 RelayContext，供测试断言 StreamStatus.EndReason。
type observingStreamAdapter struct {
	inner     contract.StreamAdapter
	usageSeen chan struct{}
	once      sync.Once
	rc        *contract.RelayContext
}

func (a *observingStreamAdapter) Type() contract.ChannelType  { return a.inner.Type() }
func (a *observingStreamAdapter) Caps() contract.Capabilities { return a.inner.Caps() }
func (a *observingStreamAdapter) BuildRequest(rc *contract.RelayContext, req *contract.Request) (*http.Request, error) {
	return a.inner.BuildRequest(rc, req)
}
func (a *observingStreamAdapter) ParseResponse(rc *contract.RelayContext, resp *http.Response) (*contract.Response, contract.Usage, error) {
	return a.inner.ParseResponse(rc, resp)
}
func (a *observingStreamAdapter) ClassifyError(status int, body []byte) contract.ErrorClass {
	return a.inner.ClassifyError(status, body)
}
func (a *observingStreamAdapter) StreamEvents(rc *contract.RelayContext, rw io.ReadWriter) iter.Seq[contract.StreamEvent] {
	a.rc = rc
	return func(yield func(contract.StreamEvent) bool) {
		a.inner.StreamEvents(rc, rw)(func(ev contract.StreamEvent) bool {
			if ev.Kind == contract.EventUsage && a.usageSeen != nil {
				a.once.Do(func() { close(a.usageSeen) })
			}
			return yield(ev)
		})
	}
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
	// byGroup 非 nil 的跨组装置自带候选，不能注入默认候选覆盖分组语义。
	if channels.candidates == nil && channels.candidatesErr == nil && channels.byGroup == nil {
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
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
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

// fakeRelayModelStatus 是 RelayModelStatusStore 的内存 fake（R-84 测试）。
type fakeRelayModelStatus struct {
	mu     sync.Mutex
	status map[string]int16
	err    error
	calls  []string
}

func (f *fakeRelayModelStatus) ModelPricingStatus(_ context.Context, model string) (int16, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, model)
	if f.err != nil {
		return 0, false, f.err
	}
	status, ok := f.status[model]
	return status, ok, nil
}

func (f *fakeRelayModelStatus) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---------- 禁用模型（R-84 / TC-F-06-13-01） ----------

// TestRelayDisabledModelReturns404：model_pricing.status=2 的模型必须在派发
// 与预扣之前返回 404 model_not_found，绝不触达上游。
func TestRelayDisabledModelReturns404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a disabled model")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	modelStatus := &fakeRelayModelStatus{status: map[string]int16{"gpt-test": modelPricingStatusDisabled}}
	env.handler.modelStatus = modelStatus

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"model_not_found"`) {
		t.Fatalf("body = %s, want model_not_found", w.Body.String())
	}
	if env.billing.preConsumeCalls() != 0 {
		t.Fatalf("PreConsume calls = %d, want 0（停用检查必须在预扣之前）", env.billing.preConsumeCalls())
	}
	if env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatal("disabled model must not open a billing session")
	}
	if modelStatus.callCount() != 1 {
		t.Fatalf("status lookup calls = %d, want 1", modelStatus.callCount())
	}
}

// TestRelayModelWithoutPricingRowUsesDefault：无定价行的模型按默认价兜底，
// 停用检查必须放行（found=false），请求照常完成。
func TestRelayModelWithoutPricingRowUsesDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.handler.modelStatus = &fakeRelayModelStatus{status: map[string]int16{}}

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（未定价模型按默认价放行）; body=%s", w.Code, w.Body.String())
	}
}

// TestRelayModelStatusLookupFailureReturns500：状态查询失败是服务端故障，
// 固定 500 database_error，不泄露 SQL/DSN（GC-6）。
func TestRelayModelStatusLookupFailureReturns500(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when the status lookup fails")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.handler.modelStatus = &fakeRelayModelStatus{err: errors.New("pq: dsn postgres://secret@host/db")}

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"database_error"`) {
		t.Fatalf("body = %s, want database_error", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "postgres://") {
		t.Fatalf("error body leaks storage details: %s", w.Body.String())
	}
	if env.billing.preConsumeCalls() != 0 {
		t.Fatal("status lookup failure must not open a billing session")
	}
}

// TestRelayBillingFactoryReceivesRelayContext 冻结 T9 的工厂签名语义：
// 工厂必须拿到本请求的 RelayContext（真实引擎靠它绑定 RequestID/UserID/
// TokenID/UsingGroup），且每请求只构造一次。
func TestRelayBillingFactoryReceivesRelayContext(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	var (
		mu     sync.Mutex
		seenRC []*contract.RelayContext
	)
	env.handler.billing = func(rc *contract.RelayContext) contract.BillingSession {
		mu.Lock()
		seenRC = append(seenRC, rc)
		mu.Unlock()
		return billingseam.NewStub()
	}

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenRC) != 1 {
		t.Fatalf("billing factory calls = %d, want 1", len(seenRC))
	}
	rc := seenRC[0]
	if rc == nil || rc.RequestID == "" {
		t.Fatalf("factory rc = %+v, want the request's RelayContext with RequestID", rc)
	}
	if rc.UserID != int(relayTestUserID) || rc.TokenID != int(relayTestTokenID) {
		t.Fatalf("factory rc identity = user %d/token %d, want %d/%d",
			rc.UserID, rc.TokenID, relayTestUserID, relayTestTokenID)
	}
	if rc.OriginModel != "gpt-test" {
		t.Fatalf("factory rc OriginModel = %q, want gpt-test", rc.OriginModel)
	}
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

// TestRelayStreamHeaderOnlyUsage 覆盖 finding 5：上游流内没有任何 usage 块时，
// bridgeStream 必须尝试 `x-model-response-usage` 响应头（relay-03 R-05 的
// 优先级 3），而不是直接把零值标记为 estimated。否则上游精确报告的用量被丢弃，
// 计费退化成（尚未实现的）本地估算。
func TestRelayStreamHeaderOnlyUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-model-response-usage",
			`{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}`)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
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
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// include_usage 默认开启：合成末块应带头部用量，而不是全零。
	out := w.Body.String()
	if !strings.Contains(out, `"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}`) {
		t.Fatalf("合成 usage 块未使用 x-model-response-usage 头: %s", out)
	}

	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want exactly 1", got)
	}
	fact := env.billing.lastFact()
	if fact.Usage.Prompt != 7 || fact.Usage.Completion != 5 || fact.Usage.Total != 12 {
		t.Fatalf("UsageFact.Usage = %+v, want header usage 7/5/12", fact.Usage)
	}
	if fact.Usage.Source != contract.UsageUpstreamExact {
		t.Fatalf("UsageFact.Usage.Source = %v, want UsageUpstreamExact", fact.Usage.Source)
	}
}

// TestRelayStreamAllZeroUsageFallsBackToHeader 覆盖 re-review fix 1：
// 上游发送了 usage 事件但全零（`usage: {}` 或全零帧）时，`sawUsage` 已置位，
// 旧逻辑因此跳过头部的精确值，最终产出零值 upstream_exact。头部兜底必须以
// 「累计到非零数字」为条件，而不是「是否出现过 usage 事件」。
func TestRelayStreamAllZeroUsageFallsBackToHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-model-response-usage",
			`{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}`)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}` + "\n\n",
			// 全零 usage 帧：事件存在但没有任何用量事实。
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
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
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// include_usage 默认开启：合成末块必须使用头部数值，不能是全零。
	out := w.Body.String()
	if !strings.Contains(out, `"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}`) {
		t.Fatalf("全零 usage 帧后的合成用量块未使用 x-model-response-usage 头: %s", out)
	}

	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want exactly 1", got)
	}
	fact := env.billing.lastFact()
	if fact.Usage.Prompt != 7 || fact.Usage.Completion != 5 || fact.Usage.Total != 12 {
		t.Fatalf("UsageFact.Usage = %+v，期望头部用量 7/5/12（全零事件不得覆盖头部）", fact.Usage)
	}
	if fact.Usage.Source != contract.UsageUpstreamExact {
		t.Fatalf("UsageFact.Usage.Source = %v, want UsageUpstreamExact", fact.Usage.Source)
	}
}

// TestRelayStreamClientAbortRefunds 覆盖 R-85：客户端断开是失败出口，必须
// 恰好一次 Refund（不得 Terminate——真实引擎会把零值事实按按次价结算）。
func TestRelayStreamClientAbortRefunds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-model-response-usage",
			`{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}`)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"part"},"finish_reason":null}]}`+"\n\n")
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
	if n, readErr := resp.Body.Read(buf); readErr != nil && n == 0 {
		t.Fatalf("read first frame: %v", readErr)
	}
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for env.billing.refundCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billing session was not refunded after client disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1 after disconnect", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（失败出口必须退款而不是结算）", got)
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

// TestRelayModelsShapeSplitByHeaders 冻结 R-W18-13 / relay-04 §10.1 的头分流：
// 同一份启用模型清单按客户端协议头返回 OpenAI / Anthropic / Gemini 三种形状。
func TestRelayModelsShapeSplitByHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("model list must be served from the channel repo, not upstream")
	}))
	defer upstream.Close()
	channels := &fakeRelayChannelStore{models: []string{"alpha", "beta"}}
	env := newRelayTestEnv(t, upstream, channels)

	t.Run("openai-default", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1/models", "")
		var payload struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v; body=%s", err, w.Body.String())
		}
		if w.Code != http.StatusOK || payload.Object != "list" || len(payload.Data) != 2 ||
			payload.Data[0].ID != "alpha" || payload.Data[0].Object != "model" {
			t.Fatalf("openai shape = %d %+v", w.Code, payload)
		}
	})

	t.Run("anthropic-headers", func(t *testing.T) {
		w := env.do(t, http.MethodGet, "/v1/models", "", map[string]string{
			"x-api-key":         relayTestToken,
			"anthropic-version": "2023-06-01",
		})
		var payload struct {
			Data []struct {
				Type        string `json:"type"`
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				CreatedAt   string `json:"created_at"`
			} `json:"data"`
			HasMore bool    `json:"has_more"`
			FirstID *string `json:"first_id"`
			LastID  *string `json:"last_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v; body=%s", err, w.Body.String())
		}
		if w.Code != http.StatusOK || len(payload.Data) != 2 || payload.Data[0].Type != "model" ||
			payload.Data[0].DisplayName != "alpha" || payload.Data[0].CreatedAt == "" ||
			payload.HasMore || payload.FirstID == nil || *payload.FirstID != "alpha" ||
			payload.LastID == nil || *payload.LastID != "beta" {
			t.Fatalf("anthropic shape = %d %+v", w.Code, payload)
		}
	})

	t.Run("anthropic-headers-win-over-gemini", func(t *testing.T) {
		// P3：同时携带 Claude 与 Gemini 兼容头时，Anthropic 优先（relay-04
		// §10.1 的判定顺序：x-api-key+anthropic-version 先于 x-goog-api-key）。
		w := env.do(t, http.MethodGet, "/v1/models", "", map[string]string{
			"x-api-key":         relayTestToken,
			"anthropic-version": "2023-06-01",
			"x-goog-api-key":    relayTestToken,
		})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"display_name":"alpha"`) {
			t.Fatalf("status = %d; body=%s, want Anthropic shape", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `"models":[`) {
			t.Fatalf("body = %s, must not be Gemini shape when Anthropic headers are present", w.Body.String())
		}
	})

	t.Run("gemini-header", func(t *testing.T) {
		w := env.do(t, http.MethodGet, "/v1/models", "", map[string]string{"x-goog-api-key": relayTestToken})
		var payload struct {
			Models []struct {
				Name                       string   `json:"name"`
				DisplayName                string   `json:"displayName"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v; body=%s", err, w.Body.String())
		}
		if w.Code != http.StatusOK || len(payload.Models) != 2 ||
			payload.Models[0].Name != "models/alpha" || payload.Models[0].DisplayName != "alpha" ||
			len(payload.Models[0].SupportedGenerationMethods) != 1 ||
			payload.Models[0].SupportedGenerationMethods[0] != "embedContent" {
			t.Fatalf("gemini shape = %d %+v", w.Code, payload)
		}
	})

	t.Run("v1beta-models-gemini", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1beta/models", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"models/alpha"`) {
			t.Fatalf("v1beta list = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("v1beta-openai-models-stays-openai", func(t *testing.T) {
		// 该路径设计固定 OpenAI 形状，即使携带 Gemini 载体也不得改成 models[]。
		w := env.do(t, http.MethodGet, "/v1beta/openai/models?key="+relayTestToken, "", nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"alpha"`) {
			t.Fatalf("v1beta/openai/models = %d %s", w.Code, w.Body.String())
		}
	})
}

// TestRelayModelRetrieval 覆盖 GET /v1/models/{model} 与 /v1beta/models/{model}：
// 存在 → 对应协议形状；不存在 → 404 OpenAI 信封。
func TestRelayModelRetrieval(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("model retrieval must not hit upstream")
	}))
	defer upstream.Close()
	channels := &fakeRelayChannelStore{models: []string{"alpha", "Qwen/Qwen2.5-72B"}}
	env := newRelayTestEnv(t, upstream, channels)

	t.Run("openai-found", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1/models/alpha", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"alpha"`) ||
			!strings.Contains(w.Body.String(), `"object":"model"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	// P2-2：模型名允许包含 `/`（厂商前缀）；单段路由会把这类请求变成 405。
	t.Run("openai-found-slash-model", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1/models/Qwen/Qwen2.5-72B", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"Qwen/Qwen2.5-72B"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("openai-missing-slash-model", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1/models/Qwen/does-not-exist", "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"model_not_found"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("openai-missing", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1/models/does-not-exist", "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"model_not_found"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("anthropic-found", func(t *testing.T) {
		w := env.do(t, http.MethodGet, "/v1/models/alpha", "", map[string]string{
			"x-api-key":         relayTestToken,
			"anthropic-version": "2023-06-01",
		})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"type":"model"`) ||
			!strings.Contains(w.Body.String(), `"display_name":"alpha"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("v1beta-found", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1beta/models/alpha", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"models/alpha"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("v1beta-found-slash-model", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1beta/models/Qwen/Qwen2.5-72B", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"models/Qwen/Qwen2.5-72B"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("v1beta-missing", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1beta/models/does-not-exist", "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"model_not_found"`) {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		for _, path := range []string{"/v1/models/alpha", "/v1beta/models", "/v1beta/models/alpha", "/v1beta/openai/models"} {
			w := env.do(t, http.MethodGet, path, "", nil)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d, want 401; body=%s", path, w.Code, w.Body.String())
			}
		}
	})
}

// TestBetaFallbackUsesRelayEnvelope 覆盖 P2-2/P3：`/v1beta/*` 的未匹配方法
// 与未知路径都必须回 OpenAI JSON 信封，而不是 Go ServeMux 的 text/plain 405。
func TestBetaFallbackUsesRelayEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fallback must not hit upstream")
	}))
	defer upstream.Close()
	env := newRelayTestEnv(t, upstream, &fakeRelayChannelStore{models: []string{"alpha"}})

	t.Run("method-not-allowed", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodPut, "/v1beta/models/alpha", "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405; body=%s", w.Code, w.Body.String())
		}
		if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Fatalf("Allow = %q, want GET", allow)
		}
		body := assertOpenAIErrorEnvelope(t, w, "method_not_allowed")
		if body.Message == "" || !strings.Contains(body.Message, "requestId:") {
			t.Fatalf("message = %q, want requestId suffix", body.Message)
		}
	})

	t.Run("unknown-path", func(t *testing.T) {
		w := env.doAuthed(t, http.MethodGet, "/v1beta/unknown", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
		}
		assertOpenAIErrorEnvelope(t, w, "invalid_request_error")
	})
}

// ---------- (f) 客户端断开：失败退款且只终结一次 ----------

func TestRelayStreamingClientDisconnectRefundsOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"par"},"finish_reason":null}]}`+"\n\n")
		// 上游确实给出了部分用量（finding 3：只有这种情况才是 upstream_partial）。
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 保持流打开，直到客户端断开级联取消上游请求 context。
		<-r.Context().Done()
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	observing := &observingStreamAdapter{inner: adapters.NewOpenAI(), usageSeen: make(chan struct{})}
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return observing, nil }
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
	// 等上游用量真正被桥接消费后再断开：否则 partial/estimated 的判定依赖时序。
	select {
	case <-observing.usageSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("usage event was not observed before disconnect")
	}

	// 客户端断开：网关必须停止转发、关闭上游 body，并全额退款（R-85）。
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for env.billing.refundCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billing session was not refunded after client disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 给潜在的双重终结留出观察窗口。
	time.Sleep(100 * time.Millisecond)
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1 after disconnect", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（断开必须退款）", got)
	}
}

// TestRelayStreamingDisconnectWithoutUsageRefunds 覆盖断开且从未观察到上游
// 用量的情形：仍是失败出口 → Refund（不再构造 estimated fact 去结算）。
func TestRelayStreamingDisconnectWithoutUsageRefunds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"part"},"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
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
	if n, readErr := resp.Body.Read(buf); readErr != nil && n == 0 {
		t.Fatalf("read first frame: %v", readErr)
	}
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for env.billing.refundCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billing session was not refunded after client disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1 after disconnect", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（无用量断开也必须退款）", got)
	}
}

// TestRelayStreamWatchdogTimeoutRefundsOnce 覆盖 finding 4 + R-85：
// 上游静默超过看门狗阈值 → EndReason=timeout、恰好一次 Refund、不写 [DONE]。
func TestRelayStreamWatchdogTimeoutRefundsOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"stalled"},"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 永远不再发送任何字节，直到网关关闭 body 级联取消上游请求。
		<-r.Context().Done()
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.handler.streamInactivity = 100 * time.Millisecond
	observing := &observingStreamAdapter{inner: adapters.NewOpenAI()}
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return observing, nil }

	start := time.Now()
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（响应头已发送）; body=%s", w.Code, w.Body.String())
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("handler returned after %v，早于看门狗阈值（未真正等待超时）", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("watchdog did not fire in time: %v", elapsed)
	}
	if strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("超时是异常结束，不得伪造 [DONE]; body=%s", w.Body.String())
	}
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（超时必须退款）", got)
	}
	if observing.rc == nil || observing.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded")
	}
	if observing.rc.StreamStatus.EndReason != contract.EndStreamTimeout {
		t.Fatalf("EndReason = %d, want EndStreamTimeout", observing.rc.StreamStatus.EndReason)
	}
}

// TestRelayStreamPanicRecovery 覆盖 finding 4 的 panic 恢复：进程不崩、
// EndReason=EndPanic、恰好一次 Refund、不写 [DONE]。
func TestRelayStreamPanicRecovery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	adapter := &scriptedStreamAdapter{
		url:        upstream.URL + "/v1/chat/completions",
		events:     []contract.StreamEvent{{Kind: contract.EventDelta, Delta: "before-panic"}},
		panicAfter: 1,
	}
	env := newRelayTestEnv(t, upstream, nil)
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil }

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（响应头已发送）; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("panic 是异常结束，不得伪造 [DONE]; body=%s", w.Body.String())
	}
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（panic 必须退款）", got)
	}
	if adapter.rc == nil || adapter.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded after panic")
	}
	if adapter.rc.StreamStatus.EndReason != contract.EndPanic {
		t.Fatalf("EndReason = %d, want EndPanic", adapter.rc.StreamStatus.EndReason)
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
		if got := channelTestErrorClass(err); got != "auth" {
			t.Fatalf("error_class = %q, want auth (401 upstream)", got)
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
// soft（坏块）/hard（读取失败）/panic 流错误路径。
type scriptedStreamAdapter struct {
	url    string
	events []contract.StreamEvent
	// panicAfter > 0 时，在产出第 panicAfter 个事件后 panic（finding 4 的恢复测试）。
	panicAfter int
	// rc 记录执行层传入的中继上下文（测试断言 StreamStatus）。
	rc *contract.RelayContext
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
func (a *scriptedStreamAdapter) StreamEvents(rc *contract.RelayContext, _ io.ReadWriter) iter.Seq[contract.StreamEvent] {
	a.rc = rc
	return func(yield func(contract.StreamEvent) bool) {
		for i, ev := range a.events {
			if !yield(ev) {
				return
			}
			if a.panicAfter > 0 && i+1 == a.panicAfter {
				panic("scripted adapter: intentional panic")
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
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（硬错误必须退款）", got)
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

// TestRelayBillingErrorMapping 覆盖 T9 的计费错误映射（R-45/R-86）：
//   - *billing.QuotaError 与 M2 桩的额度错误 → 402 insufficient_quota；
//   - 定价/钳制/表达式/配置错误 → 400 model_price_error（绝不落 502）；
//   - ErrRequestClosed / ErrSettlementInProgress → 409（不重试），后者带 Retry-After。
//
// 错误都经 relay.FastFailError 包装，与真实执行路径（预扣失败 → safeWrapf →
// FastFailError）保持同一错误链形状。
func TestRelayBillingErrorMapping(t *testing.T) {
	wrap := func(err error) error {
		return &relay.FastFailError{Err: fmt.Errorf("relay: 预扣费失败: %w", err)}
	}
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "wallet quota error",
			err:    wrap(&billing.QuotaError{Class: contract.ErrInsufficientQuota, Required: 150, Available: 100}),
			status: http.StatusPaymentRequired,
			code:   "insufficient_quota",
		},
		{
			name:   "token quota sentinel",
			err:    wrap(billing.ErrInsufficientQuota),
			status: http.StatusPaymentRequired,
			code:   "insufficient_quota",
		},
		{
			name:   "stub quota error keeps 402",
			err:    wrap(&billingseam.InsufficientQuotaError{Class: contract.ErrInsufficientQuota, Limit: 10}),
			status: http.StatusPaymentRequired,
			code:   "insufficient_quota",
		},
		{
			name:   "price error",
			err:    wrap(fmt.Errorf("billing: estimate quota: %w", billing.ErrPriceError)),
			status: http.StatusBadRequest,
			code:   "model_price_error",
		},
		{
			name:   "clamp error",
			err:    wrap(&pricing.ClampError{Clamp: &contract.QuotaClamp{Op: "QuotaFromFloatStrict", Clamped: 100}}),
			status: http.StatusBadRequest,
			code:   "model_price_error",
		},
		{
			name:   "expression error",
			err:    wrap(&expr.EvalError{Kind: expr.EvalSyntax, Message: "unexpected token"}),
			status: http.StatusBadRequest,
			code:   "model_price_error",
		},
		{
			name:   "invalid config",
			err:    wrap(fmt.Errorf("%w: billing.pre_consume_ratio must be > 0", billing.ErrInvalidConfig)),
			status: http.StatusBadRequest,
			code:   "model_price_error",
		},
		{
			name:   "request closed",
			err:    wrap(fmt.Errorf("%w: req-1", billing.ErrRequestClosed)),
			status: http.StatusConflict,
			code:   "request_closed",
		},
		{
			name:   "settlement in progress",
			err:    wrap(fmt.Errorf("%w: req-1", billing.ErrSettlementInProgress)),
			status: http.StatusConflict,
			code:   "settlement_in_progress",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, _ := mapRelayError(tc.err)
			if status != tc.status || code != tc.code {
				t.Fatalf("mapRelayError = (%d,%q), want (%d,%q)", status, code, tc.status, tc.code)
			}
		})
	}

	// Retry-After 只在结算并发冲突时出现（409 + 退避建议）。
	t.Run("settlement in progress sets Retry-After", func(t *testing.T) {
		rr := httptest.NewRecorder()
		writeRelayMappedError(rr, "req-1", wrap(fmt.Errorf("%w: req-1", billing.ErrSettlementInProgress)))
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rr.Code)
		}
		if got := rr.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q, want 1", got)
		}
	})
	t.Run("request closed has no Retry-After", func(t *testing.T) {
		rr := httptest.NewRecorder()
		writeRelayMappedError(rr, "req-1", wrap(fmt.Errorf("%w: req-1", billing.ErrRequestClosed)))
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rr.Code)
		}
		if got := rr.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After = %q, want empty", got)
		}
	})
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

// useLegacyPinToken 把 env 的令牌存储替换为 legacy sk-<key>[-<channelId>] 形态。
func useLegacyPinToken(env *relayTestEnv, meta auth.TokenMeta) {
	env.handler.tokens = newFakeRelayTokenStore("sk-pintest", &meta)
}

// TestRelayAdminPinRoutesToPinnedChannel 覆盖 finding 1（relay-01 §3.5/§5.4-A）：
// 管理员 pin → 只调用 pinned 渠道，即使其它候选优先级更高。
func TestRelayAdminPinRoutesToPinnedChannel(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}
	}
	upstream1 := httptest.NewServer(handler("pinned"))
	defer upstream1.Close()
	upstream2 := httptest.NewServer(handler("other"))
	defer upstream2.Close()

	env := newRelayTestEnv(t, upstream1, nil)
	base2 := upstream2.URL
	env.channels.channels[2] = &store.Channel{
		ID:              2,
		Name:            "other-openai",
		Type:            string(contract.ChannelTypeOpenAI),
		BaseURL:         &base2,
		Credentials:     sealedTestCredentials(t, env.sealer),
		SupportedModels: []byte(`["gpt-test"]`),
		ModelMapping:    []byte(`{}`),
		Status:          1,
		CreatedAt:       time.Now().UTC(),
	}
	// 候选 2 的 priority 更高：没有 pin 时它会被优先选中。
	env.channels.candidates = []contract.ChannelCandidate{
		{ChannelID: 1, Priority: 10, Weight: 1},
		{ChannelID: 2, Priority: 20, Weight: 1},
	}
	useLegacyPinToken(env, auth.TokenMeta{
		ID: 9, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
	})

	w := env.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"Authorization": "Bearer sk-pintest-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["pinned"] != 1 {
		t.Fatalf("pinned channel hits = %d, want 1", hits["pinned"])
	}
	if hits["other"] != 0 {
		t.Fatalf("non-pinned channel hits = %d, want 0（pin 必须固定渠道）", hits["other"])
	}
	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want 1", got)
	}
}

// TestRelayAdminPinAbsentChannelReturns404 覆盖 finding 1：pin 目标不在
// 候选集时，必须在 PreConsume 之前返回 404 channel_not_found。
func TestRelayAdminPinAbsentChannelReturns404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when the pinned channel is absent")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	useLegacyPinToken(env, auth.TokenMeta{
		ID: 9, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
	})

	w := env.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"Authorization": "Bearer sk-pintest-99"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"channel_not_found"`) {
		t.Fatalf("body = %s, want channel_not_found", w.Body.String())
	}
	if env.billing.preConsumeCalls() != 0 {
		t.Fatalf("PreConsume calls = %d, want 0（404 必须在预扣之前）", env.billing.preConsumeCalls())
	}
	if env.billing.terminateCount() != 0 {
		t.Fatalf("Terminate calls = %d, want 0（会话未开启）", env.billing.terminateCount())
	}
}

// TestRelayTokenModelLimitsEnforced 覆盖 finding 6（relay-01 §3.4）：
// model_limits 非空时拒绝白名单外模型；命中时放行；管理员 pin 跳过白名单。
func TestRelayTokenModelLimitsEnforced(t *testing.T) {
	var mu sync.Mutex
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	// 每个请求使用独立计费桩（Stub 是单会话实例）。
	env.handler.billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }

	env.tokens.mu.Lock()
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)].ModelLimits = []string{"other-model"}
	env.tokens.mu.Unlock()

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("restricted model status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"distributor_token_model_forbidden"`) {
		t.Fatalf("body = %s, want distributor_token_model_forbidden", w.Body.String())
	}
	mu.Lock()
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0（模型白名单在路由前拒绝）", upstreamCalls)
	}
	mu.Unlock()

	env.tokens.mu.Lock()
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)].ModelLimits = []string{"gpt-test"}
	env.tokens.mu.Unlock()
	w = env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusOK {
		t.Fatalf("allowed model status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// 管理员 pin 跳过白名单（§3.4 注：pin 优先级高于白名单）。
	useLegacyPinToken(env, auth.TokenMeta{
		ID: 9, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
		ModelLimits: []string{"other-model"},
	})
	w = env.do(t, http.MethodPost, "/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer sk-pintest-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("admin pin status = %d, want 200（pin 跳过模型白名单）; body=%s", w.Code, w.Body.String())
	}
}

// TestRelayTokenIPAllowlist 覆盖 finding 6（relay-01 §3.4）：allow_ips 非空时
// 客户端 IP 必须在列表中；无法解析客户端地址同样 403 access_denied。
func TestRelayTokenIPAllowlist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.handler.billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`

	env.tokens.mu.Lock()
	meta := env.tokens.byHash[auth.HashTokenKey(relayTestToken)]
	meta.AllowIPs = []string{"10.0.0.0/8"}
	env.tokens.mu.Unlock()
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed IP status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"access_denied"`) {
		t.Fatalf("body = %s, want access_denied", w.Body.String())
	}

	// httptest.NewRequest 的默认 RemoteAddr 是 192.0.2.1:1234。
	env.tokens.mu.Lock()
	meta.AllowIPs = []string{"192.0.2.1"}
	env.tokens.mu.Unlock()
	w = env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusOK {
		t.Fatalf("allowed IP status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// 客户端地址无法解析 → 403（fail-closed）。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	req.RemoteAddr = ""
	rec := httptest.NewRecorder()
	env.handler.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unparsable client address status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRelayTrustedProxyPolicyForAllowIPs 冻结 M2 的 X-Forwarded-For 信任策略：
// 未配置 TRUSTED_PROXIES 时忽略 XFF；直连对端受信时才采信。
func TestRelayTrustedProxyPolicyForAllowIPs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.handler.billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	env.tokens.mu.Lock()
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)].AllowIPs = []string{"203.0.113.0/24"}
	env.tokens.mu.Unlock()

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+relayTestToken)
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		req.RemoteAddr = "10.1.1.1:1234"
		rec := httptest.NewRecorder()
		env.handler.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 未配置 trusted proxies：XFF 完全忽略，客户端是直连的 10.1.1.1 → 403。
	if rec := do(); rec.Code != http.StatusForbidden {
		t.Fatalf("untrusted XFF status = %d, want 403（未配置时不得采信 XFF）", rec.Code)
	}
	// 直连对端 10.1.1.1 属于受信代理 → 取 XFF 的 203.0.113.9 → 200。
	env.handler.trustedProxies = parseTrustedProxies("10.0.0.0/8")
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("trusted proxy status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRelayModelsAcceptsQueryKey 覆盖 finding 12（relay-01 §3.1）：/v1/models
// 支持 ?key=<token> 兼容载体（鉴权中间件会把它归一化进 Authorization）。
//
// R-W18-13：`?key=` 同时是 Gemini 兼容载体，因此响应形状按 relay-04 §10.1
// 分流为 Gemini（`models[]`），而不是 OpenAI 的 `data[]`。
func TestRelayModelsAcceptsQueryKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("model list must not hit upstream")
	}))
	defer upstream.Close()

	channels := &fakeRelayChannelStore{models: []string{"alpha"}}
	env := newRelayTestEnv(t, upstream, channels)
	w := env.do(t, http.MethodGet, "/v1/models?key="+relayTestToken, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（?key= 载体）; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"models/alpha"`) {
		t.Fatalf("body = %s, want Gemini model list", w.Body.String())
	}
}

// TestRelayModelsAcceptsXGoogAPIKey 覆盖 finding 12：x-goog-api-key 兼容载体，
// 响应形状为 Gemini（relay-04 §10.1）。
func TestRelayModelsAcceptsXGoogAPIKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("model list must not hit upstream")
	}))
	defer upstream.Close()

	channels := &fakeRelayChannelStore{models: []string{"alpha"}}
	env := newRelayTestEnv(t, upstream, channels)
	w := env.do(t, http.MethodGet, "/v1/models", "",
		map[string]string{"x-goog-api-key": relayTestToken})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（x-goog-api-key 载体）; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"models/alpha"`) {
		t.Fatalf("body = %s, want Gemini model list", w.Body.String())
	}
}

// TestRelayQuotaEstimateReachesBilling 覆盖 finding 13：RelayContext 必须携带
// QuotaEstimate（Model/Group/MaxTokens），否则 PreConsume 永远看到零值。
func TestRelayQuotaEstimateReachesBilling(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.tokens.mu.Lock()
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)].Group = "vip"
	env.tokens.mu.Unlock()

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	est, ok := env.billing.lastEstimate()
	if !ok {
		t.Fatal("billing PreConsume was not called")
	}
	if est.Model != "gpt-test" {
		t.Fatalf("QuotaEstimate.Model = %q, want gpt-test", est.Model)
	}
	if est.Group != "vip" {
		t.Fatalf("QuotaEstimate.Group = %q, want vip（令牌分组）", est.Group)
	}
	if est.MaxTokens == nil || *est.MaxTokens != 64 {
		t.Fatalf("QuotaEstimate.MaxTokens = %v, want 64", est.MaxTokens)
	}
	if est.PromptTokens != 0 {
		t.Fatalf("QuotaEstimate.PromptTokens = %d, want 0（估算器未落地）", est.PromptTokens)
	}
}

// TestRelayRejectsStreamWithNMultiple 覆盖 finding 14：stream=true + n>1
// 必须在入口 400（SSE 桥接无法保留 choice 身份）。
//
// finding 4（二轮复审）：n 必须按 JSON 数值解码，而不是 *int。`2.0`/`2e0`
// 是合法的 JSON 数字且语义等于 2；用 *int 解码会失败并静默回落到 1，
// 从而绕过拒绝、把 n>1 的请求送进只保留 index=0 的流式桥。
func TestRelayRejectsStreamWithNMultiple(t *testing.T) {
	cases := []struct {
		name string
		n    string
	}{
		{"integer", `"n":2`},
		{"decimal", `"n":2.0`},
		{"exponent", `"n":2e0`},
		{"fractional greater than one", `"n":1.5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("upstream must not be called for unsupported stream+n")
			}))
			defer upstream.Close()

			env := newRelayTestEnv(t, upstream, nil)
			w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
				`{"model":"gpt-test","stream":true,`+tc.n+`,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("body = %s, want invalid_request", w.Body.String())
			}
			if env.billing.preConsumeCalls() != 0 {
				t.Fatal("stream+n>1 must be rejected before PreConsume")
			}
		})
	}
}

// TestRelayNonStreamStripsStreamOptions 覆盖 finding 11（relay-03 §2.3）：
// 非流式请求不得把 stream_options/include_usage 发给上游。
func TestRelayNonStreamStripsStreamOptions(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if _, present := gotBody["stream_options"]; present {
		t.Fatalf("upstream body still has stream_options: %v", gotBody)
	}
	if stream, _ := gotBody["stream"].(bool); stream {
		t.Fatalf("upstream body stream = true, want false: %v", gotBody)
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

// ---------- F-06-06 error_class 五值枚举 ----------

func TestChannelTestErrorClassEnum(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"upstream 401", &channelTestUpstreamError{StatusCode: 401, Class: contract.ErrUpstreamAuth}, "auth"},
		{"upstream 403", &channelTestUpstreamError{StatusCode: 403, Class: contract.ErrUpstreamAuth}, "auth"},
		{"param", &channelTestUpstreamError{StatusCode: 400, Class: contract.ErrParamInvalid}, "param"},
		{"content filter", &channelTestUpstreamError{StatusCode: 400, Class: contract.ErrContentFilter}, "param"},
		{"upstream 5xx", &channelTestUpstreamError{StatusCode: 500, Class: contract.ErrUpstreamServer}, "server"},
		{"rate limit", &channelTestUpstreamError{StatusCode: 429, Class: contract.ErrUpstreamRateLimit}, "server"},
		{"unknown status", &channelTestUpstreamError{StatusCode: 418, Class: contract.ErrUnknown}, "server"},
		{"plain error", errors.New("boom"), "server"},
		{"timeout transport", &channelTestTransportError{Class: channelTestClassTimeout}, "timeout"},
		{"network transport", &channelTestTransportError{Class: channelTestClassNetwork}, "network"},
	}
	allowed := map[string]bool{"auth": true, "timeout": true, "server": true, "network": true, "param": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := channelTestErrorClass(tc.err)
			if got != tc.want {
				t.Fatalf("channelTestErrorClass = %q, want %q", got, tc.want)
			}
			if !allowed[got] {
				t.Fatalf("error_class %q is outside the modules-06 F-06-06 enum", got)
			}
		})
	}
}

func TestChannelTestTransportClass(t *testing.T) {
	if got := channelTestTransportClass(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline => %q, want timeout", got)
	}
	if got := channelTestTransportClass(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)); got != "timeout" {
		t.Fatalf("wrapped deadline => %q, want timeout", got)
	}
	if got := channelTestTransportClass(&net.DNSError{Err: "i/o timeout", IsTimeout: true}); got != "timeout" {
		t.Fatalf("net timeout => %q, want timeout", got)
	}
	if got := channelTestTransportClass(&net.DNSError{Err: "no such host"}); got != "network" {
		t.Fatalf("dns failure => %q, want network", got)
	}
	if got := channelTestTransportClass(errors.New("connection refused")); got != "network" {
		t.Fatalf("connection failure => %q, want network", got)
	}
}

// TestRelayHandlerNilBillingFallsBackToStub 冻结 nil 工厂的降级语义：
// 未注入真实引擎时每请求构造一个新的 billingseam.Stub（M2 行为），
// 而不是共享单会话实例或返回 nil。
func TestRelayHandlerNilBillingFallsBackToStub(t *testing.T) {
	h := NewRelayHandler(RelayHandlerOptions{
		Channels: &fakeRelayChannelStore{},
		Sealer:   NewSecretSealer(relaySealerMaster),
	})
	first := h.billing(&contract.RelayContext{RequestID: "req-1"})
	if _, ok := first.(*billingseam.Stub); !ok {
		t.Fatalf("default billing = %T, want *billingseam.Stub", first)
	}
	second := h.billing(&contract.RelayContext{RequestID: "req-2"})
	if second == first {
		t.Fatal("nil factory must create a fresh stub per request（Stub 是单会话实例）")
	}
}
