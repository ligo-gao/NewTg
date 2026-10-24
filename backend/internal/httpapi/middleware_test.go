package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
)

const testSessionSecret = "middleware-test-secret-0123456789abcdef"

// 冻结的 RFC 7807 type URI（modules-03/modules-09 的既有命名约定
// `https://newtg.dev/problems/<slug>`；csrf_failed 由 security-01 §2.2 冻结）。
const (
	wantProblemUnauthorized = "https://newtg.dev/problems/unauthorized"
	wantProblemCSRFFailed   = "https://newtg.dev/problems/csrf_failed"
)

// fakeTokenLookup 用内存表替代 Task 12 的真实实现：本任务只约束
// 「接收 SHA-256 哈希、返回 TokenMeta」这一层契约。
type fakeTokenLookup struct {
	metas map[string]*auth.TokenMeta
	err   error
	calls []string
}

func (f *fakeTokenLookup) Resolve(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
	f.calls = append(f.calls, keyHash)
	if f.err != nil {
		return nil, f.err
	}
	meta, ok := f.metas[keyHash]
	if !ok {
		return nil, errors.New("fakeTokenLookup: token not found")
	}
	return meta, nil
}

// newTestSessions 用 miniredis 替代真 Redis；MaxRetries=-1 让 Redis-down 用例
// 立即得到错误而不是等待默认退避。
func newTestSessions(t *testing.T) (*auth.SessionManager, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return auth.NewSessionManager(rdb, []byte(testSessionSecret)), mr
}

func mustSession(t *testing.T, m *auth.SessionManager, userID int64, role string) string {
	t.Helper()
	sid, err := m.Create(context.Background(), userID, auth.SessionOpts{
		UA:   "MiddlewareTest/1.0",
		IP:   "203.0.113.10",
		Role: role,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return sid
}

func withSessionCookie(req *http.Request, sid string) *http.Request {
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	return req
}

// captureHandler 把注入的 Principal 交给测试；未注入时回 500，这样「中间件误放行」
// 会以 500 暴露出来，而不是被当成成功。
func captureHandler(seen **auth.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			http.Error(w, "principal missing from context", http.StatusInternalServerError)
			return
		}
		*seen = p
		w.WriteHeader(http.StatusNoContent)
	})
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
}

func decodeProblem(t *testing.T, rr *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	var p problemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem 体不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return p
}

// TestSessionAuthNoCredentials 覆盖 brief (a)：无 Cookie 无 Bearer → 401，
// 且业务 handler 不得被调用。
func TestSessionAuthNoCredentials(t *testing.T) {
	m, _ := newTestSessions(t)
	var seen *auth.Principal
	h := SessionAuth(m, &fakeTokenLookup{})(captureHandler(&seen))

	rr := serve(h, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d，期望 401", rr.Code)
	}
	if seen != nil {
		t.Fatal("未认证请求不应把 Principal 传给业务 handler")
	}
	if p := decodeProblem(t, rr); p.Type != wantProblemUnauthorized || p.Status != http.StatusUnauthorized {
		t.Errorf("problem = %+v，期望 type=%s status=401", p, wantProblemUnauthorized)
	}
}

// TestSessionAuthCookieInjectsPrincipal 覆盖 brief (b)：合法 Cookie 会话路径。
func TestSessionAuthCookieInjectsPrincipal(t *testing.T) {
	m, _ := newTestSessions(t)
	sid := mustSession(t, m, 42, "user")

	var seen *auth.Principal
	h := SessionAuth(m, &fakeTokenLookup{})(captureHandler(&seen))
	rr := serve(h, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if seen == nil {
		t.Fatal("合法会话请求必须注入 Principal")
	}
	if seen.UserID != 42 {
		t.Errorf("UserID = %d，期望 42", seen.UserID)
	}
	if seen.Role != "user" {
		t.Errorf("Role = %q，期望 user（来自会话 auth_level）", seen.Role)
	}
	if seen.TokenID != nil {
		t.Errorf("会话路径 TokenID = %v，期望 nil", seen.TokenID)
	}
}

// TestSessionAuthBearerInjectsTokenID 覆盖 brief (c)：Bearer 路径 TokenID 非 nil，
// UserID 来自令牌元数据，且 Resolve 收到的是 SHA-256 哈希而非明文。
func TestSessionAuthBearerInjectsTokenID(t *testing.T) {
	m, _ := newTestSessions(t)
	token := "nkt-" + strings.Repeat("A", 48)

	lookup := &fakeTokenLookup{metas: map[string]*auth.TokenMeta{
		auth.HashTokenKey(token): {ID: 9, UserID: 7, Status: "enabled", UnlimitedQuota: true},
	}}
	var seen *auth.Principal
	h := SessionAuth(m, lookup)(captureHandler(&seen))

	req := httptest.NewRequest(http.MethodGet, "/api/usage/token", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := serve(h, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if seen == nil {
		t.Fatal("合法 Bearer 请求必须注入 Principal")
	}
	if seen.UserID != 7 {
		t.Errorf("UserID = %d，期望来自令牌的 7", seen.UserID)
	}
	if seen.TokenID == nil || *seen.TokenID != 9 {
		t.Errorf("TokenID = %v，期望指向 9", seen.TokenID)
	}
	if len(lookup.calls) != 1 || lookup.calls[0] != auth.HashTokenKey(token) {
		t.Errorf("Resolve 收到 %v，期望单次调用且参数为 SHA-256 哈希 %q", lookup.calls, auth.HashTokenKey(token))
	}
}

// TestSessionAuthBearerRejectsUnusableToken 覆盖 relay-01 §3.3 的令牌状态判定：
// 非 enabled / 过期 / 额度耗尽 / 查询失败一律 401（统一文案，不泄露原因）。
func TestSessionAuthBearerRejectsUnusableToken(t *testing.T) {
	token := "nkt-" + strings.Repeat("B", 48)
	hash := auth.HashTokenKey(token)
	expired := time.Now().Add(-time.Minute)

	cases := []struct {
		name   string
		meta   *auth.TokenMeta
		authz  string
		lookup *fakeTokenLookup
	}{
		{
			name:   "status disabled",
			meta:   &auth.TokenMeta{ID: 1, UserID: 7, Status: "disabled", UnlimitedQuota: true},
			lookup: &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}},
		},
		{
			name:   "expired",
			meta:   &auth.TokenMeta{ID: 1, UserID: 7, Status: "enabled", ExpiresAt: &expired, UnlimitedQuota: true},
			lookup: &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}},
		},
		{
			name:   "quota exhausted",
			meta:   &auth.TokenMeta{ID: 1, UserID: 7, Status: "enabled", RemainQuota: 0},
			lookup: &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}},
		},
		{
			name:   "lookup error",
			lookup: &fakeTokenLookup{err: errors.New("redis down")},
			authz:  "Bearer " + token,
		},
		{
			name:   "log missing",
			lookup: &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}},
			authz:  "Bearer " + token,
		},
		{
			name:   "unknown token prefix rejected on management path",
			lookup: &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}},
			authz:  "Bearer xsk-not-a-management-token",
		},
		{
			name:   "wrong auth scheme",
			lookup: &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}},
			authz:  "Basic " + token,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestSessions(t)
			lookup := tc.lookup
			if tc.meta != nil {
				lookup.metas[hash] = tc.meta
			}
			authz := tc.authz
			if authz == "" {
				authz = "Bearer " + token
			}
			var seen *auth.Principal
			h := SessionAuth(m, lookup)(captureHandler(&seen))
			req := httptest.NewRequest(http.MethodGet, "/api/usage/token", nil)
			req.Header.Set("Authorization", authz)

			rr := serve(h, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
			}
			if seen != nil {
				t.Fatal("不可用令牌不得注入 Principal")
			}
		})
	}
}

// TestSessionAuthCookiePathWinsOverBearer 冻结「Cookie 优先」的 fail-closed 选择：
// 浏览器请求一旦携带会话 Cookie，就按 Cookie 路径处理（CSRF 必须生效），
// 不能因为同时带了 Authorization 就绕过 CSRF。
func TestSessionAuthCookiePathWinsOverBearer(t *testing.T) {
	m, _ := newTestSessions(t)
	sid := mustSession(t, m, 42, "admin")
	token := "nkt-" + strings.Repeat("C", 48)
	lookup := &fakeTokenLookup{metas: map[string]*auth.TokenMeta{
		auth.HashTokenKey(token): {ID: 9, UserID: 7, Status: "enabled", UnlimitedQuota: true},
	}}

	var seen *auth.Principal
	h := SessionAuth(m, lookup)(captureHandler(&seen))
	req := withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := serve(h, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if seen == nil || seen.UserID != 42 || seen.TokenID != nil {
		t.Fatalf("Principal = %+v，期望 Cookie 会话身份（UserID=42, TokenID=nil）", seen)
	}
	if len(lookup.calls) != 0 {
		t.Error("Cookie 路径不应调用令牌查询")
	}
}

// TestSessionAuthRefreshesSlidingWindow 断言每次成功的 Cookie 鉴权都会调用
// SessionManager.Touch：不刷新则 7d/30min 滑动超时退化为「创建后到期即失效」，
// last_seen/LRU 驱逐顺序也不会推进（security-01 I3、R-06）。
// 连续两次请求，第二次的 last_seen_at 必须严格晚于第一次。
func TestSessionAuthRefreshesSlidingWindow(t *testing.T) {
	m, mr := newTestSessions(t)
	sid := mustSession(t, m, 42, "user")

	key := "sess:" + sid
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	mr.HSet(key, "last_seen_at", old)

	h := SessionAuth(m, &fakeTokenLookup{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if rr := serve(h, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid)); rr.Code != http.StatusNoContent {
		t.Fatalf("第一次请求 status = %d，期望 204", rr.Code)
	}
	firstSeen := mr.HGet(key, "last_seen_at")
	if firstSeen == old {
		t.Fatal("鉴权成功后 last_seen_at 未刷新，滑动超时不会生效")
	}

	// 保证两次 Touch 的墙钟时间可区分（RFC3339Nano 在 Linux 上是纳秒分辨率，
	// 2ms 余量远大于调度抖动）。
	time.Sleep(2 * time.Millisecond)
	if rr := serve(h, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid)); rr.Code != http.StatusNoContent {
		t.Fatalf("第二次请求 status = %d，期望 204", rr.Code)
	}
	secondSeen := mr.HGet(key, "last_seen_at")

	firstAt, err := time.Parse(time.RFC3339Nano, firstSeen)
	if err != nil {
		t.Fatalf("解析第一次 last_seen_at %q: %v", firstSeen, err)
	}
	secondAt, err := time.Parse(time.RFC3339Nano, secondSeen)
	if err != nil {
		t.Fatalf("解析第二次 last_seen_at %q: %v", secondSeen, err)
	}
	if !secondAt.After(firstAt) {
		t.Errorf("连续请求后 last_seen_at 未前进：first=%v second=%v", firstAt, secondAt)
	}
}

// TestSessionAuthRedisDownFailsClosed 覆盖 brief (g)/TC-A-16：Redis 不可用时
// 必须 401，绝不能把「查询失败」当成匿名或放行。
func TestSessionAuthRedisDownFailsClosed(t *testing.T) {
	m, mr := newTestSessions(t)
	sid := mustSession(t, m, 42, "user")
	mr.Close()

	var seen *auth.Principal
	h := SessionAuth(m, &fakeTokenLookup{})(captureHandler(&seen))
	rr := serve(h, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d，期望 401（fail-closed）", rr.Code)
	}
	if seen != nil {
		t.Fatal("Redis 不可用时不得注入 Principal")
	}
	if p := decodeProblem(t, rr); p.Type != wantProblemUnauthorized {
		t.Errorf("problem.type = %q，期望 %q", p.Type, wantProblemUnauthorized)
	}
}

// TestSessionAuthUniform401 断言所有认证失败（缺凭据/坏 Cookie/未知会话/坏令牌/
// Redis 故障）返回逐字节相同的响应：任何差异性都会成为凭据有效性/路径的探测信道。
func TestSessionAuthUniform401(t *testing.T) {
	var bodies []string
	run := func(name string, build func(t *testing.T) (*auth.SessionManager, auth.TokenLookup, *http.Request)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			m, lookup, req := build(t)
			var seen *auth.Principal
			rr := serve(SessionAuth(m, lookup)(captureHandler(&seen)), req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
			}
			if seen != nil {
				t.Fatal("失败路径不得注入 Principal")
			}
			bodies = append(bodies, rr.Body.String())
		})
	}

	run("no credentials", func(t *testing.T) (*auth.SessionManager, auth.TokenLookup, *http.Request) {
		m, _ := newTestSessions(t)
		return m, &fakeTokenLookup{}, httptest.NewRequest(http.MethodGet, "/api/me", nil)
	})
	run("malformed session cookie", func(t *testing.T) (*auth.SessionManager, auth.TokenLookup, *http.Request) {
		m, _ := newTestSessions(t)
		return m, &fakeTokenLookup{}, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), "not-a-session-id")
	})
	run("unknown session", func(t *testing.T) (*auth.SessionManager, auth.TokenLookup, *http.Request) {
		m, _ := newTestSessions(t)
		sid := base64.RawURLEncoding.EncodeToString(make([]byte, auth.SessionIDBytes))
		return m, &fakeTokenLookup{}, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid)
	})
	run("unknown bearer", func(t *testing.T) (*auth.SessionManager, auth.TokenLookup, *http.Request) {
		m, _ := newTestSessions(t)
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.Header.Set("Authorization", "Bearer nkt-"+strings.Repeat("D", 48))
		return m, &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}}, req
	})
	run("redis down", func(t *testing.T) (*auth.SessionManager, auth.TokenLookup, *http.Request) {
		m, mr := newTestSessions(t)
		sid := mustSession(t, m, 42, "user")
		mr.Close()
		return m, &fakeTokenLookup{}, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid)
	})

	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("认证失败响应不一致：\ncase0=%q\ncase%d=%q", bodies[0], i, bodies[i])
		}
	}
}

// TestCSRFRequiresTokenOnCookiePath 覆盖 brief (d)/TC-A-03：Cookie 路径的
// POST/PUT/PATCH/DELETE 缺/错 X-CSRF-Token → 403 csrf_failed（RFC 7807）。
func TestCSRFRequiresTokenOnCookiePath(t *testing.T) {
	m, _ := newTestSessions(t)
	sid := mustSession(t, m, 42, "user")
	csrfToken := auth.NewCSRFToken()
	if err := m.IssueCSRF(context.Background(), sid, csrfToken); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}

	var seen *auth.Principal
	chain := SessionAuth(m, &fakeTokenLookup{})(CSRF(m)(captureHandler(&seen)))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method+"/missing", func(t *testing.T) {
			rr := serve(chain, withSessionCookie(httptest.NewRequest(method, "/api/me", nil), sid))
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
			}
			if p := decodeProblem(t, rr); p.Type != wantProblemCSRFFailed || p.Status != http.StatusForbidden {
				t.Errorf("problem = %+v，期望 type=%s status=403", p, wantProblemCSRFFailed)
			}
		})
		t.Run(method+"/wrong", func(t *testing.T) {
			req := withSessionCookie(httptest.NewRequest(method, "/api/me", nil), sid)
			req.Header.Set(auth.CSRFHeaderName, "wrong-token")
			if rr := serve(chain, req); rr.Code != http.StatusForbidden {
				t.Fatalf("错误 CSRF 令牌 status = %d，期望 403", rr.Code)
			}
		})
		t.Run(method+"/ok", func(t *testing.T) {
			req := withSessionCookie(httptest.NewRequest(method, "/api/me", nil), sid)
			req.Header.Set(auth.CSRFHeaderName, csrfToken)
			if rr := serve(chain, req); rr.Code != http.StatusNoContent {
				t.Fatalf("正确 CSRF 令牌 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
			}
		})
	}

	// 安全方法不需要 CSRF 令牌。
	if rr := serve(chain, withSessionCookie(httptest.NewRequest(http.MethodGet, "/api/me", nil), sid)); rr.Code != http.StatusNoContent {
		t.Fatalf("GET status = %d，期望 204", rr.Code)
	}
}

// TestCSRFBearerExempt 覆盖 brief (e)/TC-A-03 后半：Bearer 请求不触发 CSRF；
// 但只要请求携带会话 Cookie，CSRF 就必须生效（与 SessionAuth 的 Cookie 优先一致）。
func TestCSRFBearerExempt(t *testing.T) {
	m, _ := newTestSessions(t)
	token := "nkt-" + strings.Repeat("E", 48)
	lookup := &fakeTokenLookup{metas: map[string]*auth.TokenMeta{
		auth.HashTokenKey(token): {ID: 9, UserID: 7, Status: "enabled", UnlimitedQuota: true},
	}}
	var seen *auth.Principal
	chain := SessionAuth(m, lookup)(CSRF(m)(captureHandler(&seen)))

	req := httptest.NewRequest(http.MethodPost, "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if rr := serve(chain, req); rr.Code != http.StatusNoContent {
		t.Fatalf("Bearer POST status = %d，期望 204（不得触发 CSRF，body=%q）", rr.Code, rr.Body.String())
	}
	if seen == nil || seen.TokenID == nil {
		t.Fatal("Bearer POST 应注入带 TokenID 的 Principal")
	}

	// Cookie + Bearer 同时出现时按 Cookie 路径处理：缺 CSRF → 403。
	sid := mustSession(t, m, 42, "user")
	req = withSessionCookie(httptest.NewRequest(http.MethodPost, "/api/tokens", nil), sid)
	req.Header.Set("Authorization", "Bearer "+token)
	if rr := serve(chain, req); rr.Code != http.StatusForbidden {
		t.Fatalf("Cookie+Bearer POST 缺 CSRF status = %d，期望 403", rr.Code)
	}
}

// TestOriginRefererCheck 覆盖 brief (f)：Origin/Referer 同源校验（纵深）。
// 选择：Cookie 路径的非安全方法缺 Origin 与 Referer 时拒绝（fail-closed）——
// 浏览器发起的非安全请求必带 Origin；缺失意味着无法建立同源来源，
// 而 Bearer/非浏览器路径不受影响（无 Cookie 即跳过）。
func TestOriginRefererCheck(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	cases := []struct {
		name    string
		method  string
		cookie  bool
		origin  string
		referer string
		want    int
	}{
		{name: "同源 Origin", method: http.MethodPost, cookie: true, origin: "https://app.example.com", want: http.StatusNoContent},
		{name: "同源 Referer（带路径）", method: http.MethodPost, cookie: true, referer: "https://app.example.com/console/keys", want: http.StatusNoContent},
		{name: "跨源 Origin", method: http.MethodPost, cookie: true, origin: "https://evil.example", want: http.StatusForbidden},
		{name: "Origin null（不透明来源）", method: http.MethodPost, cookie: true, origin: "null", want: http.StatusForbidden},
		{name: "缺 Origin 与 Referer", method: http.MethodPost, cookie: true, want: http.StatusForbidden},
		{name: "端口不同", method: http.MethodPost, cookie: true, origin: "https://app.example.com:8443", want: http.StatusForbidden},
		{name: "安全方法不校验", method: http.MethodGet, cookie: true, origin: "https://evil.example", want: http.StatusNoContent},
		{name: "无 Cookie 的 Bearer 路径豁免", method: http.MethodPost, cookie: false, origin: "https://evil.example", want: http.StatusNoContent},
		{name: "无 Cookie 且无来源头", method: http.MethodPost, cookie: false, want: http.StatusNoContent},
	}

	// 白名单故意带尾斜杠：构造期归一化必须让它与无斜杠的 Origin 匹配。
	h := OriginRefererCheck([]string{"https://app.example.com/"})(handler)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://app.example.com/api/me", nil)
			req.Host = "app.example.com"
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-id"})
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				req.Header.Set("Referer", tc.referer)
			}
			rr := serve(h, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d，期望 %d（body=%q）", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusForbidden {
				if p := decodeProblem(t, rr); p.Type != wantProblemCSRFFailed {
					t.Errorf("problem.type = %q，期望 %q", p.Type, wantProblemCSRFFailed)
				}
			}
		})
	}
}

// TestOriginRefererCheckHostFallback 冻结「无 PUBLIC_BASE_URL 白名单时按请求 Host
// 同源」的回退：本地开发（PUBLIC_BASE_URL 为空）仍能校验，生产应显式配置白名单。
func TestOriginRefererCheckHostFallback(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := OriginRefererCheck(nil)(handler)

	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/api/me", nil)
	req.Host = "app.example.com"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-id"})
	req.Header.Set("Origin", "https://app.example.com")
	if rr := serve(h, req); rr.Code != http.StatusNoContent {
		t.Fatalf("Host 同源回退 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "http://app.example.com/api/me", nil)
	req.Host = "app.example.com"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-id"})
	req.Header.Set("Origin", "https://other.example.com")
	if rr := serve(h, req); rr.Code != http.StatusForbidden {
		t.Fatalf("不同 Host status = %d，期望 403", rr.Code)
	}
}

func TestBearerCredentialPrefixes(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"Bearer sk-abc", true},
		{"Bearer nkt-abc", true},
		{"Bearer xsk-abc", false},
		{"Bearer sk-", false},
		{"Bearer nkt-", false},
		{"Basic sk-abc", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", tc.header)
		if _, ok := bearerCredential(req); ok != tc.want {
			t.Fatalf("bearerCredential(%q) = %v, want %v", tc.header, ok, tc.want)
		}
	}
}
