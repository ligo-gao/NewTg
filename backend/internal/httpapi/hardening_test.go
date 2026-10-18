package httpapi

// 本文件是「认证加固」任务的验收：B1/B3/B8 请求速率桶、TRUSTED_PROXIES
// 感知的 clientIP 解析、must_change_password 强制改密拦截。
//
// 全部使用 miniredis + 内存仓储（GC-8 单元层，不触碰外部 PG/Redis）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
)

// ---------------------------------------------------------------------------
// TRUSTED_PROXIES / clientIP
// ---------------------------------------------------------------------------

// TestResolveClientIP 覆盖 security-07 §2.4 的信任边界：
//   - 空信任列表 = 永不采信 X-Forwarded-For；
//   - 仅当直连 peer 属于可信代理时才从右向左跳过可信代理，取第一个不可信地址；
//   - IPv4/IPv6/映射地址与畸形条目逐表验证。
func TestResolveClientIP(t *testing.T) {
	prefixes := func(raw ...string) []netip.Prefix {
		out := make([]netip.Prefix, 0, len(raw))
		for _, s := range raw {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out
	}

	cases := []struct {
		name       string
		remoteAddr string
		xff        []string
		trusted    []netip.Prefix
		want       string
	}{
		{
			name:       "空信任列表忽略 XFF",
			remoteAddr: "203.0.113.9:1234",
			xff:        []string{"198.51.100.7"},
			want:       "203.0.113.9",
		},
		{
			name:       "无可信代理时无端口 RemoteAddr",
			remoteAddr: "203.0.113.9",
			xff:        []string{"198.51.100.7"},
			want:       "203.0.113.9",
		},
		{
			name:       "直连 peer 不可信则忽略 XFF",
			remoteAddr: "203.0.113.9:1234",
			xff:        []string{"1.2.3.4"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.9",
		},
		{
			name:       "可信代理单跳取客户端",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"203.0.113.7"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.7",
		},
		{
			name:       "链式跳过可信代理",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"203.0.113.7, 10.0.0.3"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.7",
		},
		{
			name:       "伪造的左侧条目被忽略",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"1.1.1.1, 203.0.113.7"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.7",
		},
		{
			name:       "全部条目可信时取最左",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"10.0.0.9, 10.0.0.3"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "10.0.0.9",
		},
		{
			name:       "多行 XFF 按顺序拼接",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"203.0.113.7", "10.0.0.4"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.7",
		},
		{
			name:       "IPv6 可信代理",
			remoteAddr: "[2001:db8:1::2]:443",
			xff:        []string{"2001:db8:2::7"},
			trusted:    prefixes("2001:db8:1::/48"),
			want:       "2001:db8:2::7",
		},
		{
			name:       "IPv4 映射 IPv6 前缀",
			remoteAddr: "[::ffff:10.0.0.2]:1234",
			xff:        []string{"203.0.113.7"},
			trusted:    prefixes("::ffff:10.0.0.0/104"),
			want:       "203.0.113.7",
		},
		{
			name:       "XFF 含畸形条目时整体回退直连 peer",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"203.0.113.7, not-an-ip"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "10.0.0.2",
		},
		{
			name:       "信任列表中的畸形条目被忽略",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"203.0.113.7"},
			trusted:    append(prefixes("10.0.0.0/8"), netip.Prefix{}), // 零值前缀必须被跳过
			want:       "203.0.113.7",
		},
		{
			name:       "畸形条目不得让远端地址变为可信",
			remoteAddr: "192.0.2.1:1234",
			xff:        []string{"203.0.113.7"},
			trusted:    append(prefixes("10.0.0.0/8"), netip.Prefix{}),
			want:       "192.0.2.1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := resolveClientIP(req, tc.trusted); got != tc.want {
				t.Fatalf("resolveClientIP = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestConfigureTrustedProxies 验证装配接缝：合法条目生效、畸形条目返回错误
// 但不被信任；空串恢复「永不信任 XFF」。
func TestConfigureTrustedProxies(t *testing.T) {
	t.Cleanup(func() { _ = ConfigureTrustedProxies(envTrustedProxiesForTest()) })

	if err := ConfigureTrustedProxies("10.0.0.0/8, not-an-ip"); err == nil {
		t.Fatal("畸形条目必须返回错误（装配方应记录/拒绝）")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := clientIP(req); got != "203.0.113.7" {
		t.Fatalf("配置可信代理后 clientIP = %q，期望 XFF 客户端 203.0.113.7", got)
	}

	if err := ConfigureTrustedProxies(""); err != nil {
		t.Fatalf("空配置不应报错: %v", err)
	}
	if got := clientIP(req); got != "10.0.0.2" {
		t.Fatalf("空信任列表 clientIP = %q，期望直连 peer 10.0.0.2", got)
	}
}

// envTrustedProxiesForTest 返回进程环境里的 TRUSTED_PROXIES，供清理恢复。
func envTrustedProxiesForTest() string {
	// 与 init 读取同一来源（config.Config.TrustedProxies 的底层环境变量）。
	return os.Getenv(trustedProxiesEnv)
}

// ---------------------------------------------------------------------------
// B1 / B3 / B8 请求速率桶
// ---------------------------------------------------------------------------

func TestLoginRequestRateBucket(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})
	env.router.handlers.verifyPassword = func(_, plain string) bool { return plain == authTestPassword }

	for i := 0; i < LoginRateLimit; i++ {
		if rr := env.login("alice", authTestPassword); rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次登录 status = %d，期望 200（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}

	limited := env.login("alice", authTestPassword)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次登录 status = %d，期望 429 请求速率桶（body=%q）",
			LoginRateLimit+1, limited.Code, limited.Body.String())
	}
	if p := decodeProblem(t, limited); p.Type != authProblemTooManyRequests {
		t.Errorf("problem.type = %q，期望 %q", p.Type, authProblemTooManyRequests)
	}
	retry, err := strconv.Atoi(limited.Header().Get("Retry-After"))
	if err != nil || retry <= 0 || retry > int(LoginRateWindow.Seconds()) {
		t.Errorf("Retry-After = %q，期望 1..%v 秒", limited.Header().Get("Retry-After"), LoginRateWindow.Seconds())
	}

	key := LoginRateLimitKey("192.0.2.1", "alice")
	if !env.mr.Exists(key) {
		t.Fatalf("缺少 B1 桶键 %s", key)
	}
	if ttl := env.mr.TTL(key); ttl <= 0 || ttl > LoginRateWindow {
		t.Errorf("B1 桶 TTL = %v，期望 (0, %v]", ttl, LoginRateWindow)
	}
	if !strings.HasPrefix(key, RateLimitKeyPrefix+"LI:") {
		t.Errorf("B1 键前缀 = %q，期望冻结命名空间 %q", key, RateLimitKeyPrefix+"LI:...")
	}

	// 同 IP 不同 identifier 使用独立桶。
	env.repo.add(UserRecord{Username: "bob", PasswordHash: authTestPasswordHash()})
	if rr := env.login("bob", authTestPassword); rr.Code != http.StatusOK {
		t.Fatalf("同 IP 不同 identifier status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	// Redis 故障 fail-closed：503 + Retry-After，绝不绕过速率桶。
	down := newAuthTestEnv(t, nil, authTestOptions{})
	down.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})
	down.mr.Close()
	rr := down.login("alice", authTestPassword)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis 故障 status = %d，期望 503 fail-closed（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Redis 故障的 503 必须携带 Retry-After")
	}
}

func TestRegisterRequestRateBucket(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	for i := 0; i < RegisterRateLimit; i++ {
		username := "user" + strconv.Itoa(i)
		rr := env.request(http.MethodPost, "/api/auth/register",
			registerPayload(username, username+"@example.com", "N7x!qPz2wR", ""), nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("第 %d 次注册 status = %d，期望 201（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}

	limited := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("overflow", "overflow@example.com", "N7x!qPz2wR", ""), nil)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次注册 status = %d，期望 429 请求速率桶（body=%q）",
			RegisterRateLimit+1, limited.Code, limited.Body.String())
	}
	if retry, err := strconv.Atoi(limited.Header().Get("Retry-After")); err != nil ||
		retry <= 0 || retry > int(RegisterRateWindow.Seconds()) {
		t.Errorf("Retry-After = %q，期望 1..%v 秒", limited.Header().Get("Retry-After"), RegisterRateWindow.Seconds())
	}
	key := RegisterRateLimitKey("192.0.2.1")
	if !env.mr.Exists(key) {
		t.Fatalf("缺少 B3 桶键 %s", key)
	}
	if !strings.HasPrefix(key, RateLimitKeyPrefix+"RG:") {
		t.Errorf("B3 键前缀 = %q，期望冻结命名空间 %q", key, RateLimitKeyPrefix+"RG:...")
	}

	// Redis 故障 fail-closed。
	down := newAuthTestEnv(t, nil, authTestOptions{})
	down.mr.Close()
	rr := down.request(http.MethodPost, "/api/auth/register",
		registerPayload("downuser", "down@example.com", "N7x!qPz2wR", ""), nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis 故障注册 status = %d，期望 503 fail-closed（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Redis 故障的 503 必须携带 Retry-After")
	}
	if len(down.repo.created) != 0 {
		t.Fatal("速率桶故障时不得创建用户")
	}
}

func TestStatusRequestRateBucket(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	for i := 0; i < PublicReadRateLimit; i++ {
		if rr := env.request(http.MethodGet, "/api/status", nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status status = %d，期望 200（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}

	limited := env.request(http.MethodGet, "/api/status", nil, nil)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次 status status = %d，期望 429（body=%q）",
			PublicReadRateLimit+1, limited.Code, limited.Body.String())
	}
	if retry, err := strconv.Atoi(limited.Header().Get("Retry-After")); err != nil ||
		retry <= 0 || retry > int(PublicReadRateWindow.Seconds()) {
		t.Errorf("Retry-After = %q，期望 1..%v 秒", limited.Header().Get("Retry-After"), PublicReadRateWindow.Seconds())
	}
	key := PublicReadRateLimitKey("192.0.2.1", "/api/status")
	if !env.mr.Exists(key) {
		t.Fatalf("缺少 B8 桶键 %s", key)
	}
	if !strings.HasPrefix(key, RateLimitKeyPrefix+"PB:") {
		t.Errorf("B8 键前缀 = %q，期望冻结命名空间 %q", key, RateLimitKeyPrefix+"PB:...")
	}

	// 公开只读面按 security-07 §2.3 fail-open：Redis 故障仍返回 200。
	down := newAuthTestEnv(t, nil, authTestOptions{})
	down.mr.Close()
	if rr := down.request(http.MethodGet, "/api/status", nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("Redis 故障 status = %d，期望 200 fail-open（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// must_change_password 强制改密
// ---------------------------------------------------------------------------

// TestLoginMustChangePasswordRestrictedSession：临时口令登录后会话处于受限态，
// 除白名单端点外一律 403 password_change_required；清除标记后恢复。
func TestLoginMustChangePasswordRestrictedSession(t *testing.T) {
	ctx := context.Background()
	env := newAuthTestEnv(t, nil, authTestOptions{})
	user := env.repo.add(UserRecord{
		Username: "temp", PasswordHash: authTestPasswordHash(), MustChangePassword: true,
	})
	if err := env.mr.Set(tempPasswordKeyPrefix+strconv.FormatInt(user.ID, 10), "1"); err != nil {
		t.Fatalf("写临时口令标记: %v", err)
	}

	login := env.login("temp", authTestPassword)
	if login.Code != http.StatusOK {
		t.Fatalf("临时口令登录 status = %d，期望 200（body=%q）", login.Code, login.Body.String())
	}
	sid := cookieByName(login, auth.SessionCookieName)
	if sid == nil {
		t.Fatal("登录未下发会话 Cookie")
	}
	restricted, err := env.sessions.PasswordChangeRequired(ctx, user.ID)
	if err != nil || !restricted {
		t.Fatalf("PasswordChangeRequired = (%v, %v)，期望 true", restricted, err)
	}

	// 注册一组测试路由覆盖白名单与非白名单。
	var seen *auth.Principal
	for _, pattern := range []string{
		"GET /api/whoami",
		"GET /api/me",
		"PUT /api/self/setting",
		"POST /api/password",
		"POST /api/2fa/setup",
		"POST /api/tokens",
	} {
		env.router.HandleProtected(pattern, captureHandler(&seen))
	}
	csrf := auth.NewCSRFToken()
	if err := env.sessions.IssueCSRF(ctx, sid.Value, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	do := func(method, path string) *httptest.ResponseRecorder {
		return env.request(method, path, map[string]any{}, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid.Value})
			r.Header.Set(auth.CSRFHeaderName, csrf)
			r.Header.Set("Origin", "http://"+r.Host)
		})
	}

	blocked := do(http.MethodGet, "/api/whoami")
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("受限会话访问受保护端点 status = %d，期望 403（body=%q）", blocked.Code, blocked.Body.String())
	}
	if p := decodeProblem(t, blocked); p.Type != problemTypePasswordChangeRequired {
		t.Errorf("problem.type = %q，期望 %q", p.Type, problemTypePasswordChangeRequired)
	}
	if seen != nil {
		t.Fatal("受限会话不得到达业务 handler")
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodPut, "/api/self/setting"},
		{http.MethodPost, "/api/password"},
		{http.MethodPost, "/api/2fa/setup"},
	} {
		if rr := do(tc.method, tc.path); rr.Code != http.StatusNoContent {
			t.Errorf("白名单端点 %s %s status = %d，期望 204（body=%q）",
				tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}

	if rr := do(http.MethodPost, "/api/tokens"); rr.Code != http.StatusForbidden {
		t.Errorf("非白名单 POST /api/tokens status = %d，期望 403", rr.Code)
	}

	// 清除标记（等价于改密成功）：受限解除。
	if err := env.sessions.ClearPasswordChangeRequired(ctx, user.ID); err != nil {
		t.Fatalf("ClearPasswordChangeRequired: %v", err)
	}
	if rr := do(http.MethodGet, "/api/whoami"); rr.Code != http.StatusNoContent {
		t.Fatalf("解除限制后 GET /api/whoami status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestPasswordChangeLiftsRestriction：POST /api/password 成功后删除受限标记，
// 同时更新 DB 的 must_change_password。
func TestPasswordChangeLiftsRestriction(t *testing.T) {
	env := newPasswordTestEnv(t)
	hash, err := auth.HashPassword("TempPassw0rd!234")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user := env.repo.add(UserRecord{
		Username: "tempuser", Email: "tempuser@example.com",
		PasswordHash: hash, MustChangePassword: true,
	})
	ctx := context.Background()
	if err := env.sessions.MarkPasswordChangeRequired(ctx, user.ID); err != nil {
		t.Fatalf("MarkPasswordChangeRequired: %v", err)
	}
	sid, csrf := env.sessionWithCSRF(user.ID)

	var seen *auth.Principal
	env.router.HandleProtected("GET /api/whoami", captureHandler(&seen))
	get := func() *httptest.ResponseRecorder {
		return env.authedRequest(http.MethodGet, "/api/whoami", nil, sid, csrf)
	}
	if rr := get(); rr.Code != http.StatusForbidden {
		t.Fatalf("改密前受保护端点 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}

	rr := env.authedRequest(http.MethodPost, "/api/password", map[string]any{
		"current_password": "TempPassw0rd!234",
		"new_password":     "NewPassw0rd!234",
	}, sid, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("改密 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	restricted, err := env.sessions.PasswordChangeRequired(ctx, user.ID)
	if err != nil {
		t.Fatalf("PasswordChangeRequired: %v", err)
	}
	if restricted {
		t.Fatal("改密成功后受限标记必须删除")
	}
	if got := env.repo.storedHash(t, user.ID); !auth.VerifyPassword(got, "NewPassw0rd!234") {
		t.Fatal("新口令未落库")
	}
	if u, _ := env.repo.FindUserByID(ctx, user.ID); u == nil || u.MustChangePassword {
		t.Fatal("改密成功后 must_change_password 必须为 false")
	}
	if rr := get(); rr.Code != http.StatusNoContent {
		t.Fatalf("改密后受保护端点 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestLoginTempPasswordExpiredRejected：must_change_password=true 但 24h 标记
// 缺失（过期/被清理）→ 401 temp_password_expired，且不建立会话。
func TestLoginTempPasswordExpiredRejected(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{
		Username: "temp", PasswordHash: authTestPasswordHash(), MustChangePassword: true,
	})

	rr := env.login("temp", authTestPassword)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("临时口令过期 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem JSON 非法: %v", err)
	}
	if p.Type != authProblemTempPasswordExpired {
		t.Errorf("problem.type = %q，期望 %q", p.Type, authProblemTempPasswordExpired)
	}
	if cookieByName(rr, auth.SessionCookieName) != nil {
		t.Fatal("临时口令过期不得建立会话")
	}
	restricted, err := env.sessions.PasswordChangeRequired(context.Background(), 1)
	if err != nil || restricted {
		t.Fatalf("失败登录不得写入受限标记: (%v, %v)", restricted, err)
	}
}

// TestTwoFAChallengeMarksPasswordChangeRestriction 覆盖「ticket 在管理员重置
// 口令之前签发」的竞态：登录路径当时还没写受限标记，challenge 签发会话时必须
// 用最新的 users.must_change_password 再确认一次，否则会得到一个不受限会话。
func TestTwoFAChallengeMarksPasswordChangeRestriction(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{
		Username: "totp", PasswordHash: "unused-but-required", MustChangePassword: true,
	})
	env.enableUser(user)

	ticket := env.issueTicket(user.ID)
	rr := env.challenge(ticket, twoFATestCode, "totp")
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if cookieByName(rr, auth.SessionCookieName) == nil {
		t.Fatal("challenge 成功必须下发会话 Cookie")
	}
	restricted, err := env.sessions.PasswordChangeRequired(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("PasswordChangeRequired: %v", err)
	}
	if !restricted {
		t.Fatal("must_change_password 用户在 2FA challenge 后必须处于受限态")
	}
}
