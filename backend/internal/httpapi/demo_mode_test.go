package httpapi

// W13-DEMO 的单元验收：演示站只读守卫（modules-08 §3.17 / F-08-46）与
// 中继两层日额度（§3.17.2 / N-33 / G7-R48）。
//
// 覆盖：
//   - 开关关闭：写方法与中继行为完全不变（零新拒绝）。
//   - 开启：管理面写 403 固定 problem；读 200；登录/登出/找回密码/2FA
//     第二步与支付回调放行；注册被拒（换号无法绕过额度）。
//   - 中继：每 IP 第 21 次 429、全站第 2001 次 429、另一 IP 也被拒；
//     Retry-After 与固定 code；0 额度 = 中继整体关闭（403）；
//     Redis 故障 fail-closed 503，且被拒请求零预扣/零上游。
//   - 与 B10/B11/B12 叠加：先到者拒，互不覆盖。
//   - 演示账号：unlimited_quota 只解除余额门，不豁免两个计数器
//     （设计原文「兜底全在这两个计数器里」）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	platformconfig "newtg/gateway/internal/config"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/tasks"
)

// demoProblemBody 是守卫拒绝的完整 problem 体（比现有 decodeProblem 多 detail）。
type demoProblemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func decodeDemoProblem(t *testing.T, rr *httptest.ResponseRecorder) demoProblemBody {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	var body demoProblemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem 体不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return body
}

// demoGuardTestHandler 记录是否被调用；被守卫放行时返回 204。
func demoGuardTestHandler(called *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
}

func demoConfig(enabled bool) mapConfig {
	return mapConfig{keyDemoEnabled: configBool(enabled)}
}

func TestDemoReadOnlyGuardDisabledIsPassthrough(t *testing.T) {
	var called atomic.Bool
	h := DemoReadOnlyGuard(demoConfig(false))(demoGuardTestHandler(&called))
	rr := serve(h, httptest.NewRequest(http.MethodPost, "/api/settings", nil))
	if rr.Code != http.StatusNoContent || !called.Load() {
		t.Fatalf("关闭时 POST = %d called=%v，期望透传 204", rr.Code, called.Load())
	}
}

func TestDemoReadOnlyGuardNilConfigIsPassthrough(t *testing.T) {
	var called atomic.Bool
	h := DemoReadOnlyGuard(nil)(demoGuardTestHandler(&called))
	rr := serve(h, httptest.NewRequest(http.MethodDelete, "/api/tokens/1", nil))
	if rr.Code != http.StatusNoContent || !called.Load() {
		t.Fatalf("nil config POST = %d called=%v，期望透传 204", rr.Code, called.Load())
	}
}

func TestDemoReadOnlyGuardAllowsSafeMethods(t *testing.T) {
	// TRACE 是 RFC 9110 的安全方法，必须与 GET/HEAD/OPTIONS 同等放行。
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace} {
		t.Run(method, func(t *testing.T) {
			var called atomic.Bool
			h := DemoReadOnlyGuard(demoConfig(true))(demoGuardTestHandler(&called))
			rr := serve(h, httptest.NewRequest(method, "/api/settings", nil))
			if !called.Load() {
				t.Fatalf("%s 被误拦：status=%d body=%s", method, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestDemoReadOnlyGuardDeniesManagementWrites 覆盖 Q1 的「绕过前端直 POST 仍 403」。
func TestDemoReadOnlyGuardDeniesManagementWrites(t *testing.T) {
	cases := []struct {
		method string
		path   string
		family string
	}{
		{http.MethodPost, "/api/settings", "settings"},
		{http.MethodPut, "/api/users/1", "users"},
		{http.MethodPost, "/api/tokens", "tokens"},
		{http.MethodPost, "/api/orders", "orders"},
		{http.MethodDelete, "/api/channels/7", "channels"},
		{http.MethodPatch, "/api/me", "me"},
		{http.MethodPost, "/api/usage/logs/export", "usage"},
		{http.MethodPost, "/api/auth/register", "auth"},
		{http.MethodPost, "/api/some-future-route", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var called atomic.Bool
			h := DemoReadOnlyGuard(demoConfig(true))(demoGuardTestHandler(&called))
			rr := serve(h, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d，期望 403；body=%s", rr.Code, rr.Body.String())
			}
			if called.Load() {
				t.Fatal("被拒请求不得到达业务 handler")
			}
			body := decodeDemoProblem(t, rr)
			if body.Type != problemTypeDemoReadOnly {
				t.Fatalf("type = %q，期望固定 %q", body.Type, problemTypeDemoReadOnly)
			}
			if body.Status != http.StatusForbidden || body.Title == "" || body.Detail == "" {
				t.Fatalf("problem 形状不完整: %+v", body)
			}
			if got := demoWriteRouteFamily(tc.path); got != tc.family {
				t.Fatalf("route_family = %q，期望 %q", got, tc.family)
			}
		})
	}
}

// TestDemoReadOnlyGuardAllowsSessionAndCallbacks 覆盖 Q2① 与 Q2③。
func TestDemoReadOnlyGuardAllowsSessionAndCallbacks(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodPost, "/api/auth/login"},
		{http.MethodPost, "/api/auth/2fa/challenge"},
		{http.MethodPost, "/api/logout"},
		{http.MethodPost, "/api/auth/email-code"},
		{http.MethodPost, "/api/auth/password-reset/request"},
		{http.MethodPost, "/api/auth/password-reset/validate"},
		{http.MethodPost, "/api/auth/password-reset/confirm"},
		{http.MethodPost, "/api/webhooks/stripe"},
		{http.MethodPost, "/api/webhooks/alipay"},
		{http.MethodPost, "/api/webhooks/epay"},
	}
	for _, tc := range allowed {
		t.Run(tc.path, func(t *testing.T) {
			var called atomic.Bool
			h := DemoReadOnlyGuard(demoConfig(true))(demoGuardTestHandler(&called))
			rr := serve(h, httptest.NewRequest(tc.method, tc.path, nil))
			if !called.Load() {
				t.Fatalf("%s 应放行，实际 status=%d body=%s", tc.path, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestDemoReadOnlyGuardIgnoresNonAPIPaths 防御：守卫只属于 /api/*。
func TestDemoReadOnlyGuardIgnoresNonAPIPaths(t *testing.T) {
	var called atomic.Bool
	h := DemoReadOnlyGuard(demoConfig(true))(demoGuardTestHandler(&called))
	rr := serve(h, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !called.Load() {
		t.Fatalf("/v1 不应被只读守卫拦截：status=%d", rr.Code)
	}
}

// TestNewRouterInstallsDemoGuard 验证装配：守卫包住整个 /api/* mux，
// 且公开端点与受保护子树的写方法都被覆盖。
func TestNewRouterInstallsDemoGuard(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{
		keyDemoEnabled:                configBool(true),
		keyDemoRelayPerIPDaily:        configInt(20),
		keyDemoRelayGlobalDaily:       configInt(2000),
		"auth.password_login_enabled": configBool(true),
		"auth.signup_enabled":         configBool(true),
	}, authTestOptions{})
	// 预置演示账号：登录放行必须真的成功（M2 强化），不是「恰好没被 403」。
	env.repo.add(UserRecord{
		Username: "demo-user", Email: "demo@example.com",
		PasswordHash: authTestPasswordHash(),
	})

	// 未认证的管理面写：守卫先于 SessionAuth，必须是 403 demo（不是 401）。
	rr := env.request(http.MethodPost, "/api/settings", nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /api/settings = %d，期望守卫 403；body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeDemoProblem(t, rr); body.Type != problemTypeDemoReadOnly {
		t.Fatalf("problem type = %q", body.Type)
	}

	// 读请求不受影响。
	if rr := env.request(http.MethodGet, "/api/status", nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d，期望 200", rr.Code)
	}

	// 登出在演示站必须可用（Q2①）。
	if rr := env.request(http.MethodPost, "/api/logout", nil, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("POST /api/logout = %d，期望 204；body=%s", rr.Code, rr.Body.String())
	}

	// 注册被拒（换号无法绕过额度）。
	if rr := env.request(http.MethodPost, "/api/auth/register", map[string]any{
		"username": "demo-user", "email": "demo@example.com", "password": "correct horse battery staple",
	}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("POST /api/auth/register = %d，期望守卫 403；body=%s", rr.Code, rr.Body.String())
	}

	// 登录必须真正成功（M2 强化）：守卫放行之后，#Q2① 的会话入口可用，
	// 且拿到会话 Cookie。只断言「不是 403」会把登录逻辑回归伪装成放行。
	rr = env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "demo-user", "password": authTestPassword,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("登录 = %d，期望 200；body=%s", rr.Code, rr.Body.String())
	}
	if cookieByName(rr, auth.SessionCookieName) == nil {
		t.Fatal("登录成功必须下发会话 Cookie")
	}
}

// TestNewRouterDemoDisabledKeepsWrites 是反向基线：关闭时注册等写方法不被守卫拦。
func TestNewRouterDemoDisabledKeepsWrites(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{
		keyDemoEnabled:                configBool(false),
		"auth.password_login_enabled": configBool(true),
		"auth.signup_enabled":         configBool(true),
	}, authTestOptions{})
	rr := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("demo-user", "demo@example.com", authTestPassword, ""), nil)
	// 关闭演示站后注册必须真的成功（M2 强化）：201 表示请求穿过了守卫并
	// 完成业务写入，而不只是「没被 demo problem 拒绝」。
	if rr.Code != http.StatusCreated {
		t.Fatalf("关闭演示站后注册 = %d，期望 201；body=%s", rr.Code, rr.Body.String())
	}
	if len(env.repo.created) != 1 {
		t.Fatalf("注册写入次数 = %d，期望 1", len(env.repo.created))
	}
}

// ---------- 中继额度 ----------

func demoQuotaConfig(enabled bool, perIP, global int64) mapConfig {
	cfg := mapConfig{
		keyDemoEnabled:          configBool(enabled),
		keyDemoRelayPerIPDaily:  configInt(perIP),
		keyDemoRelayGlobalDaily: configInt(global),
	}
	return cfg
}

// chatFromIP 与 rateLimitTestEnv.chat 相同，但显式指定客户端地址，
// 用于隔离 per-IP 桶。
func (e *rateLimitTestEnv) chatFromIP(t *testing.T, token, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(rateLimitChatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func TestDemoRelayQuotaDisabledDoesNotChangeRelay(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(false, 20, 2000)})
	for i := 0; i < 25; i++ {
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.77"); w.Code != http.StatusOK {
			t.Fatalf("关闭演示站后第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	if got := env.billing.preConsumeCount(); got != 25 {
		t.Fatalf("预扣次数 = %d，期望 25", got)
	}
}

func TestDemoRelayQuotaPerIP21stIs429(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, 20, 2000)})
	const ip = "203.0.113.10"
	for i := 0; i < 20; i++ {
		if w := env.chatFromIP(t, relayTestToken, ip); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	w := env.chatFromIP(t, relayTestToken, ip)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 21 次 = %d，期望 429；body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	envelope := decodeRelayError(t, w.Body.Bytes())
	if envelope.Error.Code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", envelope.Error.Code, demoRelayQuotaExceededCode)
	}
	// 被拒请求零预扣、零上游：20 次成功各一次。
	if got := env.billing.preConsumeCount(); got != 20 {
		t.Fatalf("预扣次数 = %d，期望 20（第 21 次零预扣）", got)
	}
	if got := env.upstream.count(); got != 20 {
		t.Fatalf("上游调用 = %d，期望 20", got)
	}

	// 键形状：日期 + ip 段（sha256，不落明文）+ global。
	keys := env.mr.Keys()
	var sawIP, sawGlobal bool
	for _, key := range keys {
		if strings.Contains(key, demoIPHash(ip)) && strings.HasPrefix(key, "demo:relay:") && strings.Contains(key, ":ip:") {
			sawIP = true
			if strings.Contains(key, ip) {
				t.Fatalf("Redis 键含明文 IP：%q", key)
			}
		}
		if strings.HasPrefix(key, "demo:relay:") && strings.HasSuffix(key, ":global") {
			sawGlobal = true
		}
	}
	if !sawIP || !sawGlobal {
		t.Fatalf("Redis 键缺失：sawIP=%v sawGlobal=%v keys=%v", sawIP, sawGlobal, keys)
	}
}

// TestDemoRelayQuotaPerIPIsIndependent 是「配额键必须带 IP 段」的变异锚点：
// 去掉 IP 段后，B 的首次请求会被 A 消耗过的桶拒绝。
func TestDemoRelayQuotaPerIPIsIndependent(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, 2, 2000)})
	for i := 0; i < 2; i++ {
		if w := env.chatFromIP(t, relayTestToken, "198.51.100.1"); w.Code != http.StatusOK {
			t.Fatalf("A 第 %d 次 = %d，期望 200", i+1, w.Code)
		}
	}
	if w := env.chatFromIP(t, relayTestToken, "198.51.100.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("A 第 3 次 = %d，期望 429", w.Code)
	}
	if w := env.chatFromIP(t, relayTestToken, "198.51.100.2"); w.Code != http.StatusOK {
		t.Fatalf("B 第 1 次 = %d，期望 200（IP 桶必须独立）；body=%s", w.Code, w.Body.String())
	}
}

// TestDemoRelayQuotaGlobal2001stIs429 覆盖保险丝：另一 IP 也被拒。
func TestDemoRelayQuotaGlobal2001stIs429(t *testing.T) {
	cfg := demoQuotaConfig(true, 1000000, 2000)
	// 关闭 B10（默认 60/分钟）。全站额度的验证需要同一用户连打 2000 次，
	// 若 B10 仍生效会在第 61 次先拒，测不到保险丝。0 = 不限（F-08-16）。
	cfg["rl.user_model_rpm"] = configInt(0)
	cfg["rl.token_rpm"] = configInt(0)
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: cfg})
	for i := 0; i < 2000; i++ {
		ip := fmt.Sprintf("198.51.%d.%d", i/250+1, i%250+1)
		if w := env.chatFromIP(t, relayTestToken, ip); w.Code != http.StatusOK {
			t.Fatalf("全站第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	first := env.chatFromIP(t, relayTestToken, "203.0.113.200")
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("全站第 2001 次 = %d，期望 429；body=%s", first.Code, first.Body.String())
	}
	if first.Header().Get("Retry-After") == "" {
		t.Fatal("全站 429 缺少 Retry-After")
	}
	if code := decodeRelayError(t, first.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaExceededCode)
	}
	// 另一个从未调用过的 IP 也撞全站保险丝。
	if second := env.chatFromIP(t, relayTestToken, "203.0.113.201"); second.Code != http.StatusTooManyRequests {
		t.Fatalf("全站耗尽后另一 IP = %d，期望 429", second.Code)
	}
	if got := env.upstream.count(); got != 2000 {
		t.Fatalf("上游调用 = %d，期望 2000（被拒不触达上游）", got)
	}
}

func TestDemoRelayQuotaZeroClosesRelay(t *testing.T) {
	for _, tc := range []struct {
		name          string
		perIP, global int64
	}{
		{"per_ip_zero", 0, 2000},
		{"global_zero", 20, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, tc.perIP, tc.global)})
			w := env.chatFromIP(t, relayTestToken, "203.0.113.30")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d，期望 403（0=中继关闭）；body=%s", w.Code, w.Body.String())
			}
			if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != demoRelayDisabledCode {
				t.Fatalf("code = %q，期望 %s", code, demoRelayDisabledCode)
			}
			if env.upstream.count() != 0 || env.billing.preConsumeCount() != 0 {
				t.Fatalf("关闭态必须零上游零预扣：upstream=%d preconsume=%d",
					env.upstream.count(), env.billing.preConsumeCount())
			}
			if len(env.mr.Keys()) != 0 {
				t.Fatalf("配置级关闭不应访问 Redis：keys=%v", env.mr.Keys())
			}
		})
	}
}

func TestDemoRelayQuotaRedisFailureIs503(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, 20, 2000)})
	env.mr.Close()
	w := env.chatFromIP(t, relayTestToken, "203.0.113.40")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（fail-closed）；body=%s", w.Code, w.Body.String())
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != demoRelayQuotaUnavailableCode {
		t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaUnavailableCode)
	}
	if env.upstream.count() != 0 || env.billing.preConsumeCount() != 0 {
		t.Fatalf("配额后端故障必须零上游零预扣：upstream=%d preconsume=%d",
			env.upstream.count(), env.billing.preConsumeCount())
	}
}

// TestDemoRelayQuotaRedisFailureNonDemoUnchanged：非演示模式仍走既有
// B10–B12 的 500 fail-closed 口径，不受本任务影响。
func TestDemoRelayQuotaRedisFailureNonDemoUnchanged(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(false, 20, 2000)})
	env.mr.Close()
	w := env.chatFromIP(t, relayTestToken, "203.0.113.41")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，期望既有 500 rate_limit_check_failed；body=%s", w.Code, w.Body.String())
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != "rate_limit_check_failed" {
		t.Fatalf("code = %q，期望 rate_limit_check_failed", code)
	}
}

// TestDemoRelayQuotaStacksWithBPools 证明演示额度与 B10 可叠加、先到者拒。
func TestDemoRelayQuotaStacksWithBPools(t *testing.T) {
	cfg := demoQuotaConfig(true, 5, 2000)
	cfg["rl.user_model_rpm"] = configInt(2)
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: cfg})
	for i := 0; i < 2; i++ {
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.50"); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 = %d，期望 200", i+1, w.Code)
		}
	}
	// B10 阈值 2 先于演示额度的 5 触发：code 必须是 B 池的固定值。
	w := env.chatFromIP(t, relayTestToken, "203.0.113.50")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次 = %d，期望 429", w.Code)
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != "rate_limit_exceeded" {
		t.Fatalf("code = %q，期望 rate_limit_exceeded（先到者拒）", code)
	}
}

// TestDemoRelayQuotaCountsEvenForUnlimitedToken 固定设计口径：
// 演示账号用 `unlimited_quota=true` 识别（§3.17.2「其令牌置
// unlimited_quota=true」）。该标志在演示模式下换取的是**账单豁免**（
// 余额不扣，见 billing 的 demoExempt），但**不豁免**两个日计数器——
// 「兜底全在这两个计数器里」。改名以表达真实语义，避免旧名
// 「OnlyBypassesBalance」误导读者以为额度也被绕过。
func TestDemoRelayQuotaCountsEvenForUnlimitedToken(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config: demoQuotaConfig(true, 2, 2000),
		tokens: []rateLimitSeedToken{{
			plain: "demo-account-token",
			meta: auth.TokenMeta{
				ID: 9001, UserID: 9001, Status: "enabled", Group: "default",
				// 演示账号：不走令牌余额，但不豁免额度计数。
				UnlimitedQuota: true,
				RemainQuota:    0,
			},
		}},
	})
	for i := 0; i < 2; i++ {
		if w := env.chatFromIP(t, "demo-account-token", "203.0.113.60"); w.Code != http.StatusOK {
			t.Fatalf("演示账号第 %d 次 = %d，期望 200（unlimited_quota 绕过余额门）；body=%s",
				i+1, w.Code, w.Body.String())
		}
	}
	if w := env.chatFromIP(t, "demo-account-token", "203.0.113.60"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("演示账号第 3 次 = %d，期望 429（计数器不因账号身份豁免）", w.Code)
	}
}

// ---------- 额度配置/时间窗的纯函数 ----------

func TestDemoRelayQuotaLimitsResolve(t *testing.T) {
	if perIP, global := demoRelayQuotaLimits(nil); perIP != 20 || global != 2000 {
		t.Fatalf("默认 = %d/%d，期望 20/2000", perIP, global)
	}
	cfg := mapConfig{
		keyDemoRelayPerIPDaily:  configInt(7),
		keyDemoRelayGlobalDaily: configInt(0),
	}
	if perIP, global := demoRelayQuotaLimits(cfg); perIP != 7 || global != 0 {
		t.Fatalf("显式值 = %d/%d，期望 7/0（0 必须原样保留）", perIP, global)
	}
	bad := mapConfig{
		keyDemoRelayPerIPDaily:  json.RawMessage(`"not-an-int"`),
		keyDemoRelayGlobalDaily: json.RawMessage(`-5`),
	}
	if perIP, global := demoRelayQuotaLimits(bad); perIP != 20 || global != 2000 {
		t.Fatalf("脏值回落 = %d/%d，期望 20/2000", perIP, global)
	}
}

func TestDemoQuotaWindowUsesSiteTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("宿主缺少 tzdata，跳过时区用例")
	}
	now := time.Date(2026, 10, 22, 7, 30, 0, 0, loc)
	date, window := demoQuotaWindow(now, loc)
	if date != "20261022" {
		t.Fatalf("date = %q，期望站点时区的 20261022", date)
	}
	if want := 16*time.Hour + 30*time.Minute; window != want {
		t.Fatalf("window = %v，期望 %v（到次日 00:00）", window, want)
	}
	// 同一瞬间在 UTC 下已跨日：键必须跟随站点时区而不是进程时区。
	utcDate, _ := demoQuotaWindow(now.UTC(), time.UTC)
	if utcDate == date {
		t.Fatalf("UTC 与站点时区不应得到同一日期（UTC=%s site=%s）", utcDate, date)
	}
}

// ---------- I2：NEWTG_DEMO_ENABLED 部署侧覆盖 ----------

func boolPtr(v bool) *bool { return &v }

// TestDemoModeEnvOverrideIsSingleEffectiveValue 验证守卫与中继额度都走
// platformconfig.DemoEnabled / WithDemoOverride 这一条有效值路径。
func TestDemoModeEnvOverrideIsSingleEffectiveValue(t *testing.T) {
	t.Run("env=false overrides setting=true", func(t *testing.T) {
		wrapped := platformconfig.WithDemoOverride(demoConfig(true), boolPtr(false))

		// 守卫按 env 关闭：写请求透传（204 来自测试 handler）。
		var called atomic.Bool
		h := DemoReadOnlyGuard(wrapped)(demoGuardTestHandler(&called))
		rr := serve(h, httptest.NewRequest(http.MethodPost, "/api/settings", nil))
		if rr.Code != http.StatusNoContent || !called.Load() {
			t.Fatalf("env=false 时 POST /api/settings = %d called=%v，期望放行", rr.Code, called.Load())
		}

		// 中继额度同样按 env 关闭：per-IP=1 也不应触发 429。
		env := newRateLimitTestEnv(t, rateLimitEnvOptions{
			config: platformconfig.WithDemoOverride(demoQuotaConfig(true, 1, 100), boolPtr(false)),
		})
		for i := 0; i < 3; i++ {
			if w := env.chatFromIP(t, relayTestToken, "203.0.113.91"); w.Code != http.StatusOK {
				t.Fatalf("env=false 第 %d 次 = %d，期望 200（额度必须随守卫一起关闭）；body=%s",
					i+1, w.Code, w.Body.String())
			}
		}
	})

	t.Run("env=true overrides setting=false", func(t *testing.T) {
		wrapped := platformconfig.WithDemoOverride(demoConfig(false), boolPtr(true))

		var called atomic.Bool
		h := DemoReadOnlyGuard(wrapped)(demoGuardTestHandler(&called))
		rr := serve(h, httptest.NewRequest(http.MethodPost, "/api/settings", nil))
		if rr.Code != http.StatusForbidden || called.Load() {
			t.Fatalf("env=true 时 POST /api/settings = %d called=%v，期望 403", rr.Code, called.Load())
		}

		env := newRateLimitTestEnv(t, rateLimitEnvOptions{
			config: platformconfig.WithDemoOverride(demoQuotaConfig(false, 1, 100), boolPtr(true)),
		})
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.92"); w.Code != http.StatusOK {
			t.Fatalf("env=true 第 1 次 = %d，期望 200", w.Code)
		}
		w := env.chatFromIP(t, relayTestToken, "203.0.113.92")
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("env=true 第 2 次 = %d，期望 429（额度必须随守卫一起开启）；body=%s", w.Code, w.Body.String())
		}
		if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
			t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaExceededCode)
		}
	})
}

// ---------- I3：可观测断言 ----------

// demoMetricValue 从进程级 registry 读取一个带标签的计数器当前值。
// 只用于测试，不给生产新增读取接口。
func demoMetricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := true
			for wantKey, wantValue := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == wantKey && lp.GetValue() == wantValue {
						found = true
						break
					}
				}
				if !found {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
		}
	}
	return 0
}

// demoCountingExecer 只统计审计写入次数：用来断言「被拒写入后审计表行数不变」。
type demoCountingExecer struct{ calls atomic.Int64 }

func (e *demoCountingExecer) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	e.calls.Add(1)
	return demoSQLResult{}, nil
}

type demoSQLResult struct{}

func (demoSQLResult) LastInsertId() (int64, error) { return 0, nil }
func (demoSQLResult) RowsAffected() (int64, error) { return 1, nil }

func TestDemoGuardRejectionIsObservableButNotAudited(t *testing.T) {
	exec := &demoCountingExecer{}
	env := newAuthTestEnv(t, mapConfig{keyDemoEnabled: configBool(true)},
		authTestOptions{audit: audit.New(exec)})

	before := demoMetricValue(t, "newtg_demo_write_rejected_total",
		map[string]string{"route_family": "settings"})
	rr := env.request(http.MethodPost, "/api/settings", map[string]any{}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /api/settings = %d，期望 403；body=%s", rr.Code, rr.Body.String())
	}
	after := demoMetricValue(t, "newtg_demo_write_rejected_total",
		map[string]string{"route_family": "settings"})
	if delta := after - before; delta != 1 {
		t.Fatalf("demo_write_rejected_total{route_family=settings} 增量 = %v，期望 1", delta)
	}
	if got := exec.calls.Load(); got != 0 {
		t.Fatalf("被拒写入产生 %d 条审计，期望 0（Q6：不写审计）", got)
	}
}

func TestDemoRelayQuotaExceededMetricsByScope(t *testing.T) {
	t.Run("ip", func(t *testing.T) {
		env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, 1, 1000)})
		ipBefore := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "ip"})
		globalBefore := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "global"})
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.94"); w.Code != http.StatusOK {
			t.Fatalf("第 1 次 = %d，期望 200", w.Code)
		}
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.94"); w.Code != http.StatusTooManyRequests {
			t.Fatalf("第 2 次 = %d，期望 429", w.Code)
		}
		if delta := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "ip"}) - ipBefore; delta != 1 {
			t.Fatalf("scope=ip 增量 = %v，期望 1", delta)
		}
		if delta := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "global"}) - globalBefore; delta != 0 {
			t.Fatalf("scope=global 增量 = %v，期望 0", delta)
		}
	})

	t.Run("global", func(t *testing.T) {
		env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, 1000, 1)})
		ipBefore := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "ip"})
		globalBefore := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "global"})
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.95"); w.Code != http.StatusOK {
			t.Fatalf("第 1 次 = %d，期望 200", w.Code)
		}
		if w := env.chatFromIP(t, relayTestToken, "203.0.113.96"); w.Code != http.StatusTooManyRequests {
			t.Fatalf("全站超限 = %d，期望 429", w.Code)
		}
		if delta := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "global"}) - globalBefore; delta != 1 {
			t.Fatalf("scope=global 增量 = %v，期望 1", delta)
		}
		if delta := demoMetricValue(t, "newtg_demo_relay_quota_exceeded_total", map[string]string{"scope": "ip"}) - ipBefore; delta != 0 {
			t.Fatalf("scope=ip 增量 = %v，期望 0", delta)
		}
	})
}

// ---------- M3：顺序短路 ----------

// TestDemoRelayQuotaIPRejectionDoesNotIncrementGlobal 读 Redis 键值证明：
// IP 层已经拒绝时全站计数器不再自增。把顺序改成 global→ip 时本用例必须红。
func TestDemoRelayQuotaIPRejectionDoesNotIncrementGlobal(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{config: demoQuotaConfig(true, 1, 100)})
	const ip = "203.0.113.97"
	if w := env.chatFromIP(t, relayTestToken, ip); w.Code != http.StatusOK {
		t.Fatalf("第 1 次 = %d，期望 200", w.Code)
	}
	if w := env.chatFromIP(t, relayTestToken, ip); w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次 = %d，期望 429", w.Code)
	}
	var globalKey string
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "demo:relay:") && strings.HasSuffix(key, ":global") {
			globalKey = key
			break
		}
	}
	if globalKey == "" {
		t.Fatalf("未找到全站计数键；keys=%v", env.mr.Keys())
	}
	got, err := env.mr.Get(globalKey)
	if err != nil {
		t.Fatalf("读取全站计数: %v", err)
	}
	if got != "1" {
		t.Fatalf("全站计数 = %s，期望 1（IP 已拒绝的请求不得再自增全站）", got)
	}
}

// ---------- I4：非 chat 入口的额度覆盖 ----------

func (e *rateLimitTestEnv) doFromIP(t *testing.T, path, body, ip string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func assertDemoQuotaRejected(t *testing.T, env *rateLimitTestEnv, w *httptest.ResponseRecorder,
	ip string, wantUpstream, wantPreConsume int) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 status = %d，期望 429；body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaExceededCode)
	}
	if got := env.upstream.count(); got != wantUpstream {
		t.Fatalf("上游调用 = %d，期望 %d（被拒不得触达上游）", got, wantUpstream)
	}
	if got := env.billing.preConsumeCount(); got != wantPreConsume {
		t.Fatalf("PreConsume = %d，期望 %d（被拒不得预扣）", got, wantPreConsume)
	}
	// 计数键按真实 IP 归桶，而不是共享的 unknown 桶。
	foundIP := false
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "demo:relay:") && strings.Contains(key, ":ip:"+demoIPHash(ip)) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Fatalf("未找到 %s 的 per-IP 计数键（不得归到 unknown）；keys=%v", ip, env.mr.Keys())
	}
}

func TestDemoRelayQuotaCoversMessages(t *testing.T) {
	// /v1/messages 只接受 Anthropic 协议族渠道；用真实的 messages 装置
	// （假上游 + 原生回写）保证第 1 次是 200 而不是 501。
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, messagesUpstreamBody)
		})
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	env.handler.rateLimiter = ratelimit.New(rdb)
	env.handler.config = demoQuotaConfig(true, 1, 100)

	const ip = "203.0.113.101"
	body := `{"model":"` + messagesTestModel + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", relayTestToken)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		env.handler.Handler().ServeHTTP(rec, req)
		return rec
	}
	if first := do(); first.Code != http.StatusOK {
		t.Fatalf("第 1 次 = %d，期望 200；body=%s", first.Code, first.Body.String())
	}
	if counter.count() != 1 || env.billing.preConsumeCount() != 1 {
		t.Fatalf("第 1 次副作用 = upstream %d / preconsume %d，期望 1/1",
			counter.count(), env.billing.preConsumeCount())
	}
	second := do()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次 = %d，期望 429；body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if code := decodeRelayError(t, second.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaExceededCode)
	}
	if counter.count() != 1 || env.billing.preConsumeCount() != 1 {
		t.Fatalf("被拒请求产生副作用：upstream %d / preconsume %d，期望仍为 1/1",
			counter.count(), env.billing.preConsumeCount())
	}
	assertDemoIPKey(t, mr, ip)
}

func TestDemoRelayQuotaCoversMultimodal(t *testing.T) {
	env := newRateLimitTestEnv(t, rateLimitEnvOptions{
		config:     demoQuotaConfig(true, 1, 100),
		multimodal: true,
	})
	const ip = "203.0.113.102"
	body := `{"model":"` + rateLimitTestModel + `","input":"hi"}`
	first := env.doFromIP(t, "/v1/embeddings", body, ip, map[string]string{"Authorization": "Bearer " + relayTestToken})
	if first.Code != http.StatusOK {
		t.Fatalf("第 1 次 = %d，期望 200；body=%s", first.Code, first.Body.String())
	}
	// dispatcher 是接缝替身：真正的 PreConsume 在 dispatcher 内部（本用例
	// 只验证额度先于 dispatcher 执行），因此这里断言 dispatch 发生、预扣 0。
	if env.multimodal.calls != 1 || env.billing.preConsumeCount() != 0 {
		t.Fatalf("第 1 次副作用 = dispatch %d / preconsume %d，期望 1/0",
			env.multimodal.calls, env.billing.preConsumeCount())
	}
	second := env.doFromIP(t, "/v1/embeddings", body, ip, map[string]string{"Authorization": "Bearer " + relayTestToken})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次 = %d，期望 429；body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if code := decodeRelayError(t, second.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaExceededCode)
	}
	// 多模态经由 dispatcher 接缝执行；被拒请求不得到达 dispatcher。
	if env.multimodal.calls != 1 || env.billing.preConsumeCount() != 0 {
		t.Fatalf("被拒请求产生副作用：dispatch %d / preconsume %d，期望仍为 1/0",
			env.multimodal.calls, env.billing.preConsumeCount())
	}
	assertDemoIPKey(t, env.mr, ip)
}

// assertDemoIPKey 断言 per-IP 计数键使用请求真实地址的哈希（不是 unknown）。
func assertDemoIPKey(t *testing.T, mr *miniredis.Miniredis, ip string) {
	t.Helper()
	for _, key := range mr.Keys() {
		if strings.HasPrefix(key, "demo:relay:") && strings.Contains(key, ":ip:"+demoIPHash(ip)) {
			if strings.Contains(key, ip) {
				t.Fatalf("Redis 键含明文 IP：%q", key)
			}
			return
		}
	}
	t.Fatalf("未找到 %s 的 per-IP 计数键（不得归到 unknown）；keys=%v", ip, mr.Keys())
}

func TestDemoRelayQuotaCoversTaskSubmit(t *testing.T) {
	env := newRateLimitTaskEnv(t, demoQuotaConfig(true, 1, 100), nil)
	const ip = "203.0.113.103"
	submit := func() *httptest.ResponseRecorder {
		req := newTaskSubmitHTTPReq(`{"model":"sample-model"}`)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		return rec
	}
	if w := submit(); w.Code != http.StatusAccepted {
		t.Fatalf("第 1 次提交 = %d，期望 202；body=%s", w.Code, w.Body.String())
	}
	w := submit()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次提交 = %d，期望 429；body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", code, demoRelayQuotaExceededCode)
	}
	if got := taskPreConsumeCount(env.billing); got != 1 {
		t.Fatalf("PreConsume = %d，期望 1", got)
	}
	if got := env.upstream.count(); got != 1 {
		t.Fatalf("上游调用 = %d，期望 1", got)
	}
	if env.store.inserted != 1 {
		t.Fatalf("任务行 = %d，期望 1", env.store.inserted)
	}
	foundIP := false
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "demo:relay:") && strings.Contains(key, ":ip:"+demoIPHash(ip)) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Fatalf("未找到提交 IP 的计数键；keys=%v", env.mr.Keys())
	}
}

// demoMJEnv 是演示额度用例的 MJ 接线：提交走假上游，image-seed 走假 querier，
// 两者共用同一 Redis 限流器与 demo 配置。
type demoMJEnv struct {
	mux      *http.ServeMux
	mr       *miniredis.Miniredis
	billing  *rateLimitBilling
	upstream *mjUpstream
	querier  *fakeMJQuerier
	store    *fakeMJStore
	auth     string
}

func newDemoMJEnv(t *testing.T, cfg fakeMJConfig, perIP int64) *demoMJEnv {
	t.Helper()
	if cfg == nil {
		cfg = fakeMJConfig{}
	}
	if _, ok := cfg["mj.enabled"]; !ok {
		cfg["mj.enabled"] = mjBoolSetting(true)
	}
	if _, ok := cfg["mj.channel_max_inflight"]; !ok {
		cfg["mj.channel_max_inflight"] = mjIntSetting(1000)
	}
	cfg[keyDemoEnabled] = configBool(true)
	cfg[keyDemoRelayPerIPDaily] = configInt(perIP)
	cfg[keyDemoRelayGlobalDaily] = configInt(1000)

	store := newFakeMJStore()
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_1")] = mjQueryTask(42, "mj_1", tasks.StateSuccess)
	upstream := newMJUpstream(t, `{"code":1,"description":"ok","result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	target, candidate := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{
		channels:   map[int64]*tasks.ChannelTarget{10: target},
		candidates: []tasks.MJChannelCandidate{candidate},
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	tokens, key, _ := newMJTokens()
	querier := &fakeMJQuerier{seedBody: []byte(`{"code":1,"description":"ok","result":"seed"}`)}
	billing := newRateLimitBilling()
	handler := NewMJHandler(MJHandlerConfig{
		Store:       store,
		Channels:    channels,
		Sealer:      sealer,
		Tokens:      tokens,
		Billing:     billing.factory,
		Config:      cfg,
		RateLimiter: ratelimit.New(rdb),
		ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
		Querier:     querier,
		Now:         func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) },
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	return &demoMJEnv{mux: mux, mr: mr, billing: billing, upstream: upstream, querier: querier, store: store, auth: "Bearer " + key}
}

func (e *demoMJEnv) do(t *testing.T, path, body, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", e.auth)
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestDemoRelayQuotaCoversMJSubmit(t *testing.T) {
	env := newDemoMJEnv(t, nil, 1)
	const ip = "203.0.113.104"
	first := env.do(t, "/mj/submit/imagine", `{"prompt":"a cat"}`, ip)
	if first.Code != http.StatusOK {
		t.Fatalf("第 1 次 = %d，期望 200；body=%s", first.Code, first.Body.String())
	}
	second := env.do(t, "/mj/submit/imagine", `{"prompt":"a cat"}`, ip)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次 = %d，期望 429；body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if desc := fmt.Sprint(decodeMJEnvelope(t, second)["description"]); desc != mj.DescriptionRateLimited {
		t.Fatalf("description = %q，期望 %s", desc, mj.DescriptionRateLimited)
	}
	if got := env.upstream.calls(); got != 1 {
		t.Fatalf("上游调用 = %d，期望 1（被拒不得提交上游）", got)
	}
	if got := env.billing.preConsumeCount(); got != 1 {
		t.Fatalf("PreConsume = %d，期望 1", got)
	}
	foundIP := false
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "demo:relay:") && strings.Contains(key, ":ip:"+demoIPHash(ip)) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Fatalf("未找到 MJ 提交 IP 的计数键；keys=%v", env.mr.Keys())
	}
}

// TestDemoRelayQuotaMJMapping 覆盖 429/403/503 三条映射，且拒绝路径零上游、
// 零预扣（因此真实引擎也不会写 usage_logs：日志发生在 PreConsume 之后）。
func TestDemoRelayQuotaMJMapping(t *testing.T) {
	t.Run("quota exhausted 429", func(t *testing.T) {
		env := newDemoMJEnv(t, nil, 1)
		const ip = "203.0.113.105"
		if w := env.do(t, "/mj/submit/imagine", `{"prompt":"a cat"}`, ip); w.Code != http.StatusOK {
			t.Fatalf("第 1 次 = %d，期望 200", w.Code)
		}
		w := env.do(t, "/mj/submit/imagine", `{"prompt":"a cat"}`, ip)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("超限 = %d，期望 429；body=%s", w.Code, w.Body.String())
		}
		if desc := fmt.Sprint(decodeMJEnvelope(t, w)["description"]); desc != mj.DescriptionRateLimited {
			t.Fatalf("description = %q，期望 %s", desc, mj.DescriptionRateLimited)
		}
		if env.upstream.calls() != 1 || env.billing.preConsumeCount() != 1 {
			t.Fatalf("被拒请求产生副作用：upstream=%d preconsume=%d，期望 1/1",
				env.upstream.calls(), env.billing.preConsumeCount())
		}
	})

	t.Run("quota zero 403", func(t *testing.T) {
		env := newDemoMJEnv(t, nil, 0)
		w := env.do(t, "/mj/submit/imagine", `{"prompt":"a cat"}`, "203.0.113.106")
		if w.Code != http.StatusForbidden {
			t.Fatalf("额度为 0 = %d，期望 403；body=%s", w.Code, w.Body.String())
		}
		if env.upstream.calls() != 0 || env.billing.preConsumeCount() != 0 {
			t.Fatalf("403 必须零上游零预扣：upstream=%d preconsume=%d",
				env.upstream.calls(), env.billing.preConsumeCount())
		}
	})

	t.Run("redis failure 503", func(t *testing.T) {
		env := newDemoMJEnv(t, nil, 1)
		env.mr.Close()
		w := env.do(t, "/mj/submit/imagine", `{"prompt":"a cat"}`, "203.0.113.107")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("Redis 故障 = %d，期望 503；body=%s", w.Code, w.Body.String())
		}
		if env.upstream.calls() != 0 || env.billing.preConsumeCount() != 0 {
			t.Fatalf("503 必须零上游零预扣：upstream=%d preconsume=%d",
				env.upstream.calls(), env.billing.preConsumeCount())
		}
	})
}

// TestDemoRelayQuotaCoversMJImageSeed 覆盖 M5：image-seed 会真实转发上游，
// 必须在转发之前消耗同一套两层日额度。
func TestDemoRelayQuotaCoversMJImageSeed(t *testing.T) {
	env := newDemoMJEnv(t, nil, 1)
	const ip = "203.0.113.108"
	first := env.do(t, "/mj/task/mj_1/image-seed", ``, ip)
	if first.Code != http.StatusOK {
		t.Fatalf("第 1 次 = %d，期望 200；body=%s", first.Code, first.Body.String())
	}
	if env.querier.calls() != 1 {
		t.Fatalf("上游 image-seed 调用 = %d，期望 1", env.querier.calls())
	}
	second := env.do(t, "/mj/task/mj_1/image-seed", ``, ip)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次 = %d，期望 429；body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if env.querier.calls() != 1 {
		t.Fatalf("被拒请求仍触达上游：image-seed 调用 = %d，期望仍为 1", env.querier.calls())
	}
	foundIP := false
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "demo:relay:") && strings.Contains(key, ":ip:"+demoIPHash(ip)) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Fatalf("未找到 image-seed 请求 IP 的计数键；keys=%v", env.mr.Keys())
	}
}
