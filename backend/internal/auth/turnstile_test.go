package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/clientip"
)

// 本文件是 Task 13 的 Turnstile 单元验收（F-01-26）。校验服务用 httptest
// 桩替代，重放守卫用 miniredis：全程不访问外网（GC-8）。

// turnstileStub 模拟 Cloudflare siteverify。默认对任意 token 返回 success，
// 便于单独验证服务端的「一次性 + 接入点绑定」逻辑。
type turnstileStub struct {
	mu       sync.Mutex
	success  bool
	status   int
	hits     int
	lastForm url.Values
}

func newTurnstileStub(t *testing.T) (*turnstileStub, *httptest.Server) {
	t.Helper()
	stub := &turnstileStub{success: true, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.hits++
		stub.lastForm = r.PostForm
		status, success := stub.status, stub.success
		stub.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "stub failure", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": success})
	}))
	t.Cleanup(srv.Close)
	return stub, srv
}

func (s *turnstileStub) setSuccess(v bool) {
	s.mu.Lock()
	s.success = v
	s.mu.Unlock()
}

func (s *turnstileStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *turnstileStub) form() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastForm
}

func newTurnstileTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// countingHandler 记录是否被调用，供「拒绝时不得放行下游」断言。
type countingHandler struct {
	calls int
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls++
	w.WriteHeader(http.StatusNoContent)
}

func turnstileConfig(endpoint, verifyURL string, settings TurnstileSettings, rdb *redis.Client) TurnstileMiddlewareConfig {
	return TurnstileMiddlewareConfig{
		Endpoint:   endpoint,
		Settings:   func() TurnstileSettings { return settings },
		VerifyURL:  verifyURL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		Redis:      rdb,
	}
}

func postTurnstile(t *testing.T, h http.Handler, endpoint, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{}
	if token != "" {
		body["turnstile_token"] = token
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:4321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestTurnstileMissingTokenRejected 覆盖 TC-F-01-26-01：开启时无令牌直接拒绝，
// 下游 handler 不得执行，也不得先花一次外部校验成本。
func TestTurnstileMissingTokenRejected(t *testing.T) {
	stub, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)
	next := &countingHandler{}
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}

	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointLogin, srv.URL, settings, rdb))(next)
	rr := postTurnstile(t, h, "/api/auth/login", "")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("无令牌状态码 = %d，期望 403", rr.Code)
	}
	if next.calls != 0 {
		t.Fatal("无令牌时不得放行下游 handler")
	}
	if stub.hitCount() != 0 {
		t.Fatal("无令牌时不得调用 siteverify")
	}
	if !strings.Contains(rr.Body.String(), "turnstile_challenge_failed") {
		t.Fatalf("错误体应携带 turnstile_challenge_failed：%s", rr.Body.String())
	}
}

// TestTurnstileCrossEndpointReuseRejected 覆盖 TC-F-01-26-02：同一 token
// 先在登录接入点成功消费后，在注册接入点必须被拒（即使桩校验服务仍返回 success）。
func TestTurnstileCrossEndpointReuseRejected(t *testing.T) {
	_, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}

	loginNext := &countingHandler{}
	login := TurnstileMiddleware(turnstileConfig(TurnstileEndpointLogin, srv.URL, settings, rdb))(loginNext)
	registerNext := &countingHandler{}
	register := TurnstileMiddleware(turnstileConfig(TurnstileEndpointRegister, srv.URL, settings, rdb))(registerNext)

	if rr := postTurnstile(t, login, "/api/auth/login", "token-once"); rr.Code != http.StatusNoContent {
		t.Fatalf("首次使用状态码 = %d，期望放行", rr.Code)
	}
	if loginNext.calls != 1 {
		t.Fatalf("首次使用下游调用次数 = %d，期望 1", loginNext.calls)
	}

	if rr := postTurnstile(t, register, "/api/auth/register", "token-once"); rr.Code != http.StatusForbidden {
		t.Fatalf("跨接入点复用状态码 = %d，期望 403", rr.Code)
	}
	if registerNext.calls != 0 {
		t.Fatal("跨接入点复用时不得放行下游 handler")
	}

	if rr := postTurnstile(t, register, "/api/auth/register", "token-fresh"); rr.Code != http.StatusNoContent {
		t.Fatalf("新令牌状态码 = %d，期望放行", rr.Code)
	}
}

// TestTurnstileServiceUnreachableFailClosed 覆盖 TC-F-01-26-03：
// 校验服务不可达时拒绝而不是放行（fail-closed），并给出可重试提示。
func TestTurnstileServiceUnreachableFailClosed(t *testing.T) {
	_, srv := newTurnstileStub(t)
	verifyURL := srv.URL
	srv.Close() // 连接被拒 → http.Client 返回错误

	_, rdb := newTurnstileTestRedis(t)
	next := &countingHandler{}
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointLogin, verifyURL, settings, rdb))(next)

	rr := postTurnstile(t, h, "/api/auth/login", "token-x")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("校验服务不可达状态码 = %d，期望 503（fail-closed）", rr.Code)
	}
	if next.calls != 0 {
		t.Fatal("校验服务不可达时不得放行下游 handler")
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 应携带 Retry-After 提示")
	}
}

// TestTurnstileVerifyFailureRejected 覆盖校验服务返回 success=false。
func TestTurnstileVerifyFailureRejected(t *testing.T) {
	stub, srv := newTurnstileStub(t)
	stub.setSuccess(false)
	_, rdb := newTurnstileTestRedis(t)
	next := &countingHandler{}
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointCheckin, srv.URL, settings, rdb))(next)

	rr := postTurnstile(t, h, "/api/checkin", "token-bad")
	if rr.Code != http.StatusForbidden || next.calls != 0 {
		t.Fatalf("校验失败应 403 且不放行，实际 status=%d calls=%d", rr.Code, next.calls)
	}
}

// TestTurnstileDisabledPassesThrough 覆盖 TC-F-01-26-05 的开关关闭面：
// 5 处接入点在关闭时全部直接放行，且不触碰校验服务（防刷由外层限流兜底）。
func TestTurnstileDisabledPassesThrough(t *testing.T) {
	stub, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)
	settings := TurnstileSettings{Enabled: false}

	cases := []struct {
		endpoint string
		method   string
		path     string
	}{
		{TurnstileEndpointVerification, http.MethodGet, "/api/verification"},
		{TurnstileEndpointResetPassword, http.MethodGet, "/api/reset_password"},
		{TurnstileEndpointRegister, http.MethodPost, "/api/auth/register"},
		{TurnstileEndpointLogin, http.MethodPost, "/api/auth/login"},
		{TurnstileEndpointCheckin, http.MethodPost, "/api/checkin"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			next := &countingHandler{}
			h := TurnstileMiddleware(turnstileConfig(tc.endpoint, srv.URL, settings, rdb))(next)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent || next.calls != 1 {
				t.Fatalf("关闭时应直接放行，实际 status=%d calls=%d", rr.Code, next.calls)
			}
		})
	}
	if stub.hitCount() != 0 {
		t.Fatal("关闭时不得调用 siteverify")
	}
}

// TestTurnstileMissingSecretRejected 覆盖 TC-F-01-26-04：开启但密钥未配置
// 属配置错误，必须拒绝并告警，绝不静默放行。
func TestTurnstileMissingSecretRejected(t *testing.T) {
	stub, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)
	next := &countingHandler{}
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: ""}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointRegister, srv.URL, settings, rdb))(next)

	rr := postTurnstile(t, h, "/api/auth/register", "token-x")
	if rr.Code != http.StatusForbidden || next.calls != 0 {
		t.Fatalf("缺 secret 应 403 且不放行，实际 status=%d calls=%d", rr.Code, next.calls)
	}
	if stub.hitCount() != 0 {
		t.Fatal("缺 secret 时不得调用 siteverify")
	}
}

// TestTurnstileBodyPreservedAndRemoteIPForwarded 证明中间件读取 JSON 请求体
// 后必须完整还原（下游 handler 仍要能解析），且 remoteip 取连接地址而非
// 可伪造的 X-Forwarded-For。
func TestTurnstileBodyPreservedAndRemoteIPForwarded(t *testing.T) {
	stub, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)

	var downstreamBody map[string]any
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&downstreamBody); err != nil {
			t.Errorf("下游读取请求体失败：%v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointLogin, srv.URL, settings, rdb))(next)

	raw := `{"identifier":"alice","password":"x","turnstile_token":"token-body"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.RemoteAddr = "203.0.113.9:4321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("合法令牌状态码 = %d，期望放行", rr.Code)
	}
	if downstreamBody["turnstile_token"] != "token-body" || downstreamBody["identifier"] != "alice" {
		t.Fatalf("下游请求体被破坏：%#v", downstreamBody)
	}
	form := stub.form()
	if form.Get("response") != "token-body" {
		t.Fatalf("siteverify response = %q", form.Get("response"))
	}
	if form.Get("remoteip") != "203.0.113.9" {
		t.Fatalf("remoteip = %q，期望连接地址 203.0.113.9（不得信任 X-Forwarded-For）", form.Get("remoteip"))
	}
	if form.Get("secret") != "secret-key" {
		t.Fatalf("siteverify secret = %q", form.Get("secret"))
	}
}

// TestTurnstileRemoteIPHonorsTrustedProxies：边缘部署（TRUSTED_PROXIES 已配置）
// 时 remoteip 必须与限流/审计使用同一解析器，取真实客户端而非 edge IP。
func TestTurnstileRemoteIPHonorsTrustedProxies(t *testing.T) {
	t.Cleanup(func() { _ = clientip.Configure("") })
	if err := clientip.Configure("10.0.0.0/8"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	stub, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)

	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointLogin, srv.URL, settings, rdb))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	raw := `{"identifier":"alice","password":"x","turnstile_token":"token-edge"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.RemoteAddr = "10.0.0.2:4321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("合法令牌状态码 = %d，期望放行（body=%q）", rr.Code, rr.Body.String())
	}
	if got := stub.form().Get("remoteip"); got != "198.51.100.7" {
		t.Fatalf("remoteip = %q，期望可信代理解析后的客户端 198.51.100.7", got)
	}
}

// TestTurnstileQueryToken 覆盖 GET 接入点从查询参数取 turnstile_token。
func TestTurnstileQueryToken(t *testing.T) {
	_, srv := newTurnstileStub(t)
	_, rdb := newTurnstileTestRedis(t)
	next := &countingHandler{}
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointVerification, srv.URL, settings, rdb))(next)

	req := httptest.NewRequest(http.MethodGet, "/api/verification?turnstile_token=token-query", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || next.calls != 1 {
		t.Fatalf("查询参数令牌应放行，实际 status=%d calls=%d", rr.Code, next.calls)
	}
}

// TestTurnstileReplayGuardUnavailableFailClosed：一次性保证无法落库
// （Redis 不可用）时必须拒绝，而不是退化为无重放防护。
func TestTurnstileReplayGuardUnavailableFailClosed(t *testing.T) {
	_, srv := newTurnstileStub(t)
	mr, rdb := newTurnstileTestRedis(t)
	mr.Close()
	next := &countingHandler{}
	settings := TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	h := TurnstileMiddleware(turnstileConfig(TurnstileEndpointLogin, srv.URL, settings, rdb))(next)

	rr := postTurnstile(t, h, "/api/auth/login", "token-x")
	if rr.Code != http.StatusServiceUnavailable || next.calls != 0 {
		t.Fatalf("重放守卫不可用应 503 且不放行，实际 status=%d calls=%d", rr.Code, next.calls)
	}
}

// TestTurnstileEndpointConstants 固化 5 个接入点的冻结名称（wiring 用）。
func TestTurnstileEndpointConstants(t *testing.T) {
	got := []string{
		TurnstileEndpointVerification,
		TurnstileEndpointResetPassword,
		TurnstileEndpointRegister,
		TurnstileEndpointLogin,
		TurnstileEndpointCheckin,
	}
	want := []string{"verification", "reset_password", "register", "login", "checkin"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("接入点名称[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}
