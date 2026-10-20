package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	}{
		{"not_configured", notify.ErrWebhookNotConfigured, http.StatusUnprocessableEntity},
		{"blocked_target", notify.ErrWebhookBlockedTarget, http.StatusUnprocessableEntity},
		{"transport", notify.ErrWebhookTransport, http.StatusGatewayTimeout},
		{"internal", errors.New("seal open failed: internal detail"), http.StatusInternalServerError},
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

// 保证接口形状与生产发送器一致（编译期钉死），避免 fake 通过而生产漂移。
var _ WebhookTester = (*notify.WebhookSender)(nil)
