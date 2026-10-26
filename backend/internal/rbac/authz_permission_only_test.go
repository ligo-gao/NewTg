package rbac

// HandlerPermissionOnly 是 multipart 端点的授权变体：只做功能级 + 数据级
// 判定，不读取请求体。本文件冻结它的三条行为：
//   1. 未登记字段策略的写动作 + multipart body 也能通过（不读 body）；
//   2. 功能级权限仍然必须持有，缺 users:create → 403；
//   3. 未认证 → 403 且审计 reason=unauthenticated。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
)

func servePermissionOnly(
	t *testing.T, m *AuthzMiddleware, p *auth.Principal,
	required string, action AuthzAction, req *http.Request,
) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	rr := httptest.NewRecorder()
	m.HandlerPermissionOnly(required, action)(next).ServeHTTP(rr, req)
	return rr, called
}

func TestHandlerPermissionOnlySkipsFieldPolicy(t *testing.T) {
	m := NewAuthzMiddleware()
	// 刻意不登记写策略：普通 Handler 会以 policy_missing 失败关闭。
	action := AuthzAction{ResourceKind: "user", ActionName: "bulk_import"}
	req := httptest.NewRequest(http.MethodPost, "/api/users/bulk/import",
		strings.NewReader("--boundary\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nusername\r\n--boundary--\r\n"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")

	rr, called := servePermissionOnly(t, m, ptr(admin(1, PermUsersCreate)), PermUsersCreate, action, req)
	if !called {
		t.Fatalf("HandlerPermissionOnly 不应读取 multipart body，实际被字段层拦截：%s", rr.Body.String())
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("HTTP 状态 = %d，期望 204", rr.Code)
	}
}

func TestHandlerPermissionOnlyStillEnforcesPermission(t *testing.T) {
	m := NewAuthzMiddleware()
	action := AuthzAction{ResourceKind: "user", ActionName: "bulk_import"}
	req := httptest.NewRequest(http.MethodPost, "/api/users/bulk/import", nil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")

	rr, called := servePermissionOnly(t, m, ptr(admin(1, PermUsersRead)), PermUsersCreate, action, req)
	if called {
		t.Fatal("缺少 users:create 仍进入下游")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
	}
	if got := decodeProblem(t, rr).Type; got != problemTypeForbidden {
		t.Fatalf("problem type = %q，期望 %q", got, problemTypeForbidden)
	}
}

func TestHandlerPermissionOnlyRejectsUnauthenticated(t *testing.T) {
	m := NewAuthzMiddleware()
	var events []DenyEvent
	m.SetDenyAuditor(func(_ *http.Request, e DenyEvent) {
		events = append(events, e)
	})
	action := AuthzAction{ResourceKind: "user", ActionName: "bulk_import"}
	req := httptest.NewRequest(http.MethodPost, "/api/users/bulk/import", nil)

	rr, called := servePermissionOnly(t, m, nil, PermUsersCreate, action, req)
	if called {
		t.Fatal("未认证请求进入下游")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
	}
	if len(events) != 1 || events[0].Reason != DenyReasonUnauthenticated {
		t.Fatalf("拒绝审计 = %+v，期望一条 reason=%s", events, DenyReasonUnauthenticated)
	}
}
