package rbac

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// wantDeny 是各拒绝分支期望的审计事件形状。
type wantDeny struct {
	reason   string
	userID   int64
	perm     string
	fields   []string
	wantCode int
}

// TestWritePolicyCheckPrecedesBodyPresence 覆盖 D1-2：未登记字段策略的动作，
// 即使写请求没有 body（DELETE/Content-Length: 0），也必须 403 且不调用下游；
// 登记过的动作则允许空 body（没有字段可写）通过。
func TestWritePolicyCheckPrecedesBodyPresence(t *testing.T) {
	m := NewAuthzMiddleware()
	unregistered := AuthzAction{ResourceKind: "user", ActionName: "delete"}

	t.Run("DELETE 无 body + 未登记策略 → 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/admin/users/9", nil)
		rr, called := serve(t, m, ptr(admin(1)), PermUsersDelete, unregistered, req)
		if called {
			t.Fatal("未登记字段策略的写请求被放行到下游（失败开放）")
		}
		if rr.Code != http.StatusForbidden {
			t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
		}
		if got := decodeProblem(t, rr).Type; got != problemTypeFieldNotWritable {
			t.Fatalf("problem type = %q，期望 %q", got, problemTypeFieldNotWritable)
		}
	})

	registered := AuthzAction{ResourceKind: "user", ActionName: "delete"}
	m.SetWritePolicy(registered, &FieldPolicy{Fields: map[string]*FieldPolicy{"name": nil}})

	t.Run("DELETE 无 body + 已登记策略 → 放行", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/admin/users/9", nil)
		rr, called := serve(t, m, ptr(admin(1)), PermUsersDelete, registered, req)
		if !called || rr.Code != http.StatusNoContent {
			t.Fatalf("called=%v status=%d，期望放行 204", called, rr.Code)
		}
	})

	t.Run("空 JSON 对象 + 已登记策略 → 放行", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/admin/users/9", strings.NewReader(`{}`))
		rr, called := serve(t, m, ptr(admin(1)), PermUsersDelete, registered, req)
		if !called || rr.Code != http.StatusNoContent {
			t.Fatalf("called=%v status=%d，期望放行 204", called, rr.Code)
		}
	})
}

// TestLiteralNullWriteBodyRejected 覆盖 D1-9：字面量 null 不是 JSON 对象，
// 不能因为 json.Unmarshal(null, &map) 返回 nil map 且无错误就被当成空对象放行。
func TestLiteralNullWriteBodyRejected(t *testing.T) {
	m := NewAuthzMiddleware()
	action := AuthzAction{ResourceKind: "user", ActionName: "update"}
	m.SetWritePolicy(action, &FieldPolicy{Fields: map[string]*FieldPolicy{"name": nil}})

	for _, body := range []string{"null", "  null\n", " null "} {
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/users/9", strings.NewReader(body))
		rr, called := serve(t, m, ptr(admin(1)), PermUsersUpdate, action, req)
		if called {
			t.Fatalf("body=%q 被放行到下游", body)
		}
		if rr.Code != http.StatusForbidden {
			t.Fatalf("body=%q status=%d，期望 403", body, rr.Code)
		}
		if got := decodeProblem(t, rr).Type; got != problemTypeFieldNotWritable {
			t.Fatalf("body=%q problem type = %q，期望 %q", body, got, problemTypeFieldNotWritable)
		}
	}
}

// TestDenyAuditorReportsEveryDenyBranch 覆盖 D1-3：Handler 的每个拒绝分支都必须
// 同步产生一条 DenyEvent（Reason/Permission/路由/方法/字段），供装配层写
// audit_logs 的 security 行（security-09 §2.7 设计 B / security-02 TC-B-17）。
func TestDenyAuditorReportsEveryDenyBranch(t *testing.T) {
	collect := func(t *testing.T, m *AuthzMiddleware) *[]DenyEvent {
		t.Helper()
		events := &[]DenyEvent{}
		m.SetDenyAuditor(func(_ *http.Request, e DenyEvent) {
			*events = append(*events, e)
		})
		return events
	}

	t.Run("Principal 缺失", func(t *testing.T) {
		m := NewAuthzMiddleware()
		events := collect(t, m)
		rr, called := serve(t, m, nil, PermSettingsManage, targetAction(PermSettingsManage),
			httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonUnauthenticated, userID: 0, perm: PermSettingsManage, wantCode: http.StatusForbidden,
		}, http.MethodGet, "/api/admin/settings")
	})

	t.Run("权限装载失败", func(t *testing.T) {
		m := NewAuthzMiddleware()
		m.SetPermissionLoader(&fakePermissionLoader{err: errors.New("db down")})
		events := collect(t, m)
		p := admin(9)
		rr, called := serve(t, m, &p, PermUsersCreate, targetAction(PermUsersCreate),
			httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonPermissionLoadFailed, userID: 9, perm: PermUsersCreate, wantCode: http.StatusForbidden,
		}, http.MethodGet, "/api/admin/users")
	})

	t.Run("功能级拒绝", func(t *testing.T) {
		m := NewAuthzMiddleware()
		events := collect(t, m)
		p := admin(9, PermUsersRead) // 缺 users:create
		rr, called := serve(t, m, &p, PermUsersCreate, targetAction(PermUsersCreate),
			httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonPermission, userID: 9, perm: PermUsersCreate, wantCode: http.StatusForbidden,
		}, http.MethodGet, "/api/admin/users")
	})

	t.Run("数据级拒绝", func(t *testing.T) {
		m := NewAuthzMiddleware()
		events := collect(t, m)
		action := AuthzAction{ResourceKind: "order", TargetOwnerUserID: 42}
		p := user(7)
		rr, called := serve(t, m, &p, "", action,
			httptest.NewRequest(http.MethodGet, "/api/orders/42", nil))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonDataScope, userID: 7, perm: "", wantCode: http.StatusNotFound,
		}, http.MethodGet, "/api/orders/42")
	})

	t.Run("字段策略未登记", func(t *testing.T) {
		m := NewAuthzMiddleware()
		events := collect(t, m)
		action := AuthzAction{ResourceKind: "channel", ActionName: PermChannelsManage}
		p := admin(1)
		rr, called := serve(t, m, &p, PermChannelsManage, action,
			httptest.NewRequest(http.MethodPatch, "/api/admin/channels/1", strings.NewReader(`{"name":"x"}`)))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonPolicyMissing, userID: 1, perm: PermChannelsManage, wantCode: http.StatusForbidden,
		}, http.MethodPatch, "/api/admin/channels/1")
	})

	t.Run("非法 JSON body", func(t *testing.T) {
		m := NewAuthzMiddleware()
		action := AuthzAction{ResourceKind: "user", ActionName: "update"}
		m.SetWritePolicy(action, &FieldPolicy{Fields: map[string]*FieldPolicy{"name": nil}})
		events := collect(t, m)
		p := admin(1)
		rr, called := serve(t, m, &p, PermUsersUpdate, action,
			httptest.NewRequest(http.MethodPatch, "/api/admin/users/9", strings.NewReader(`{"name":`)))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonInvalidBody, userID: 1, perm: PermUsersUpdate, wantCode: http.StatusForbidden,
		}, http.MethodPatch, "/api/admin/users/9")
	})

	t.Run("越权字段（Fields 与响应体一致且已排序）", func(t *testing.T) {
		m := NewAuthzMiddleware()
		action := AuthzAction{ResourceKind: "user", ActionName: "update"}
		m.SetWritePolicy(action, &FieldPolicy{Fields: map[string]*FieldPolicy{"name": nil}})
		events := collect(t, m)
		p := admin(1)
		rr, called := serve(t, m, &p, PermUsersUpdate, action,
			httptest.NewRequest(http.MethodPatch, "/api/admin/users/9",
				strings.NewReader(`{"role":"admin","zeta":1,"alpha":2}`)))
		assertDeny(t, rr, called, *events, wantDeny{
			reason: DenyReasonField, userID: 1, perm: PermUsersUpdate,
			fields: []string{"alpha", "role", "zeta"}, wantCode: http.StatusForbidden,
		}, http.MethodPatch, "/api/admin/users/9")

		// 响应体 problem.fields 必须是同一份排序结果。
		var problem struct {
			Fields []string `json:"fields"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		if strings.Join(problem.Fields, ",") != strings.Join((*events)[0].Fields, ",") {
			t.Fatalf("响应 fields = %v，审计 Fields = %v，必须一致", problem.Fields, (*events)[0].Fields)
		}
	})
}

func assertDeny(t *testing.T, rr *httptest.ResponseRecorder, called bool, events []DenyEvent, w wantDeny, method, route string) {
	t.Helper()
	if called {
		t.Fatal("拒绝分支仍调用下游 handler")
	}
	if rr.Code != w.wantCode {
		t.Fatalf("HTTP 状态 = %d，期望 %d", rr.Code, w.wantCode)
	}
	if len(events) != 1 {
		t.Fatalf("DenyEvent 次数 = %d，期望 1", len(events))
	}
	got := events[0]
	if got.Reason != w.reason {
		t.Errorf("Reason = %q，期望 %q", got.Reason, w.reason)
	}
	if got.UserID != w.userID {
		t.Errorf("UserID = %d，期望 %d", got.UserID, w.userID)
	}
	if got.Permission != w.perm {
		t.Errorf("Permission = %q，期望 %q", got.Permission, w.perm)
	}
	if got.Method != method {
		t.Errorf("Method = %q，期望 %q", got.Method, method)
	}
	if got.Route != route {
		t.Errorf("Route = %q，期望 %q", got.Route, route)
	}
	wantFields := append([]string(nil), w.fields...)
	gotFields := append([]string(nil), got.Fields...)
	sort.Strings(wantFields)
	sort.Strings(gotFields)
	if strings.Join(gotFields, ",") != strings.Join(wantFields, ",") {
		t.Errorf("Fields = %v，期望 %v", got.Fields, w.fields)
	}
}

// TestDenyAuditorPanicDoesNotBreakDenyPath 钉死回调契约：审计实现 panic 时，
// 拒绝路径仍必须稳定返回 403 且不调用下游（由 rbac 侧 recover 兜底）。
func TestDenyAuditorPanicDoesNotBreakDenyPath(t *testing.T) {
	m := NewAuthzMiddleware()
	m.SetDenyAuditor(func(*http.Request, DenyEvent) { panic("audit hook exploded") })

	rr, called := serve(t, m, ptr(admin(9, PermUsersRead)), PermUsersCreate, targetAction(PermUsersCreate),
		httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
	if called {
		t.Fatal("拒绝分支仍调用下游 handler")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d，期望 403（panic 不得改变拒绝响应）", rr.Code)
	}
}

// TestDenyAuditorNilIsNoop：未装配审计时拒绝路径不得有任何行为变化。
func TestDenyAuditorNilIsNoop(t *testing.T) {
	m := NewAuthzMiddleware()
	rr, called := serve(t, m, ptr(user(3)), PermSettingsManage, targetAction(PermSettingsManage),
		httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if called || rr.Code != http.StatusForbidden {
		t.Fatalf("called=%v status=%d，期望 false/403", called, rr.Code)
	}
}

// 编译期钉死跨波契约：D2 的装配代码按这个形状接线。
var _ = func(m *AuthzMiddleware) {
	var fn func(r *http.Request, e DenyEvent)
	m.SetDenyAuditor(fn)
}
