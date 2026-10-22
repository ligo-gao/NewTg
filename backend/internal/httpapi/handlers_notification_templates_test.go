package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
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

	getView        notify.TemplateView
	getErr         error
	lastGetKey     string
	lastGetChannel notify.Channel
	lastGetLocale  notify.Locale

	updateView notify.TemplateView
	updateErr  error
	lastUpdate notify.UpdateInput

	rendered           notify.Rendered
	previewErr         error
	lastPreviewKey     string
	lastPreviewChannel notify.Channel
	lastPreviewLocale  notify.Locale

	delivery notify.Delivery
	testErr  error
	lastTest notify.TestInput

	deliveries         []notify.DeliveryView
	deliveriesTotal    int
	deliveriesErr      error
	lastDeliveryFilter notify.DeliveryFilter

	restoreView       notify.TemplateView
	restoreErr        error
	lastRestoreKey    string
	lastRestoreChan   notify.Channel
	lastRestoreLocale notify.Locale
	lastRestoreActor  int64

	restoreItems      []notify.TemplateView
	restoreAllErr     error
	lastRestoreFilter notify.RestoreFilter
	lastRestoreAllBy  int64

	deliveryView   notify.DeliveryView
	deliveryErr    error
	lastDeliveryID int64
}

func (f *fakeNotificationService) List(_ context.Context, filter notify.TemplateFilter) ([]notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	return f.listItems, f.listErr
}

func (f *fakeNotificationService) Get(_ context.Context, key string, channel notify.Channel, locale notify.Locale) (notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastGetKey = key
	f.lastGetChannel = channel
	f.lastGetLocale = locale
	return f.getView, f.getErr
}

func (f *fakeNotificationService) Update(_ context.Context, in notify.UpdateInput) (notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUpdate = in
	return f.updateView, f.updateErr
}

func (f *fakeNotificationService) Preview(_ context.Context, key string, channel notify.Channel, locale notify.Locale, _ map[string]any) (notify.Rendered, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPreviewKey = key
	f.lastPreviewChannel = channel
	f.lastPreviewLocale = locale
	return f.rendered, f.previewErr
}

func (f *fakeNotificationService) TestSend(_ context.Context, in notify.TestInput) (notify.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTest = in
	return f.delivery, f.testErr
}

func (f *fakeNotificationService) ListDeliveries(_ context.Context, filter notify.DeliveryFilter) ([]notify.DeliveryView, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDeliveryFilter = filter
	return f.deliveries, f.deliveriesTotal, f.deliveriesErr
}

func (f *fakeNotificationService) RestoreDefault(_ context.Context, key string, channel notify.Channel, locale notify.Locale, actor int64) (notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRestoreKey = key
	f.lastRestoreChan = channel
	f.lastRestoreLocale = locale
	f.lastRestoreActor = actor
	return f.restoreView, f.restoreErr
}

func (f *fakeNotificationService) RestoreDefaults(_ context.Context, filter notify.RestoreFilter, actor int64) ([]notify.TemplateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRestoreFilter = filter
	f.lastRestoreAllBy = actor
	return f.restoreItems, f.restoreAllErr
}

func (f *fakeNotificationService) GetDelivery(_ context.Context, id int64) (notify.DeliveryView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDeliveryID = id
	return f.deliveryView, f.deliveryErr
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
		{"precondition required", notify.ErrExpectationRequired, http.StatusBadRequest},
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

	// A5 残余（G-DOM-37 ⑦）：管理面投递列表的 channel 筛选必须接受 inapp，
	// 且原样透传到仓储过滤（真实值就是 'inapp'，不做 UI 词汇映射）。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/notification-deliveries?channel=inapp", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("channel=inapp = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastDeliveryFilter.Channel != notify.ChannelInApp {
		t.Fatalf("channel 过滤未透传：%+v", service.lastDeliveryFilter)
	}
}

func TestNotificationTemplateChannelInference(t *testing.T) {
	service := &fakeNotificationService{
		getView:    notify.TemplateView{Key: "system.announcement", Channel: notify.ChannelInApp},
		updateView: notify.TemplateView{Key: "system.announcement", Channel: notify.ChannelInApp, Enabled: true},
	}
	mux := notificationTestMux(t, service)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}

	// 单渠道 key 省略 channel → 从注册表推断（email-only）。
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates/auth.verify_code?locale=en", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("单渠道 GET = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastGetChannel != notify.ChannelEmail {
		t.Fatalf("auth.verify_code 应推断 email，得到 %s", service.lastGetChannel)
	}

	// inapp-only（含锁定的 system.announcement）必须无需 channel 即可达。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates/system.announcement?locale=en", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("inapp-only GET = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastGetChannel != notify.ChannelInApp {
		t.Fatalf("system.announcement 应推断 inapp，得到 %s", service.lastGetChannel)
	}

	// 多渠道 key 省略 channel → 400（不得默认 email）。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates/billing.balance_low?locale=en", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("多渠道省略 channel = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
	}

	// 显式 inapp 可达。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-templates/billing.balance_low?locale=en&channel=inapp", "", admin, nil))
	if rr.Code != http.StatusOK || service.lastGetChannel != notify.ChannelInApp {
		t.Fatalf("显式 inapp GET = %d channel=%s，期望 200/inapp", rr.Code, service.lastGetChannel)
	}

	// PUT inapp-only 省略 channel：透传 inapp，且 enabled 省略必须是 nil（F5）。
	body := `{"locale":"en","body":"**{{.title}}** {{.summary}}"}`
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/notification-templates/system.announcement", body, admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("inapp PUT = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastUpdate.Channel != notify.ChannelInApp {
		t.Fatalf("PUT 应推断 inapp，得到 %s", service.lastUpdate.Channel)
	}
	if service.lastUpdate.Enabled != nil {
		t.Fatalf("省略 enabled 应传 nil（保持当前值），得到 %v", *service.lastUpdate.Enabled)
	}
}

func TestNotificationTemplateTestSendRateLimit(t *testing.T) {
	service := &fakeNotificationService{delivery: notify.Delivery{ID: 1, Status: "sent"}}
	mux := notificationTestMux(t, service)
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	body := `{"locale":"en","recipient":"admin@example.com"}`

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
			"/api/notification-templates/system.smtp_test:test", body, manager, nil))
		if rr.Code != http.StatusAccepted {
			t.Fatalf("第 %d 次测试发送 = %d，期望 202（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/system.smtp_test:test", body, manager, nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 4 次测试发送 = %d，期望 429（3 次/分钟）", rr.Code)
	}
}

type recordingExec struct {
	mu   sync.Mutex
	args [][]any
}

func (r *recordingExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, args)
	return fakeSQLResult{}, nil
}

func TestNotificationTemplateUpdateAudited(t *testing.T) {
	exec := &recordingExec{}
	logger := audit.New(exec)
	service := &fakeNotificationService{
		getView:    notify.TemplateView{Key: "auth.verify_code", Enabled: true},
		updateView: notify.TemplateView{Key: "auth.verify_code", Enabled: true},
	}
	handlers, err := NewNotificationTemplateHandlers(NotificationTemplateHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   logger,
	})
	if err != nil {
		t.Fatalf("NewNotificationTemplateHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	manager := &auth.Principal{UserID: 9, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	body := `{"locale":"en","subject":"Your code","body":"code {{.code}}","expect_updated_at":null}`
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/notification-templates/auth.verify_code", body, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.args) != 1 {
		t.Fatalf("审计写入次数 = %d，期望 1", len(exec.args))
	}
	// $1=actor, $3=category, $10=detail（action 由 writer 注入）。
	args := exec.args[0]
	if len(args) < 10 {
		t.Fatalf("审计参数不足：%v", args)
	}
	if args[0] != int64(9) {
		t.Fatalf("审计 actor = %v，期望 9", args[0])
	}
	if args[2] != "operation" {
		t.Fatalf("审计 category = %v，期望 operation", args[2])
	}
	if detail := fmt.Sprint(args[9]); !strings.Contains(detail, "notification_template.update") {
		t.Fatalf("审计 detail 未包含动作名：%v", args[9])
	}
	if detail := fmt.Sprint(args[9]); !strings.Contains(detail, "auth.verify_code") {
		t.Fatalf("审计 detail 未保留 template_key：%v", args[9])
	}
}

func TestNotificationTemplateRestoreDefaultPermissionAndAudit(t *testing.T) {
	exec := &recordingExec{}
	service := &fakeNotificationService{
		getView:     notify.TemplateView{Key: "billing.balance_low", Channel: notify.ChannelEmail, Locale: notify.LocaleEN, Customized: true},
		restoreView: notify.TemplateView{Key: "billing.balance_low", Channel: notify.ChannelEmail, Locale: notify.LocaleEN, Enabled: true},
	}
	handlers, err := NewNotificationTemplateHandlers(NotificationTemplateHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(exec),
	})
	if err != nil {
		t.Fatalf("NewNotificationTemplateHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)

	body := `{"locale":"en","channel":"email"}`
	adminNoPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/billing.balance_low:restore-default", body, adminNoPerm, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}

	manager := &auth.Principal{UserID: 9, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/billing.balance_low:restore-default", body, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("恢复默认 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastRestoreKey != "billing.balance_low" ||
		service.lastRestoreChan != notify.ChannelEmail ||
		service.lastRestoreLocale != notify.LocaleEN ||
		service.lastRestoreActor != 9 {
		t.Fatalf("恢复参数未透传：key=%s channel=%s locale=%s actor=%d",
			service.lastRestoreKey, service.lastRestoreChan, service.lastRestoreLocale, service.lastRestoreActor)
	}

	// 未登记字段由 RBAC 写策略拒绝（BOPLA）。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/billing.balance_low:restore-default",
		`{"locale":"en","unexpected":1}`, manager, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未登记字段 = %d，期望 403", rr.Code)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.args) != 1 {
		t.Fatalf("恢复默认审计写入次数 = %d，期望 1", len(exec.args))
	}
	if detail := fmt.Sprint(exec.args[0][9]); !strings.Contains(detail, "notification_template.restore_default") ||
		!strings.Contains(detail, "billing.balance_low") {
		t.Fatalf("审计 detail 未记录恢复动作：%v", exec.args[0][9])
	}
}

func TestNotificationTemplateRestoreAllFiltersAndAudit(t *testing.T) {
	exec := &recordingExec{}
	service := &fakeNotificationService{
		restoreItems: []notify.TemplateView{
			{Key: "billing.balance_low", Channel: notify.ChannelEmail, Locale: notify.LocaleEN},
			{Key: "billing.balance_low", Channel: notify.ChannelInApp, Locale: notify.LocaleEN},
		},
	}
	handlers, err := NewNotificationTemplateHandlers(NotificationTemplateHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(exec),
	})
	if err != nil {
		t.Fatalf("NewNotificationTemplateHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)

	adminNoPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	body := `{"categories":["billing.balance_low"],"channel":"email","locale":"en","modified_only":true}`
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates:restore-default", body, adminNoPerm, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}

	manager := &auth.Principal{UserID: 3, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates:restore-default", body, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("批量恢复 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"restored":2`) {
		t.Fatalf("批量恢复响应缺少 restored 计数：%s", rr.Body.String())
	}
	filter := service.lastRestoreFilter
	if len(filter.Categories) != 1 || filter.Categories[0] != "billing.balance_low" ||
		filter.Channel != notify.ChannelEmail || filter.Locale != notify.LocaleEN ||
		!filter.ModifiedOnly || service.lastRestoreAllBy != 3 {
		t.Fatalf("批量恢复筛选未透传：%+v actor=%d", filter, service.lastRestoreAllBy)
	}

	// 未注册 category 必须在进入 service 前 400。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates:restore-default", `{"categories":["nope"]}`, manager, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("未注册 category = %d，期望 400", rr.Code)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.args) != 1 {
		t.Fatalf("批量恢复审计写入次数 = %d，期望 1（不逐行写）", len(exec.args))
	}
	if detail := fmt.Sprint(exec.args[0][9]); !strings.Contains(detail, `"scope":"all"`) ||
		!strings.Contains(detail, `"restored":2`) {
		t.Fatalf("批量审计 detail = %v", exec.args[0][9])
	}
}

func TestNotificationDeliveriesDetailAndTimeRange(t *testing.T) {
	nextRetry := time.Date(2026, 10, 20, 12, 5, 0, 0, time.UTC)
	service := &fakeNotificationService{
		deliveries: []notify.DeliveryView{{
			ID: 1, Category: "billing.balance_low", Channel: notify.ChannelEmail,
			Status: "pending", Attempt: 2, NextRetryAt: &nextRetry,
			DedupeKey:         "billing.balance_low:u1:2026-10-20",
			ProviderMessageID: "<abc@example.com>", CreatedAt: nextRetry,
		}},
		deliveriesTotal: 1,
		deliveryView: notify.DeliveryView{
			ID: 7, Category: "billing.balance_low", Status: "sent",
			DedupeKey: "delivery:7", Attempt: 1, CreatedAt: nextRetry,
		},
	}
	mux := notificationTestMux(t, service)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/notification-deliveries?from=2026-10-20T00:00:00Z&to=2026-10-21T00:00:00Z", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("时间范围列表 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastDeliveryFilter.From == nil || service.lastDeliveryFilter.To == nil {
		t.Fatalf("时间范围未透传：%+v", service.lastDeliveryFilter)
	}
	for _, field := range []string{`"attempt":2`, `"next_retry_at"`, `"dedupe_key"`, `"provider_message_id"`} {
		if !strings.Contains(rr.Body.String(), field) {
			t.Fatalf("列表 DTO 缺字段 %s：%s", field, rr.Body.String())
		}
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/notification-deliveries?from=not-a-time", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 from = %d，期望 400", rr.Code)
	}

	// 跨度校验由 service 负责；这里验证 handler 把哨兵错误映射为 400。
	tooLarge := notificationTestMux(t, &fakeNotificationService{deliveriesErr: notify.ErrInvalidTimeRange})
	rr = httptest.NewRecorder()
	tooLarge.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/notification-deliveries?from=2026-01-01T00:00:00Z&to=2026-10-01T00:00:00Z", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("超 90 天 = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-deliveries/7", "", admin, nil))
	if rr.Code != http.StatusOK || service.lastDeliveryID != 7 {
		t.Fatalf("投递详情 = %d id=%d，期望 200/7（body=%s）", rr.Code, service.lastDeliveryID, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"dedupe_key":"delivery:7"`) {
		t.Fatalf("详情 DTO 缺 dedupe_key：%s", rr.Body.String())
	}

	notFound := notificationTestMux(t, &fakeNotificationService{deliveryErr: notify.ErrDeliveryNotFound})
	rr = httptest.NewRecorder()
	notFound.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-deliveries/99", "", admin, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在投递 = %d，期望 404", rr.Code)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/notification-deliveries/abc", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 id = %d，期望 400", rr.Code)
	}
}

func TestNotificationTemplatePreviewInfersInAppOnlyChannel(t *testing.T) {
	service := &fakeNotificationService{rendered: notify.Rendered{Body: "**维护** 摘要"}}
	mux := notificationTestMux(t, service)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}

	// L2：inapp-only 键省略 channel → 从注册表推断 inapp。
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/system.announcement:preview", `{"locale":"en"}`, admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("inapp-only 预览 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.lastPreviewChannel != notify.ChannelInApp {
		t.Fatalf("system.announcement 预览应推断 inapp，得到 %s", service.lastPreviewChannel)
	}

	// 显式 inapp 同样 200。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/system.announcement:preview", `{"locale":"en","channel":"inapp"}`, admin, nil))
	if rr.Code != http.StatusOK || service.lastPreviewChannel != notify.ChannelInApp {
		t.Fatalf("显式 inapp 预览 = %d channel=%s，期望 200/inapp", rr.Code, service.lastPreviewChannel)
	}

	// 多渠道键省略 channel → 400（不得默认 email）。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/notification-templates/billing.balance_low:preview", `{"locale":"en"}`, admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("多渠道预览省略 channel = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
	}
}
