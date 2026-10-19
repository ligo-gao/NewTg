package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

// 通知模板端点的单元验收：权限门、字段白名单、POST 后缀分派与错误映射。
// 真实 SQL/回退链在 internal/notify 的集成测试覆盖（避免两包并发操作同表）。

type fakeNotificationService struct {
	mu sync.Mutex

	listItems  []notify.TemplateView
	listErr    error
	lastFilter notify.TemplateFilter

	getView notify.TemplateView
	getErr  error

	updateView notify.TemplateView
	updateErr  error
	lastUpdate notify.UpdateInput

	rendered   notify.Rendered
	previewErr error

	delivery notify.Delivery
	testErr  error
	lastTest notify.TestInput

	deliveries      []notify.DeliveryView
	deliveriesTotal int
	deliveriesErr   error
}

func (f *fakeNotificationService) List(_ context.Context, filter notify.TemplateFilter) ([]notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	return f.listItems, f.listErr
}

func (f *fakeNotificationService) Get(_ context.Context, _ string, _ notify.Channel, _ notify.Locale) (notify.TemplateView, error) {
	return f.getView, f.getErr
}

func (f *fakeNotificationService) Update(_ context.Context, in notify.UpdateInput) (notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUpdate = in
	return f.updateView, f.updateErr
}

func (f *fakeNotificationService) Preview(_ context.Context, _ string, _ notify.Channel, _ notify.Locale, _ map[string]any) (notify.Rendered, error) {
	return f.rendered, f.previewErr
}

func (f *fakeNotificationService) TestSend(_ context.Context, in notify.TestInput) (notify.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTest = in
	return f.delivery, f.testErr
}

func (f *fakeNotificationService) ListDeliveries(_ context.Context, _ notify.DeliveryFilter) ([]notify.DeliveryView, int, error) {
	return f.deliveries, f.deliveriesTotal, f.deliveriesErr
}

func notificationTestMux(t *testing.T, service NotificationTemplateService) *http.ServeMux {
	t.Helper()
	handlers, err := NewNotificationTemplateHandlers(NotificationTemplateHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
	})
	if err != nil {
		t.Fatalf("NewNotificationTemplateHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func TestNotificationTemplateListRequiresAdmin(t *testing.T) {
	service := &fakeNotificationService{listItems: []notify.TemplateView{{Key: "auth.verify_code"}}}
	mux := notificationTestMux(t, service)

	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 GET = %d，期望 403", rr.Code)
	}

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin GET = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"items"`) {
		t.Fatalf("响应缺少 items：%s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates?category=unknown", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("未注册 category = %d，期望 400", rr.Code)
	}
}

func TestNotificationTemplateGetUnknownKeyIs404(t *testing.T) {
	service := &fakeNotificationService{getErr: notify.ErrUnknownTemplate}
	mux := notificationTestMux(t, service)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates/auth.unknown?locale=en", "", admin, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知 key = %d，期望 404（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestNotificationTemplatePutRequiresSettingsManageAndFields(t *testing.T) {
	service := &fakeNotificationService{updateView: notify.TemplateView{Key: "auth.verify_code", Locale: notify.LocaleEN}}
	mux := notificationTestMux(t, service)
	body := `{"locale":"en","subject":"Your code","body":"code {{.code}}","expect_updated_at":null}`

	adminNoPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/notification-templates/auth.verify_code", body, adminNoPerm, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}

	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/notification-templates/auth.verify_code", body, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("settings:manage PUT = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastUpdate.Key != "auth.verify_code" || service.lastUpdate.Channel != notify.ChannelEmail {
		t.Fatalf("PUT 参数未透传：%+v", service.lastUpdate)
	}

	// 未登记字段由 RBAC 写策略拒绝（BOPLA），不静默丢弃。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/notification-templates/auth.verify_code",
		`{"locale":"en","subject":"x","body":"y","unexpected":1}`, manager, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未登记字段 = %d，期望 403", rr.Code)
	}
}

func TestNotificationTemplatePutErrorMapping(t *testing.T) {
	muxService := func(err error) *http.ServeMux {
		return notificationTestMux(t, &fakeNotificationService{updateErr: err})
	}
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	body := `{"locale":"en","subject":"x","body":"y"}`

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"conflict", notify.ErrConflict, http.StatusConflict},
		{"unknown variable", &notify.ErrUnknownVariable{Variable: "foo"}, http.StatusUnprocessableEntity},
		{"invalid construct", &notify.ErrInvalidConstruct{Construct: "{{.x | upper}}"}, http.StatusUnprocessableEntity},
		{"missing variable", &notify.ErrMissingVariable{Variable: "code"}, http.StatusUnprocessableEntity},
		{"locked", notify.ErrLockedTemplate, http.StatusUnprocessableEntity},
		{"unknown key", notify.ErrUnknownTemplate, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := muxService(tc.err)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/notification-templates/auth.verify_code", body, manager, nil))
			if rr.Code != tc.want {
				t.Fatalf("%s = %d，期望 %d（body=%s）", tc.name, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestNotificationTemplatePreviewIsAdminAndPlainText(t *testing.T) {
	service := &fakeNotificationService{rendered: notify.Rendered{Subject: "Your code: 428173", Body: "code 428173"}}
	mux := notificationTestMux(t, service)

	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/auth.verify_code:preview", `{"locale":"en"}`, user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户预览 = %d，期望 403", rr.Code)
	}

	// preview 是读语义：admin 无需 settings:manage。
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/auth.verify_code:preview", `{"locale":"en"}`, admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin 预览 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"subject"`) || !strings.Contains(rr.Body.String(), `"body"`) {
		t.Fatalf("预览响应缺少 subject/body：%s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/not-an-action", `{}`, admin, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知后缀 = %d，期望 404", rr.Code)
	}
}

func TestNotificationTemplateTestSendPermissionAndSMTPStatus(t *testing.T) {
	service := &fakeNotificationService{delivery: notify.Delivery{ID: 42, Status: "sent"}}
	mux := notificationTestMux(t, service)
	body := `{"locale":"en","recipient":"admin@example.com"}`

	adminNoPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/system.smtp_test:test", body, adminNoPerm, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage 测试发送 = %d，期望 403", rr.Code)
	}

	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/system.smtp_test:test", body, manager, nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("测试发送 = %d，期望 202（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastTest.Recipient != "admin@example.com" || service.lastTest.ActorID != 1 {
		t.Fatalf("测试发送参数未透传：%+v", service.lastTest)
	}

	notConfigured := notificationTestMux(t, &fakeNotificationService{testErr: notify.ErrSMTPNotConfigured})
	rr = httptest.NewRecorder()
	notConfigured.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/system.smtp_test:test", body, manager, nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("SMTP 未配置 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestNotificationDeliveriesRequiresAdminPagination(t *testing.T) {
	service := &fakeNotificationService{
		deliveries:      []notify.DeliveryView{{ID: 1, Status: "sent", CreatedAt: time.Now()}},
		deliveriesTotal: 1,
	}
	mux := notificationTestMux(t, service)

	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-deliveries", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户投递记录 = %d，期望 403", rr.Code)
	}

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-deliveries?p=1&page_size=20&status=sent", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin 投递记录 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"total"`) || !strings.Contains(rr.Body.String(), `"page_size"`) {
		t.Fatalf("分页字段缺失：%s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-deliveries?page_size=101", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("超限 page_size = %d，期望 400", rr.Code)
	}
}
