package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
)

// 本文件是设置端点的单元验收：权限门、错误映射、幂等重放与审计脱敏。
// 真实事务/乐观锁在 settings 包的集成测试中覆盖。

type fakeSettingsService struct {
	mu      sync.Mutex
	getView settings.SectionView
	// getQueue 非空时按顺序返回（GET 前后快照不同的场景，如部署设置审计）。
	getQueue  []settings.SectionView
	getErr    error
	putResult settings.UpdateResult
	putErr    error
	getCalls  int
	putCalls  int
	lastInput settings.UpdateInput
}

func (f *fakeSettingsService) Get(_ context.Context, section string) (settings.SectionView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return settings.SectionView{}, f.getErr
	}
	view := f.getView
	if len(f.getQueue) > 0 {
		view = f.getQueue[0]
		f.getQueue = f.getQueue[1:]
	}
	view.Section = section
	return view, nil
}

func (f *fakeSettingsService) Put(_ context.Context, in settings.UpdateInput) (settings.UpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	f.lastInput = in
	if f.putErr != nil {
		return settings.UpdateResult{}, f.putErr
	}
	return f.putResult, nil
}

func settingsTestMux(t *testing.T, service SettingsService, auditLogger *audit.Logger) (*http.ServeMux, *SettingsHandlers) {
	t.Helper()
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   auditLogger,
		Idem:    NewMemoryIdempotencyStore(),
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux, handlers
}

func settingsRequest(method, target string, body string, principal *auth.Principal, headers map[string]string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if principal != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	}
	return req
}

func TestSettingsGetRequiresAdmin(t *testing.T) {
	service := &fakeSettingsService{getView: settings.SectionView{Items: []settings.Item{}}}
	mux, _ := settingsTestMux(t, service, nil)

	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings?section=site", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 GET = %d，期望 403", rr.Code)
	}

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings?section=site", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin GET = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings", "", admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 section = %d，期望 400", rr.Code)
	}
}

// R-26：读设置与写设置同一权限门（settings:manage）。
//
// 契约声明 GET 需要 settings:manage；受限管理员（显式权限集不含该键）
// 不得读取整段配置，即使它仍是 admin 角色。
func TestSettingsGetRequiresSettingsManage(t *testing.T) {
	service := &fakeSettingsService{getView: settings.SectionView{Items: []settings.Item{}}}
	mux, _ := settingsTestMux(t, service, nil)

	limited := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermUsersRead}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings?section=site", "", limited, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("无 settings:manage 的 admin GET = %d，期望 403", rr.Code)
	}

	allowed := &auth.Principal{UserID: 3, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermSettingsManage}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings?section=site", "", allowed, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("带 settings:manage 的 admin GET = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

// 未注入 RBAC 的装配退回「仅管理员角色」的只读门（生产装配恒注入 Authz，
// 该分支服务于测试/嵌入式装配；这里覆盖它，避免回退变成死代码）。
func TestSettingsGetWithoutAuthzUsesAdminRoleGate(t *testing.T) {
	service := &fakeSettingsService{getView: settings.SectionView{Items: []settings.Item{}}}
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{Service: service})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)

	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings?section=site", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 GET = %d，期望 403", rr.Code)
	}

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings?section=site", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin GET（无 RBAC 注入）= %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

// 契约要求 values 必填：缺失/null 不得静默当成空更新成功；{} 合法。
func TestSettingsPutRequiresValuesMember(t *testing.T) {
	service := &fakeSettingsService{putResult: settings.UpdateResult{Section: "site", Values: map[string]any{}}}
	mux, _ := settingsTestMux(t, service, nil)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site",
		`{"expect_updated_at":null}`, admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 values = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
	}
	if service.putCalls != 0 {
		t.Fatal("缺 values 不得落到服务层")
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site",
		`{"values":null}`, admin, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("显式 null values = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site",
		`{"values":{}}`, admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("空 values 对象 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestSettingsPutRequiresSettingsManage(t *testing.T) {
	service := &fakeSettingsService{putResult: settings.UpdateResult{Section: "site", Values: map[string]any{}}}
	mux, _ := settingsTestMux(t, service, nil)
	body := `{"section":"site","values":{"site.title":"X"},"expect_updated_at":null}`

	// admin 但无 settings:manage：功能级拒绝。
	adminNoPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site", body, adminNoPerm, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}

	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site", body, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("有权限 PUT = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if service.putCalls != 1 {
		t.Fatalf("Put 调用次数 = %d，期望 1", service.putCalls)
	}
	if service.lastInput.Actor != 1 {
		t.Fatalf("审计 actor = %d，期望 1", service.lastInput.Actor)
	}
}

func TestSettingsPutErrorMapping(t *testing.T) {
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	body := `{"section":"site","values":{"site.title":""},"expect_updated_at":null}`

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"版本冲突", settings.ErrConflict, http.StatusConflict},
		{"未知 section", settings.ErrUnknownSection, http.StatusBadRequest},
		{"校验失败", &settings.ValidationError{Key: "site.title", Message: "must not be empty"}, http.StatusBadRequest},
		{"锁定键", &settings.LockedError{Key: "nav.console.require_auth"}, http.StatusUnprocessableEntity},
		{"内部错误", errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeSettingsService{putErr: tc.err}
			mux, _ := settingsTestMux(t, service, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site", body, manager, nil))
			if rr.Code != tc.want {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestSettingsPutIdempotencyReplays(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	service := &fakeSettingsService{putResult: settings.UpdateResult{
		Section:   "site",
		UpdatedAt: &now,
		Values:    map[string]any{"site.title": "Idem"},
	}}
	mux, _ := settingsTestMux(t, service, nil)
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	body := `{"section":"site","values":{"site.title":"Idem"},"expect_updated_at":null}`
	headers := map[string]string{"Idempotency-Key": "abc"}

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, settingsRequest(http.MethodPut, "/api/settings?section=site", body, manager, headers))
	if first.Code != http.StatusOK {
		t.Fatalf("首次 = %d", first.Code)
	}
	second := httptest.NewRecorder()
	mux.ServeHTTP(second, settingsRequest(http.MethodPut, "/api/settings?section=site", body, manager, headers))
	if second.Code != http.StatusOK {
		t.Fatalf("重放 = %d（body=%s）", second.Code, second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("重放响应缺少 Idempotent-Replayed: true")
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("重放 body 不一致: %q vs %q", first.Body.String(), second.Body.String())
	}
	if service.putCalls != 1 {
		t.Fatalf("同一 Idempotency-Key 应只执行一次，实际 %d", service.putCalls)
	}
}

func TestSettingsPutAuditsMaskedBeforeAfter(t *testing.T) {
	exec := &fakeAuditExec{}
	logger := audit.New(exec)
	service := &fakeSettingsService{putResult: settings.UpdateResult{
		Section: "auth.turnstile",
		Values:  map[string]any{"auth.turnstile.secret_key": settings.MaskedValue},
		Before:  map[string]any{"auth.turnstile.secret_key": settings.MaskedValue},
		After:   map[string]any{"auth.turnstile.secret_key": settings.MaskedValue},
		Changed: []string{"auth.turnstile.secret_key"},
	}}
	mux, _ := settingsTestMux(t, service, logger)
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	body := `{"section":"auth.turnstile","values":{"auth.turnstile.secret_key":"plain-secret"}}`

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=auth.turnstile", body, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if calls := exec.snapshot(); len(calls) != 1 {
		t.Fatalf("审计写入次数 = %d，期望 1", len(calls))
	}
	args := exec.snapshot()[0]
	if args[2] != "operation" {
		t.Fatalf("category = %v，期望 operation", args[2])
	}
	before, _ := args[10].(string)
	after, _ := args[11].(string)
	for name, payload := range map[string]string{"before": before, "after": after} {
		if !strings.Contains(payload, settings.MaskedValue) {
			t.Fatalf("%s 未包含掩码: %s", name, payload)
		}
		if strings.Contains(payload, "plain-secret") {
			t.Fatalf("%s 泄漏了密钥明文: %s", name, payload)
		}
	}
}

func TestSettingsPutBodySectionMismatch(t *testing.T) {
	service := &fakeSettingsService{putResult: settings.UpdateResult{Section: "site", Values: map[string]any{}}}
	mux, _ := settingsTestMux(t, service, nil)
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=site",
		`{"section":"nav","values":{}}`, manager, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("body/query section 不一致 = %d，期望 400", rr.Code)
	}
	if service.putCalls != 0 {
		t.Fatal("section 不一致不得调用服务层")
	}
}

func TestSettingsPutPassesDangerousConfirmation(t *testing.T) {
	service := &fakeSettingsService{putResult: settings.UpdateResult{Section: "pay", Values: map[string]any{}}}
	mux, _ := settingsTestMux(t, service, nil)
	manager := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=pay",
		`{"section":"pay","values":{"pay.online_enabled":false}}`, manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if service.lastInput.DangerousConfirmed {
		t.Fatal("没有 dangerous_confirmed 却标记为已确认")
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/settings?section=pay",
		`{"section":"pay","values":{"pay.online_enabled":false},"dangerous_confirmed":true}`,
		manager, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if !service.lastInput.DangerousConfirmed {
		t.Fatal("dangerous_confirmed 未被识别")
	}
}
