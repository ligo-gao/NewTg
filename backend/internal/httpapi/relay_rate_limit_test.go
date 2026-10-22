package httpapi

// W7/M5-b 中继准入限流的单元验收（全内存：miniredis + fake 上游/渠道/令牌）。
//
// 覆盖的冻结裁决：
//   - R-M5-04 执行点：四个入口都在 BuildRelayContext + 分组解析之后、pin/
//     白名单/预扣/上游之前；429 的请求零 PreConsume、零上游调用。
//   - R-M5-05 短路顺序：B11 → B10 → B12，先超限者 429，后续桶不再自增。
//   - R-M5-06 分组覆盖：只覆盖 B10，按 rc.UsingGroup（auto=首组），0=该组不限。
//   - R-M5-08 /pg：跳过 B11，仍计 B10/B12。
//   - R-M5-09 429 形状：code=rate_limit_exceeded、固定文案 + Retry-After（秒）。
//   - R-M5-10 故障 fail-closed：500 rate_limit_check_failed，不放行。
//   - R-M5-13 管理员不豁免。
//   - R-M5-07 任务只有提交面计入，查询不计。
//
// 真实计费引擎（usage_logs=0）的端到端断言在
// relay_rate_limit_integration_test.go（-tags=integration）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tasks"
)

// rateLimitTestModel 是默认渠道支持的模型（chat / 多模态共用）。
const rateLimitTestModel = "gpt-test"

// rateLimitChatBody 是不带 stream 的最小 chat 请求体。
const rateLimitChatBody = `{"model":"` + rateLimitTestModel + `","messages":[{"role":"user","content":"hi"}]}`

// rateLimitChatStreamBody 触发流式桥接（请求体里的 stream 是唯一开关）。
const rateLimitChatStreamBody = `{"model":"` + rateLimitTestModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// rateLimitUpstream 记录上游调用次数，并同时提供非流式 JSON 与 SSE 两种响应。
type rateLimitUpstream struct {
	calls  atomic.Int64
	server *httptest.Server
}

func newRateLimitUpstream(t *testing.T, handler http.HandlerFunc) *rateLimitUpstream {
	t.Helper()
	u := &rateLimitUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *rateLimitUpstream) count() int { return int(u.calls.Load()) }

// defaultRateLimitUpstream 返回最小合法 chat completion；stream=true 时返回
// 两帧 + usage + DONE，让流式用例真的走 SSE 桥接（而不是被入口 400）。
func defaultRateLimitUpstream(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}

type rateLimitTestEnv struct {
	t          *testing.T
	mux        *http.ServeMux
	handler    *RelayHandler
	mr         *miniredis.Miniredis
	limiter    *ratelimit.Limiter
	billing    *rateLimitBilling
	upstream   *rateLimitUpstream
	tokens     *fakeRelayTokenStore
	sessions   *auth.SessionManager
	groups     *playgroundGroupsFake
	multimodal *captureMultimodalDispatcher
}

// rateLimitBilling 按请求构造独立的单会话桩（billingseam.Stub 一个实例只代表
// 一个请求），并聚合「PreConsume 次数」供 429/500 的零计费断言使用。
type rateLimitBilling struct {
	mu          sync.Mutex
	preConsumed int
}

func newRateLimitBilling() *rateLimitBilling { return &rateLimitBilling{} }

func (b *rateLimitBilling) factory(*contract.RelayContext) contract.BillingSession {
	return &rateLimitBillingSession{Stub: billingseam.NewStub(), parent: b}
}

func (b *rateLimitBilling) preConsumeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.preConsumed
}

type rateLimitBillingSession struct {
	*billingseam.Stub
	parent *rateLimitBilling
}

func (s *rateLimitBillingSession) PreConsume(est contract.QuotaEstimate) error {
	s.parent.mu.Lock()
	s.parent.preConsumed++
	s.parent.mu.Unlock()
	return s.Stub.PreConsume(est)
}

type rateLimitEnvOptions struct {
	// config 是任意配置读取面（mapConfig 或 WithDemoOverride 包装）。
	config      ConfigReader
	channelType contract.ChannelType
	model       string
	upstream    http.HandlerFunc
	multimodal  bool
	playground  bool
	// tokens 允许用例追加额外令牌（B11 隔离/auto 分组）。
	tokens     []rateLimitSeedToken
	accessRole string
}

type rateLimitSeedToken struct {
	plain string
	meta  auth.TokenMeta
}

func newRateLimitTestEnv(t *testing.T, opts rateLimitEnvOptions) *rateLimitTestEnv {
	t.Helper()
	if opts.channelType == "" {
		opts.channelType = contract.ChannelTypeOpenAI
	}
	if opts.model == "" {
		opts.model = rateLimitTestModel
	}
	if opts.upstream == nil {
		opts.upstream = defaultRateLimitUpstream
	}
	if opts.accessRole == "" {
		// R-M5-13：默认就用管理员身份，证明限流不看角色。
		opts.accessRole = "admin"
	}
	upstream := newRateLimitUpstream(t, opts.upstream)
	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.server.URL
	channels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{
			ChannelID: int(relayTestChannelID),
			Priority:  10,
			Weight:    1,
		}},
		channels: map[int64]*store.Channel{
			relayTestChannelID: {
				ID:              relayTestChannelID,
				Name:            "rate-limit-upstream",
				Type:            string(opts.channelType),
				BaseURL:         &baseURL,
				Credentials:     sealedTestCredentials(t, sealer),
				SupportedModels: []byte(`["` + opts.model + `"]`),
				ModelMapping:    []byte(`{}`),
				Status:          1,
				CreatedAt:       time.Now().UTC(),
			},
		},
		models: []string{opts.model},
	}
	tokens := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID: relayTestTokenID, UserID: relayTestUserID, Status: "enabled",
		Group: "default", UnlimitedQuota: true,
	})
	for _, seed := range opts.tokens {
		meta := seed.meta
		tokens.byHash[auth.HashTokenKey(seed.plain)] = &meta
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	limiter := ratelimit.New(rdb)
	billing := newRateLimitBilling()
	groups := newSessionOnlyGroups()

	relayOpts := RelayHandlerOptions{
		Channels:    channels,
		Tokens:      tokens,
		Access:      &fakeRelayAccess{role: opts.accessRole, status: "enabled"},
		Sealer:      sealer,
		Client:      upstream.server.Client(),
		Billing:     billing.factory,
		RateLimiter: limiter,
		Config:      opts.config,
		UserGroups:  groups,
	}
	env := &rateLimitTestEnv{
		t: t, mr: mr, limiter: limiter, billing: billing,
		upstream: upstream, tokens: tokens, groups: groups,
	}
	if opts.multimodal {
		env.multimodal = &captureMultimodalDispatcher{}
		relayOpts.Multimodal = env.multimodal
	}
	handler := NewRelayHandler(relayOpts)
	mux := http.NewServeMux()
	handler.Register(mux)
	env.handler = handler
	env.mux = mux
	if opts.playground {
		env.sessions = auth.NewSessionManager(rdb, []byte(testSessionSecret))
		handler.RegisterPlayground(mux, env.sessions, "")
	}
	return env
}

func (e *rateLimitTestEnv) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
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
	e.mux.ServeHTTP(w, req)
	return w
}

func (e *rateLimitTestEnv) chat(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer " + token})
}

func (e *rateLimitTestEnv) messages(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	return e.do(t, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": token})
}

func (e *rateLimitTestEnv) embeddings(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/embeddings", `{"model":"`+rateLimitTestModel+`","input":"hi"}`,
		map[string]string{"Authorization": "Bearer " + token})
}

// playgroundChat 发送一条会话路径请求（Cookie + CSRF + Origin，与
// PlaygroundSessionAuth/CSRF/OriginRefererCheck 同一套前置条件）。
func (e *rateLimitTestEnv) playgroundChat(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	sid, err := e.sessions.Create(context.Background(), 42, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		t.Fatalf("issue csrf: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/pg/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	req.Header.Set(auth.CSRFHeaderName, csrf)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

// ---------- 断言 helper ----------

func assertRateLimited429(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", w.Code, w.Body.String())
	}
	retry := w.Header().Get("Retry-After")
	if retry == "" {
		t.Fatalf("Retry-After 缺失；headers=%v", w.Header())
	}
	seconds, err := strconv.Atoi(retry)
	if err != nil || seconds < 1 || seconds > 60 {
		t.Fatalf("Retry-After = %q，期望 (0,60] 的整秒", retry)
	}
	if rid := w.Header().Get("X-Request-ID"); rid == "" {
		t.Fatal("X-Request-ID 缺失")
	}
	envelope := decodeRelayError(t, w.Body.Bytes())
	if envelope.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("code = %q，期望 rate_limit_exceeded", envelope.Error.Code)
	}
	if !strings.HasPrefix(envelope.Error.Message, "rate limit exceeded; retry in "+retry+"s") {
		t.Fatalf("message = %q，期望固定文案且秒数与 Retry-After 一致", envelope.Error.Message)
	}
}

func assertRateLimitCheckFailed500(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("500 不得带 Retry-After（R-M5-10），got %q", got)
	}
	envelope := decodeRelayError(t, w.Body.Bytes())
	if envelope.Error.Code != "rate_limit_check_failed" {
		t.Fatalf("code = %q，期望 rate_limit_check_failed", envelope.Error.Code)
	}
}

// redisValue 返回键的计数值；键不存在时 ok=false（0=不限的桶不得建键）。
func redisValue(t *testing.T, mr *miniredis.Miniredis, key string) (string, bool) {
	t.Helper()
	value, err := mr.Get(key)
	if err != nil {
		return "", false
	}
	return value, true
}

func assertRedisCount(t *testing.T, mr *miniredis.Miniredis, key string, want string) {
	t.Helper()
	got, ok := redisValue(t, mr, key)
	if !ok {
		t.Fatalf("Redis 键 %q 不存在，期望计数 %s", key, want)
	}
	if got != want {
		t.Fatalf("Redis 键 %q = %q，期望 %q", key, got, want)
	}
}

func assertNoRedisKey(t *testing.T, mr *miniredis.Miniredis, key string) {
	t.Helper()
	if got, ok := redisValue(t, mr, key); ok {
		t.Fatalf("Redis 键 %q 不应存在（0=不限/桶被跳过），got %q", key, got)
	}
}

// rateLimitConfig 构造只含 rl 四键的配置快照。
func rateLimitConfig(userModel, token, global int64, overrides string) mapConfig {
	cfg := mapConfig{
		ratelimit.KeyUserModelRPM:  configInt(userModel),
		ratelimit.KeyTokenRPM:      configInt(token),
		ratelimit.KeyUserGlobalRPM: configInt(global),
	}
	if overrides != "" {
		cfg[ratelimit.KeyGroupOverrides] = json.RawMessage(overrides)
	}
	return cfg
}

// ---------- 四个入口：第 N+1 次 429，且零预扣/零上游 ----------

func TestRelayRateLimitChatReturns429BeforeBilling(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(3, 0, 0, "")})
	for i := 0; i < 3; i++ {
		if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	w := env.chat(t, relayTestToken, rateLimitChatBody)
	assertRateLimited429(t, w)

	// 429 在 PreConsume 与上游之前：前 3 次各 1 次预扣/上游，第 4 次两者都不得增加。
	if got := env.billing.preConsumeCount(); got != 3 {
		t.Fatalf("PreConsume = %d，期望 3（429 不得预扣）", got)
	}
	if got := env.upstream.count(); got != 3 {
		t.Fatalf("上游调用 = %d，期望 3（429 不得触达上游）", got)
	}
	assertRedisCount(t, env.mr, ratelimit.UserKey(relayTestUserID), "4")
}

// TestRelayRateLimitAdminIsNotExempt 固化 R-M5-13：管理员令牌同样 429。
func TestRelayRateLimitAdminIsNotExempt(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     rateLimitConfig(1, 0, 0, ""),
		accessRole: "admin",
	})
	if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("第一次 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
}

func TestRelayRateLimitMessagesReturns429(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:      rateLimitConfig(2, 0, 0, ""),
		channelType: contract.ChannelTypeAnthropic,
		model:       messagesTestModel,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, messagesUpstreamBody)
		},
	})
	for i := 0; i < 2; i++ {
		if w := env.messages(t, relayTestToken); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.messages(t, relayTestToken))
	if got := env.billing.preConsumeCount(); got != 2 {
		t.Fatalf("PreConsume = %d，期望 2", got)
	}
	if got := env.upstream.count(); got != 2 {
		t.Fatalf("上游调用 = %d，期望 2", got)
	}
}

func TestRelayRateLimitMultimodalReturns429(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     rateLimitConfig(2, 0, 0, ""),
		multimodal: true,
	})
	for i := 0; i < 2; i++ {
		if w := env.embeddings(t, relayTestToken); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.embeddings(t, relayTestToken))
	if got := env.multimodal.calls; got != 2 {
		t.Fatalf("dispatcher 调用 = %d，期望 2（429 不得进入计费/上游）", got)
	}
	if got := env.upstream.count(); got != 0 {
		t.Fatalf("上游调用 = %d，期望 0（多模态走 fake dispatcher）", got)
	}
}

// TestRelayRateLimitRealtimeIsNotCounted 固化「realtime 不计」：该端点的
// 连接级失败语义由 realtime 自己承担，准入限流接缝不参与（不建任何键）。
// 这里用捕获 dispatcher 让端点本身确定成功，断言只针对限流副作用。
func TestRelayRateLimitRealtimeIsNotCounted(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     rateLimitConfig(1, 1, 1, ""),
		multimodal: true,
	})
	w := env.do(t, http.MethodGet, "/v1/realtime", "",
		map[string]string{"Authorization": "Bearer " + relayTestToken})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（捕获 dispatcher）；body=%s", w.Code, w.Body.String())
	}
	for _, key := range []string{
		ratelimit.UserKey(relayTestUserID),
		ratelimit.TokenKey(relayTestTokenID),
		ratelimit.UserGlobalKey(relayTestUserID),
	} {
		assertNoRedisKey(t, env.mr, key)
	}
}

// ---------- 流式只计一次 ----------

func TestRelayRateLimitStreamCountsOnce(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(2, 0, 0, "")})
	w := env.chat(t, relayTestToken, rateLimitChatStreamBody)
	if w.Code != http.StatusOK {
		t.Fatalf("stream status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "data: ") {
		t.Fatalf("期望 SSE 响应，got %q", w.Body.String())
	}
	// 按帧计数会把一帧算一次（本流 4 帧）；冻结口径是「进入时计一次」。
	assertRedisCount(t, env.mr, ratelimit.UserKey(relayTestUserID), "1")
	if got := env.billing.preConsumeCount(); got != 1 {
		t.Fatalf("PreConsume = %d，期望 1", got)
	}
}

// ---------- /pg：跳过 B11，仍计 B10/B12 ----------

func TestRelayRateLimitPlaygroundSkipsTokenBucket(t *testing.T) {
	// token_rpm=1：Bearer 第 2 次被 B11 拒绝，证明令牌桶确实生效；
	// user_model_rpm=0：会话路径没有 B11 却仍能通过（B10 不限）。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     rateLimitConfig(0, 1, 0, ""),
		playground: true,
	})
	if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("Bearer 第 1 次 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
	assertRedisCount(t, env.mr, ratelimit.TokenKey(relayTestTokenID), "2")

	for i := 0; i < 3; i++ {
		if w := env.playgroundChat(t, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("/pg 第 %d 次 status = %d，期望 200（跳过 B11）；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	// /pg 的会话用户（42）没有令牌维度：三个桶都不应建键（0=不限）。
	assertNoRedisKey(t, env.mr, ratelimit.UserKey(42))
	assertNoRedisKey(t, env.mr, ratelimit.UserGlobalKey(42))
	// 令牌桶键只属于 Bearer 用户，不得出现会话用户的 token 键（TokenID=nil）。
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, ratelimit.TokenKeyPrefix) && strings.HasSuffix(key, ":0") {
			t.Fatalf("出现了 token 0 键（/pg 未跳过 B11）：%q", key)
		}
	}
}

func TestRelayRateLimitPlaygroundCountsUserBuckets(t *testing.T) {
	// B10/B12 各 2：会话路径两次放行、第三次超限；B12 因短路停在 2。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     rateLimitConfig(2, 0, 2, ""),
		playground: true,
	})
	for i := 0; i < 2; i++ {
		if w := env.playgroundChat(t, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("/pg 第 %d 次 status = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.playgroundChat(t, rateLimitChatBody))
	assertRedisCount(t, env.mr, ratelimit.UserKey(42), "3")
	// B12 只在前两次自增；第三次在 B10 就被短路。
	assertRedisCount(t, env.mr, ratelimit.UserGlobalKey(42), "2")
}

// ---------- 分组覆盖 ----------

func TestRelayRateLimitGroupOverrideByUsingGroup(t *testing.T) {
	// 默认 1，default 组覆盖为 3：三次放行、第四次 429。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config: rateLimitConfig(1, 0, 0, `{"default":3}`),
	})
	for i := 0; i < 3; i++ {
		if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200（覆盖值 3）；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
}

func TestRelayRateLimitGroupOverrideAutoUsesFirstGroup(t *testing.T) {
	// auto 令牌解析为首组 vip；覆盖只对 vip 生效（default 保留全局 1）。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config: rateLimitConfig(1, 0, 0, `{"vip":3}`),
		tokens: []rateLimitSeedToken{{
			plain: "nkt-auto-token",
			meta: auth.TokenMeta{
				ID: 9, UserID: relayTestUserID, Status: "enabled",
				Group: "auto", AutoGroups: []string{"vip", "default"}, UnlimitedQuota: true,
			},
		}},
	})
	for i := 0; i < 3; i++ {
		if w := env.chat(t, "nkt-auto-token", rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("auto 第 %d 次 status = %d，期望 200（vip 覆盖 3）；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.chat(t, "nkt-auto-token", rateLimitChatBody))
}

func TestRelayRateLimitGroupOverrideZeroIsUnlimited(t *testing.T) {
	// 覆盖值 0 = 该组不限：连续 5 次全部放行，且 B10 不建键。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config: rateLimitConfig(1, 0, 0, `{"default":0}`),
	})
	for i := 0; i < 5; i++ {
		if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200（覆盖 0=不限）；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertNoRedisKey(t, env.mr, ratelimit.UserKey(relayTestUserID))
}

// ---------- 短路顺序 ----------

func TestRelayRateLimitShortCircuitsDownstreamBuckets(t *testing.T) {
	// B11=1：第二次在令牌桶就被拒，B10/B12 不得自增（R-M5-05）。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(10, 1, 10, "")})
	if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("第 1 次 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
	assertRedisCount(t, env.mr, ratelimit.TokenKey(relayTestTokenID), "2")
	assertRedisCount(t, env.mr, ratelimit.UserKey(relayTestUserID), "1")
	assertRedisCount(t, env.mr, ratelimit.UserGlobalKey(relayTestUserID), "1")
}

func TestRelayRateLimitShortCircuitsGlobalBucket(t *testing.T) {
	// B11=0（不限）、B10=10：第二次在 B10 被拒，B12 停在 1。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(1, 0, 10, "")})
	if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("第 1 次 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
	assertNoRedisKey(t, env.mr, ratelimit.TokenKey(relayTestTokenID))
	assertRedisCount(t, env.mr, ratelimit.UserGlobalKey(relayTestUserID), "1")
}

// ---------- B11 维度隔离 ----------

func TestRelayRateLimitTokenBucketsAreIsolated(t *testing.T) {
	// 同一用户的两个令牌各自计 B11；B10 共用（B10 设为 100 避免干扰）。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config: rateLimitConfig(100, 1, 0, ""),
		tokens: []rateLimitSeedToken{{
			plain: "nkt-second-token",
			meta: auth.TokenMeta{
				ID: 8, UserID: relayTestUserID, Status: "enabled",
				Group: "default", UnlimitedQuota: true,
			},
		}},
	})
	if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("令牌 A 第 1 次 status = %d，期望 200", w.Code)
	}
	// 令牌 B 有自己的 B11 名额，不受令牌 A 已用掉 1 次的影响。
	if w := env.chat(t, "nkt-second-token", rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("令牌 B 第 1 次 status = %d，期望 200（B11 按令牌隔离）；body=%s", w.Code, w.Body.String())
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
	assertRateLimited429(t, env.chat(t, "nkt-second-token", rateLimitChatBody))
	assertRedisCount(t, env.mr, ratelimit.TokenKey(relayTestTokenID), "2")
	assertRedisCount(t, env.mr, ratelimit.TokenKey(8), "2")
	// B10 是用户维：令牌 A/B 的 4 次（含被 B11 拒绝的 2 次？不——短路的 2 次
	// 只计到 B11）→ 这里只能确认它存在且未被令牌维度污染。
	if got, ok := redisValue(t, env.mr, ratelimit.UserKey(relayTestUserID)); !ok || got != "2" {
		t.Fatalf("B10 计数 = %q（存在=%v），期望 2（两次放行各一次）", got, ok)
	}
}

// ---------- 0 = 不限 / 缺键 ----------

func TestRelayRateLimitAllZeroKeepsRelayBehavior(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(0, 0, 0, "")})
	for i := 0; i < 5; i++ {
		if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200（全 0 不限）；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	if keys := env.mr.Keys(); len(keys) != 0 {
		t.Fatalf("全 0 配置不得访问 Redis 建键，got %v", keys)
	}
	if got := env.upstream.count(); got != 5 {
		t.Fatalf("上游调用 = %d，期望 5", got)
	}
}

func TestRelayRateLimitMissingConfigUsesRegistryDefaults(t *testing.T) {
	// 缺键 → 注册表默认 60/0/0（R-M5-11）：只启用 B10，且不把缺键读成 0=不限。
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: mapConfig{}})
	for i := 0; i < 60; i++ {
		if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200（默认 60）；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.chat(t, relayTestToken, rateLimitChatBody))
	assertNoRedisKey(t, env.mr, ratelimit.TokenKey(relayTestTokenID))
	assertNoRedisKey(t, env.mr, ratelimit.UserGlobalKey(relayTestUserID))
}

// ---------- Redis 故障：fail-closed 500 ----------

func TestRelayRateLimitRedisFailureIsFailClosed(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(10, 10, 10, "")})
	env.mr.Close()
	assertRateLimitCheckFailed500(t, env.chat(t, relayTestToken, rateLimitChatBody))
	if got := env.upstream.count(); got != 0 {
		t.Fatalf("上游调用 = %d，期望 0", got)
	}
	if got := env.billing.preConsumeCount(); got != 0 {
		t.Fatalf("PreConsume = %d，期望 0", got)
	}
}

// ---------- 任务提交面 ----------

type rateLimitTaskEnv struct {
	t        *testing.T
	mux      *http.ServeMux
	mr       *miniredis.Miniredis
	billing  *rateLimitTaskBilling
	upstream *rateLimitUpstream
	store    *fakeTaskStore
}

// rateLimitTaskBilling 与 rateLimitBilling 同口径：每个提交一个独立会话，
// 只聚合预扣次数（任务提交的成功状态是 202）。
type rateLimitTaskBilling struct {
	mu          sync.Mutex
	preConsumed int
}

func (b *rateLimitTaskBilling) factory(*contract.RelayContext) contract.BillingSession {
	return &rateLimitTaskBillingSession{taskBillingRecorder: newTaskBillingRecorder(), parent: b}
}

func (b *rateLimitTaskBilling) preConsumeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.preConsumed
}

type rateLimitTaskBillingSession struct {
	*taskBillingRecorder
	parent *rateLimitTaskBilling
}

func (s *rateLimitTaskBillingSession) PreConsume(est contract.QuotaEstimate) error {
	s.parent.mu.Lock()
	s.parent.preConsumed++
	s.parent.mu.Unlock()
	return s.taskBillingRecorder.PreConsume(est)
}

func newRateLimitTaskEnv(t *testing.T, config mapConfig, upstreamHandler http.HandlerFunc) *rateLimitTaskEnv {
	t.Helper()
	if upstreamHandler == nil {
		upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		}
	}
	upstream := newRateLimitUpstream(t, upstreamHandler)
	sealer := NewSecretSealer("task-handler-test-key")
	store := &fakeTaskStore{}
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 1, BaseURL: upstream.server.URL, Credentials: sealedCredential(t, sealer, "sk"),
	}}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	billing := &rateLimitTaskBilling{}
	handler := NewTasksHandler(TasksHandlerConfig{
		Store:                store,
		Channels:             channels,
		Plugins:              sampleInlineManager(t),
		Sealer:               sealer,
		Tokens:               &fakeTaskTokens{meta: &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}},
		Billing:              billing.factory,
		ArtifactSigner:       tasks.NewArtifactSigner("task-handler-test-key", 0),
		AllowPrivateUpstream: true,
		RateLimiter:          ratelimit.New(rdb),
		Config:               config,
	})
	mux := http.NewServeMux()
	fakeTokens := &fakeTaskTokens{meta: &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}}
	handler.RegisterRelay(mux, RelayAuth(fakeTokens, fakeTaskAccess{}, nil))
	return &rateLimitTaskEnv{t: t, mux: mux, mr: mr, billing: billing, upstream: upstream, store: store}
}

func (e *rateLimitTaskEnv) submit(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, newTaskSubmitHTTPReq(`{"model":"sample-model"}`))
	return rec
}

// taskPreConsumeCount 统计预扣调用次数（rateLimitTaskBilling 的聚合计数）。
func taskPreConsumeCount(b *rateLimitTaskBilling) int { return b.preConsumeCount() }

func TestRelayRateLimitTaskSubmitReturns429BeforeBilling(t *testing.T) {
	env := newRateLimitTaskEnv(t, rateLimitConfig(2, 0, 0, ""), nil)
	for i := 0; i < 2; i++ {
		if w := env.submit(t); w.Code != http.StatusAccepted {
			t.Fatalf("第 %d 次提交 status = %d，期望 202；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	assertRateLimited429(t, env.submit(t))
	if got := taskPreConsumeCount(env.billing); got != 2 {
		t.Fatalf("PreConsume = %d，期望 2（429 不得预扣）", got)
	}
	if got := env.upstream.count(); got != 2 {
		t.Fatalf("上游调用 = %d，期望 2（429 不得提交上游）", got)
	}
	if env.store.inserted != 2 {
		t.Fatalf("任务行 = %d，期望 2（429 不得落库）", env.store.inserted)
	}
	assertRedisCount(t, env.mr, ratelimit.UserKey(42), "3")
}

func TestRelayRateLimitTaskQueryIsNotCounted(t *testing.T) {
	env := newRateLimitTaskEnv(t, rateLimitConfig(1, 1, 1, ""), nil)
	// 查询/轮询不计入（R-M5-07）：先耗尽不可能，直接查一个不存在的任务。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/sample?task_id=task_missing", nil)
	req.Header.Set("Authorization", "Bearer nkt-test-token")
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("查询 status = %d，期望 404；body=%s", rec.Code, rec.Body.String())
	}
	if keys := env.mr.Keys(); len(keys) != 0 {
		t.Fatalf("任务查询不得创建限流键，got %v", keys)
	}
	// 提交仍照常计入：1 次放行、第 2 次 429（证明桶本身是通的）。
	if w := env.submit(t); w.Code != http.StatusAccepted {
		t.Fatalf("首次提交 status = %d，期望 202；body=%s", w.Code, w.Body.String())
	}
	assertRateLimited429(t, env.submit(t))
}

func TestRelayRateLimitTaskRedisFailureIsFailClosed(t *testing.T) {
	env := newRateLimitTaskEnv(t, rateLimitConfig(10, 10, 10, ""), nil)
	env.mr.Close()
	assertRateLimitCheckFailed500(t, env.submit(t))
	if got := taskPreConsumeCount(env.billing); got != 0 {
		t.Fatalf("PreConsume = %d，期望 0", got)
	}
	if got := env.upstream.count(); got != 0 {
		t.Fatalf("上游调用 = %d，期望 0", got)
	}
	if env.store.inserted != 0 {
		t.Fatalf("任务行 = %d，期望 0", env.store.inserted)
	}
}

// ---------- 入口级 fail-closed（M5-b 复核 F6） ----------

// TestRelayRateLimitMessagesRedisFailureIsFailClosed 与 chat 同口径：Messages
// 入口在限流器故障时也必须 500，而不是把这一个入口留成 fail-open。
func TestRelayRateLimitMessagesRedisFailureIsFailClosed(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:      rateLimitConfig(10, 10, 10, ""),
		channelType: contract.ChannelTypeAnthropic,
		model:       messagesTestModel,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, messagesUpstreamBody)
		},
	})
	env.mr.Close()
	assertRateLimitCheckFailed500(t, env.messages(t, relayTestToken))
	if got := env.upstream.count(); got != 0 {
		t.Fatalf("上游调用 = %d，期望 0", got)
	}
	if got := env.billing.preConsumeCount(); got != 0 {
		t.Fatalf("PreConsume = %d，期望 0", got)
	}
}

// TestRelayRateLimitMultimodalRedisFailureIsFailClosed 锁定多模态入口的
// fail-closed：共享同一 helper 也不能靠「公共代码正确」代替入口级断言。
func TestRelayRateLimitMultimodalRedisFailureIsFailClosed(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     rateLimitConfig(10, 10, 10, ""),
		multimodal: true,
	})
	env.mr.Close()
	assertRateLimitCheckFailed500(t, env.embeddings(t, relayTestToken))
	if got := env.multimodal.calls; got != 0 {
		t.Fatalf("dispatcher 调用 = %d，期望 0", got)
	}
}

// ---------- 429 日志与响应使用同一个 request id（M5-b 复核 F3/F6） ----------

// captureDefaultSlog 把默认 logger 指到内存 buffer；t.Cleanup 恢复。
// handler 用 slog.Default()，因此不需要为日志另开注入缝。
func captureDefaultSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestRelayRateLimit429LogsInfoWithConsistentRequestID(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: rateLimitConfig(1, 0, 0, "")})
	if w := env.chat(t, relayTestToken, rateLimitChatBody); w.Code != http.StatusOK {
		t.Fatalf("第 1 次 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	logs := captureDefaultSlog(t)
	w := env.chat(t, relayTestToken, rateLimitChatBody)
	assertRateLimited429(t, w)

	requestID := w.Header().Get("X-Request-ID")
	line := logs.String()
	if !strings.Contains(line, "level=INFO") {
		t.Fatalf("429 必须以 INFO 记录（R-M5-15），got %q", line)
	}
	if !strings.Contains(line, `msg="relay rate limit exceeded"`) {
		t.Fatalf("缺少限流日志，got %q", line)
	}
	// 日志里的 request_id 与响应的 X-Request-ID 必须是同一个值（F3）。
	if !strings.Contains(line, "request_id="+requestID) {
		t.Fatalf("日志 request_id 与响应 X-Request-ID（%s）不一致：%q", requestID, line)
	}
	if !strings.Contains(line, "status_code=429") {
		t.Fatalf("日志缺少 status_code=429：%q", line)
	}
	// 响应正文的 requestId 也来自同一个值。
	if !strings.Contains(w.Body.String(), "(requestId: "+requestID+")") {
		t.Fatalf("响应 requestId 与 X-Request-ID（%s）不一致：%s", requestID, w.Body.String())
	}
}

// ---------- 批次：每行一次（M5-b 复核 F4） ----------

// TestRelayRateLimitBatchRowsAreCountedIndividually 记录现实口径：批次不是
// 「一次请求一个名额」，而是**每行一个名额**（每行都是一个独立任务提交，
// 独立预扣、独立上游调用）。超限的尾部行提交失败并被计入 failed，父批次本身
// 仍作为聚合视图返回 200（handleBatchesSubmit 的实际状态码；不是 202）。
func TestRelayRateLimitBatchRowsAreCountedIndividually(t *testing.T) {
	manager := newBatchManager(t, map[string]string{
		"fast": videoPluginSource("fast", "fast-model", true),
	})
	hits := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"up_1"}`)
	}))
	t.Cleanup(upstream.Close)
	sealer := NewSecretSealer("task-handler-test-key")
	store := newFakeBatchTaskStore()
	channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
		ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk-test"),
	}}
	billing := &billingCallLog{}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	handler := NewTasksHandler(TasksHandlerConfig{
		Store:    store,
		Channels: channels,
		Plugins:  manager,
		Sealer:   sealer,
		Tokens:   &fakeTaskTokens{meta: &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}},
		Billing: func(*contract.RelayContext) contract.BillingSession {
			return &perRequestBilling{Stub: billingseam.NewStub(), log: billing}
		},
		ArtifactSigner:       tasks.NewArtifactSigner("task-handler-test-key", 0),
		AllowPrivateUpstream: true,
		RateLimiter:          ratelimit.New(rdb),
		Config:               rateLimitConfig(2, 0, 0, ""),
	})
	mux := newTaskRelayMux(handler)

	body := `{"endpoint":"/v1/videos","completion_window":"24h","requests":[` +
		`{"custom_id":"r1","method":"POST","url":"/v1/videos","body":{"model":"fast-model","seconds":5}},` +
		`{"custom_id":"r2","method":"POST","url":"/v1/videos","body":{"model":"fast-model","seconds":5}},` +
		`{"custom_id":"r3","method":"POST","url":"/v1/videos","body":{"model":"fast-model","seconds":5}}]}`
	rec := postBatch(t, mux, body)
	if rec.Code != http.StatusOK {
		// 父批次是聚合视图：单行失败不改变批次创建的 200。
		t.Fatalf("批次 status = %d，期望 200（父批次聚合）；body=%s", rec.Code, rec.Body.String())
	}
	var created batchObject
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode batch: %v; body=%s", err, rec.Body.String())
	}
	if created.RequestCounts.Total != 3 || created.RequestCounts.Completed != 2 || created.RequestCounts.Failed != 1 {
		t.Fatalf("request_counts = %+v，期望 total=3 completed=2 failed=1（尾部行被 429）",
			created.RequestCounts)
	}
	// fakeBatchTaskStore 自身也有 inserted（批次父行数），任务行数看内嵌的
	// fakeTaskStore（第 3 行 429 不落库、不预扣、不触达上游）。
	if got := store.fakeTaskStore.inserted; got != 2 {
		t.Fatalf("任务行 = %d，期望 2（第 3 行 429 不落库）", got)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("上游调用 = %d，期望 2（第 3 行未触达上游）", got)
	}
	preConsumed := 0
	for _, call := range billing.snapshot() {
		if call == "preconsume" {
			preConsumed++
		}
	}
	if preConsumed != 2 {
		t.Fatalf("PreConsume = %d，期望 2（第 3 行未预扣）", preConsumed)
	}
	assertRedisCount(t, mr, ratelimit.UserKey(42), "3")
}
