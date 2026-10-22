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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
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
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
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

	// 登录不被守卫拦（具体状态由登录逻辑决定）。
	rr = env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "demo-user", "password": "wrong",
	}, nil)
	if rr.Code == http.StatusForbidden {
		if body := decodeDemoProblem(t, rr); body.Type == problemTypeDemoReadOnly {
			t.Fatalf("登录被只读守卫误拦：%s", rr.Body.String())
		}
	}
}

// TestNewRouterDemoDisabledKeepsWrites 是反向基线：关闭时注册等写方法不被守卫拦。
func TestNewRouterDemoDisabledKeepsWrites(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{
		keyDemoEnabled:                configBool(false),
		"auth.password_login_enabled": configBool(true),
		"auth.signup_enabled":         configBool(true),
	}, authTestOptions{})
	rr := env.request(http.MethodPost, "/api/auth/register", map[string]any{
		"username": "demo-user", "email": "demo@example.com", "password": "correct horse battery staple",
	}, nil)
	if rr.Code == http.StatusForbidden {
		if body := decodeDemoProblem(t, rr); body.Type == problemTypeDemoReadOnly {
			t.Fatalf("关闭演示站后注册仍被守卫拒绝：%s", rr.Body.String())
		}
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

// TestDemoRelayQuotaDemoAccountUnlimitedOnlyBypassesBalance 固定设计口径：
// 演示账号用 `unlimited_quota=true` 识别（§3.17.2「其令牌置
// unlimited_quota=true」），它绕过的是余额门；两个日计数器仍是兜底，
// 不因账号身份豁免（「兜底全在这两个计数器里」）。
func TestDemoRelayQuotaDemoAccountUnlimitedOnlyBypassesBalance(t *testing.T) {
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
