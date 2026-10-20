package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

// POST /api/settings/test/webhook 的单元验收（D2-14）：权限、结果映射、
// 错误分类与固定文案。真实限流/审计在集成测试中覆盖。

type fakeWebhookTester struct {
	configured bool
	result     notify.WebhookResult
	err        error
	host       string
	calls      int
}

func (f *fakeWebhookTester) Configured(context.Context) bool { return f.configured }

func (f *fakeWebhookTester) SendTest(context.Context, []byte) (notify.WebhookResult, error) {
	f.calls++
	return f.result, f.err
}

func (f *fakeWebhookTester) TargetHost() string { return f.host }

func webhookTestMux(t *testing.T, tester WebhookTester, auditLogger *audit.Logger) *http.ServeMux {
	t.Helper()
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: &fakeSettingsService{},
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   auditLogger,
		Idem:    NewMemoryIdempotencyStore(),
		Webhook: tester,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func webhookTestPrincipal() *auth.Principal {
	return &auth.Principal{UserID: 42, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
}

func TestWebhookTestEndpointResultMapping(t *testing.T) {
	cases := []struct {
		name       string
		result     notify.WebhookResult
		err        error
		wantStatus int
		wantOK     bool
		wantCode   int
	}{
		{"success", notify.WebhookResult{StatusCode: 202, OK: true}, nil, 200, true, 202},
		{"non_2xx", notify.WebhookResult{StatusCode: 500, OK: false}, nil, 200, false, 500},
		{"redirect", notify.WebhookResult{StatusCode: 302, OK: false}, nil, 200, false, 302},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tester := &fakeWebhookTester{configured: true, result: tc.result}
			mux := webhookTestMux(t, tester, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "", webhookTestPrincipal(), nil))
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantStatus, rr.Body.String())
			}
			var body webhookTestResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.OK != tc.wantOK || body.StatusCode != tc.wantCode {
				t.Fatalf("body = %+v，期望 ok=%v status_code=%d", body, tc.wantOK, tc.wantCode)
			}
			if tester.calls != 1 {
				t.Fatalf("SendTest 调用次数 = %d", tester.calls)
			}
		})
	}
}

func TestWebhookTestEndpointErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{"not_configured", notify.ErrWebhookNotConfigured, http.StatusUnprocessableEntity, webhookProblemNotConfigured},
		{"blocked_target", notify.ErrWebhookBlockedTarget, http.StatusUnprocessableEntity, webhookProblemTargetBlocked},
		// LOW-1：504 必须是 gateway_timeout，而不是 internal_error。
		{"transport", notify.ErrWebhookTransport, http.StatusGatewayTimeout, problemTypeGatewayTimeout},
		{"internal", errors.New("seal open failed: internal detail"), http.StatusInternalServerError, authProblemInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tester := &fakeWebhookTester{configured: true, err: tc.err}
			mux := webhookTestMux(t, tester, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "", webhookTestPrincipal(), nil))
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantStatus, rr.Body.String())
			}
			var problem struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v（body=%s）", err, rr.Body.String())
			}
			if problem.Type != tc.wantType {
				t.Fatalf("problem.type = %q，期望 %q", problem.Type, tc.wantType)
			}
			if strings.Contains(rr.Body.String(), "internal detail") {
				t.Fatalf("响应泄露内部错误：%s", rr.Body.String())
			}
		})
	}
}

func TestWebhookTestEndpointRequiresSettingsManage(t *testing.T) {
	tester := &fakeWebhookTester{configured: true, result: notify.WebhookResult{StatusCode: 200, OK: true}}
	mux := webhookTestMux(t, tester, nil)

	// 普通用户。
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "",
		&auth.Principal{UserID: 7, Role: rbac.RoleUser}, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}

	// admin 但缺少 settings:manage（功能级拒绝）。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "",
		&auth.Principal{UserID: 8, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}
	if tester.calls != 0 {
		t.Fatal("被拒绝的请求不应触发发送")
	}
}

func TestWebhookTestEndpointWithoutSender(t *testing.T) {
	mux := webhookTestMux(t, nil, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配发送器 = %d，期望 503", rr.Code)
	}
}

// webhookResumeMux 装配 resume 端点；rdb 可为 nil（测试装配缺失）。
func webhookResumeMux(t *testing.T, rdb *redis.Client) *http.ServeMux {
	t.Helper()
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: &fakeSettingsService{},
		Authz:   rbac.NewAuthzMiddleware(),
		Idem:    NewMemoryIdempotencyStore(),
		Redis:   rdb,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

// TestWebhookResumeEndpointClearsPauseState 覆盖 F-08-20「恢复推送」：
// 同时清 paused 与 fail_streak（否则下一次失败会立刻再次暂停），响应
// {paused:false}，且不影响队列行。
func TestWebhookResumeEndpointClearsPauseState(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Set(ctx, notify.WebhookPausedKey, "1", 0).Err(); err != nil {
		t.Fatalf("seed paused: %v", err)
	}
	if err := rdb.Set(ctx, notify.WebhookFailStreakKey, "87", 0).Err(); err != nil {
		t.Fatalf("seed streak: %v", err)
	}

	mux := webhookResumeMux(t, rdb)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/notify-webhook:resume", "",
		webhookTestPrincipal(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	var body webhookResumeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Paused {
		t.Fatalf("body.paused = true，期望 false")
	}
	for _, key := range []string{notify.WebhookPausedKey, notify.WebhookFailStreakKey} {
		if exists, err := rdb.Exists(ctx, key).Result(); err != nil || exists != 0 {
			t.Fatalf("%s 未清除：exists=%d err=%v", key, exists, err)
		}
	}
}

func TestWebhookResumeEndpointRequiresSettingsManage(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	_ = rdb.Set(ctx, notify.WebhookPausedKey, "1", 0).Err()
	mux := webhookResumeMux(t, rdb)

	// 普通用户。
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/notify-webhook:resume", "",
		&auth.Principal{UserID: 7, Role: rbac.RoleUser}, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}
	// admin 但缺少 settings:manage。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/notify-webhook:resume", "",
		&auth.Principal{UserID: 8, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}
	// 被拒绝的请求不得改变暂停状态。
	if exists, _ := rdb.Exists(ctx, notify.WebhookPausedKey).Result(); exists != 1 {
		t.Fatalf("被拒绝的请求清除了 paused 标记")
	}
}

func TestWebhookResumeEndpointWithoutRedis(t *testing.T) {
	mux := webhookResumeMux(t, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/notify-webhook:resume", "",
		webhookTestPrincipal(), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配 Redis = %d，期望 503", rr.Code)
	}
}

// TestWebhookResumeEndpointRedisFailureIs503 是 LOW-3 的回归：Redis Del
// **真的失败**（不是未装配）时，恢复动作没有生效，必须 503 而不是 200。
func TestWebhookResumeEndpointRedisFailureIs503(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1", // 无监听：Del 必定失败
		MaxRetries:   -1,
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	mux := webhookResumeMux(t, rdb)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/notify-webhook:resume", "",
		webhookTestPrincipal(), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis Del 失败 = %d，期望 503（body=%s）", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v（body=%s）", err, rr.Body.String())
	}
	if problem.Type != authProblemInternal {
		t.Fatalf("problem.type = %q，期望 %q", problem.Type, authProblemInternal)
	}
}

// TestWebhookResumeRouteRequiresCSRF 是 LOW-5 的 router 层用例：resume 挂
// SessionAuth → CSRF → OriginRefererCheck 链后，带有效会话但缺 CSRF 头
// 必须 403 且不得清暂停标记；正控证明 403 来自 CSRF 而非链上其它环节。
func TestWebhookResumeRouteRequiresCSRF(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	router, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          newFakeAuthRepo(),
		SessionSecret: []byte(testSessionSecret),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: &fakeSettingsService{},
		Authz:   rbac.NewAuthzMiddleware(),
		Idem:    NewMemoryIdempotencyStore(),
		Redis:   rdb,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	router.HandleProtected("POST /api/settings/notify-webhook:resume", mux)

	sid, err := sessions.Create(ctx, 42, auth.SessionOpts{UA: "router-test", IP: "127.0.0.1", Role: "admin"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	send := func(csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings/notify-webhook:resume", nil)
		req.Host = "app.example.com"
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
		req.Header.Set("Origin", "https://app.example.com")
		if csrf != "" {
			req.Header.Set(auth.CSRFHeaderName, csrf)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	if err := rdb.Set(ctx, notify.WebhookPausedKey, "1", 0).Err(); err != nil {
		t.Fatalf("seed paused: %v", err)
	}
	rr := send("")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v（body=%s）", err, rr.Body.String())
	}
	if problem.Type != problemTypeCSRFFailed {
		t.Fatalf("problem.type = %q，期望 %q", problem.Type, problemTypeCSRFFailed)
	}
	if exists, err := rdb.Exists(ctx, notify.WebhookPausedKey).Result(); err != nil || exists != 1 {
		t.Fatalf("CSRF 拒绝的请求改变了暂停状态：exists=%d err=%v", exists, err)
	}

	// 正控：签发 CSRF 并带上后必须放行并真正清除暂停状态。
	csrf := auth.NewCSRFToken()
	if err := sessions.IssueCSRF(ctx, sid, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	if rr := send(csrf); rr.Code != http.StatusOK {
		t.Fatalf("有效 CSRF = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if exists, err := rdb.Exists(ctx, notify.WebhookPausedKey).Result(); err != nil || exists != 0 {
		t.Fatalf("有效 CSRF 未清除 paused：exists=%d err=%v", exists, err)
	}
}

// TestWebhookTestRouteRequiresCSRF 是 LOW-6 的 router 层用例：端点挂在
// SessionAuth → CSRF → OriginRefererCheck 链后，带有效会话但缺 CSRF 头
// 必须 403，且不得触达发送器。
func TestWebhookTestRouteRequiresCSRF(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	router, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          newFakeAuthRepo(),
		SessionSecret: []byte(testSessionSecret),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	tester := &fakeWebhookTester{configured: true, result: notify.WebhookResult{StatusCode: 200, OK: true}}
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: &fakeSettingsService{},
		Authz:   rbac.NewAuthzMiddleware(),
		Idem:    NewMemoryIdempotencyStore(),
		Webhook: tester,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	// 用独立 mux 承载 Register 的精确模式，再整体挂进受保护子树，
	// 从而真实经过 router 的鉴权链（而不是只测 handler）。
	mux := http.NewServeMux()
	handlers.Register(mux)
	router.HandleProtected("POST /api/settings/test/webhook", mux)

	ctx := context.Background()
	sid, err := sessions.Create(ctx, 42, auth.SessionOpts{UA: "router-test", IP: "127.0.0.1", Role: "admin"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	send := func(csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings/test/webhook", nil)
		req.Host = "app.example.com"
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
		req.Header.Set("Origin", "https://app.example.com")
		if csrf != "" {
			req.Header.Set(auth.CSRFHeaderName, csrf)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	rr := send("")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v（body=%s）", err, rr.Body.String())
	}
	if problem.Type != problemTypeCSRFFailed {
		t.Fatalf("problem.type = %q，期望 %q", problem.Type, problemTypeCSRFFailed)
	}
	if tester.calls != 0 {
		t.Fatal("CSRF 拒绝的请求不得触达发送器")
	}

	// 正控：签发 CSRF 并带上后必须放行，证明 403 来自 CSRF 而不是链上其它环节。
	csrf := auth.NewCSRFToken()
	if err := sessions.IssueCSRF(ctx, sid, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	if rr := send(csrf); rr.Code != http.StatusOK {
		t.Fatalf("有效 CSRF = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if tester.calls != 1 {
		t.Fatalf("SendTest 调用次数 = %d，期望 1", tester.calls)
	}
}

// 保证接口形状与生产发送器一致（编译期钉死），避免 fake 通过而生产漂移。
var _ WebhookTester = (*notify.WebhookSender)(nil)
