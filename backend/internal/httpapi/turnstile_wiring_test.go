package httpapi

// Turnstile 接线与顺序验收（modules-01 §3.15）：
// 冻结顺序是「限流（外层）→ Turnstile（内层）」。登录/注册的速率桶在
// handler 内部，若人机校验先于限流，攻击者可用无效令牌无限消耗 siteverify
// 配额。本测试用请求计数证明第 N+1 个请求被限流拦截时 siteverify 不再被调用。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"newtg/gateway/internal/auth"
)

// countingTurnstile 是 siteverify 桩：始终成功并统计调用次数。
func countingTurnstile(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func depsWithTurnstile(t *testing.T, verifyURL string) GatewayDeps {
	t.Helper()
	deps, _ := gatewayDepsForTest(t)
	deps.TurnstileSettings = func() auth.TurnstileSettings {
		return auth.TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	}
	deps.TurnstileVerifyURL = verifyURL
	return deps
}

// TestTurnstileRunsAfterLoginRateLimit：B1 登录速率桶（5 次/分钟）先于
// Turnstile；第 6 个请求返回 429 且不再调用 siteverify。
func TestTurnstileRunsAfterLoginRateLimit(t *testing.T) {
	srv, calls := countingTurnstile(t)
	rt, err := BuildGatewayRouter(depsWithTurnstile(t, srv.URL))
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}

	for i := 0; i < LoginRateLimit; i++ {
		body := `{"identifier":"nobody","turnstile_token":"tok-` + string(rune('a'+i)) + `"}`
		rr := doRawRequest(rt, http.MethodPost, "/api/auth/login", body, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次登录 = %d, want 401 (body=%q)", i+1, rr.Code, rr.Body.String())
		}
	}
	if got := calls.Load(); got != LoginRateLimit {
		t.Fatalf("siteverify calls = %d, want %d", got, LoginRateLimit)
	}

	rr := doRawRequest(rt, http.MethodPost, "/api/auth/login",
		`{"identifier":"nobody","turnstile_token":"tok-over"}`, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("超限登录 = %d, want 429 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != LoginRateLimit {
		t.Fatalf("被限流后 siteverify calls = %d, want %d（Turnstile 先于限流执行）",
			got, LoginRateLimit)
	}
}

// TestTurnstileRunsAfterRegisterRateLimit：B3 注册速率桶（3 次/分钟）先于
// Turnstile；第 4 个请求返回 429 且不再调用 siteverify。
func TestTurnstileRunsAfterRegisterRateLimit(t *testing.T) {
	srv, calls := countingTurnstile(t)
	deps := depsWithTurnstile(t, srv.URL)
	rt, err := BuildGatewayRouter(deps)
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}

	for i := 0; i < RegisterRateLimit; i++ {
		body := `{"turnstile_token":"rtok-` + string(rune('a'+i)) + `"}`
		rr := doRawRequest(rt, http.MethodPost, "/api/auth/register", body, nil)
		// 人机校验通过后由业务校验拒绝非法用户名（400）。
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次注册 = %d, want 400 (body=%q)", i+1, rr.Code, rr.Body.String())
		}
	}
	if got := calls.Load(); got != RegisterRateLimit {
		t.Fatalf("siteverify calls = %d, want %d", got, RegisterRateLimit)
	}

	rr := doRawRequest(rt, http.MethodPost, "/api/auth/register",
		`{"turnstile_token":"rtok-over"}`, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("超限注册 = %d, want 429 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != RegisterRateLimit {
		t.Fatalf("被限流后 siteverify calls = %d, want %d（Turnstile 先于限流执行）",
			got, RegisterRateLimit)
	}
}

// TestTurnstileMissingTokenRejectedAfterRateLimit：开启时缺令牌必须在
// 通过限流后被人机校验拒绝（403），而不是进入业务逻辑。
func TestTurnstileMissingTokenRejectedAfterRateLimit(t *testing.T) {
	srv, calls := countingTurnstile(t)
	rt, err := BuildGatewayRouter(depsWithTurnstile(t, srv.URL))
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}
	rr := doRawRequest(rt, http.MethodPost, "/api/auth/login",
		`{"identifier":"nobody"}`, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺令牌登录 = %d, want 403 (body=%q)", rr.Code, rr.Body.String())
	}
	// 缺令牌在本地即被拒绝，不消耗 siteverify。
	if got := calls.Load(); got != 0 {
		t.Fatalf("缺令牌时 siteverify calls = %d, want 0", got)
	}
}

// TestTurnstileQueryOnlyTokenIsPreserved：客户端只通过查询参数提交
// ?turnstile_token=… 时，gate 不得用请求体里的空 token 覆盖它。
func TestTurnstileQueryOnlyTokenIsPreserved(t *testing.T) {
	srv, calls := countingTurnstile(t)
	rt, err := BuildGatewayRouter(depsWithTurnstile(t, srv.URL))
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}
	rr := doRawRequest(rt, http.MethodPost,
		"/api/auth/login?turnstile_token=query-only", `{"identifier":"nobody"}`, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("仅查询参数登录 = %d, want 401（人机校验应通过）body=%q",
			rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("siteverify calls = %d, want 1（查询参数令牌被覆盖）", got)
	}
}

// TestTurnstileRunsAfterEmailCodeRateLimit：发码端点的同邮箱冷却桶先于
// Turnstile；冷却中的第二次请求 429 且不再调用 siteverify。
func TestTurnstileRunsAfterEmailCodeRateLimit(t *testing.T) {
	srv, calls := countingTurnstile(t)
	rt, err := BuildGatewayRouter(depsWithTurnstile(t, srv.URL))
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}
	body := `{"email":"rate@example.com","purpose":"register","turnstile_token":"ec-1"}`
	if rr := doRawRequest(rt, http.MethodPost, "/api/auth/email-code", body, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("首次发码 = %d, want 204 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("siteverify calls = %d, want 1", got)
	}

	body = `{"email":"rate@example.com","purpose":"register","turnstile_token":"ec-2"}`
	if rr := doRawRequest(rt, http.MethodPost, "/api/auth/email-code", body, nil); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("冷却期发码 = %d, want 429 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("被限流后 siteverify calls = %d, want 1（Turnstile 先于限流执行）", got)
	}
}

// TestTurnstileRunsAfterPasswordResetRateLimit：找回密码的同邮箱冷却桶先于
// Turnstile；冷却中的第二次请求 429 且不再调用 siteverify。
func TestTurnstileRunsAfterPasswordResetRateLimit(t *testing.T) {
	srv, calls := countingTurnstile(t)
	rt, err := BuildGatewayRouter(depsWithTurnstile(t, srv.URL))
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}
	body := `{"email":"reset-rate@example.com","turnstile_token":"pr-1"}`
	if rr := doRawRequest(rt, http.MethodPost, "/api/auth/password-reset/request", body, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("首次重置请求 = %d, want 204 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("siteverify calls = %d, want 1", got)
	}

	body = `{"email":"reset-rate@example.com","turnstile_token":"pr-2"}`
	if rr := doRawRequest(rt, http.MethodPost, "/api/auth/password-reset/request", body, nil); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("冷却期重置请求 = %d, want 429 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("被限流后 siteverify calls = %d, want 1（Turnstile 先于限流执行）", got)
	}
}

// TestTurnstileUnknownEndpointFailsClosed 覆盖 C3：未知接入点必须写 403
// problem，而不是返回 false 让 handler 落成空 200。正确装配下不可达，
// 本测试钉死纵深防御行为。
func TestTurnstileUnknownEndpointFailsClosed(t *testing.T) {
	gates := newTurnstileGates(func() auth.TurnstileSettings {
		return auth.TurnstileSettings{Enabled: true, SiteKey: "k", SecretKey: "s"}
	}, nil, "", nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/checkin", nil)
	if gates.check("checkin", rr, req, "") {
		t.Fatal("未知接入点不得放行")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	var p map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem JSON 非法: %v", err)
	}
	if p["type"] != problemTypeTurnstileFailed {
		t.Fatalf("problem.type = %v，期望 %s", p["type"], problemTypeTurnstileFailed)
	}
}
