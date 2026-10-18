package rbac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/clientip"
)

// wantCatalog 是 brief 与 modules-07 F-07-08 逐字冻结的 20 键目录。
// 测试侧独立抄写而不是引用实现常量：若实现里键名拼错，集合比较会立刻暴露。
func wantCatalog() []string {
	return []string{
		"users:read",
		"users:create",
		"users:update",
		"users:disable",
		"users:delete",
		"users:adjust-quota",
		"users:manage-roles",
		"users:reset-security",
		"groups:manage",
		"channels:read",
		"channels:manage",
		"channels:manage_sensitive",
		"pricing:read",
		"pricing:manage",
		"redemption:manage",
		"orders:read",
		"orders:manage",
		"logs:read",
		"log.prompt.view",
		"settings:manage",
	}
}

func admin(userID int64, perms ...string) auth.Principal {
	return auth.Principal{UserID: userID, Role: RoleAdmin, Permissions: perms}
}

func user(userID int64, perms ...string) auth.Principal {
	return auth.Principal{UserID: userID, Role: RoleUser, Permissions: perms}
}

func targetAction(required string) AuthzAction {
	return AuthzAction{ResourceKind: "settings", ActionName: required}
}

// serve 把 Principal 注入请求上下文后执行 Handler，并返回是否放行到下游。
func serve(t *testing.T, m *AuthzMiddleware, p *auth.Principal, required string, action AuthzAction, req *http.Request) (*httptest.ResponseRecorder, bool) {
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
	m.Handler(required, action)(next).ServeHTTP(rr, req)
	return rr, called
}

func decodeProblem(t *testing.T, rr *httptest.ResponseRecorder) dto.Problem {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	var p dto.Problem
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应不是合法 RFC 7807 JSON: %v（body=%s）", err, rr.Body.String())
	}
	return p
}

// (g) 目录必须恰好是 20 键：无多、无少、无重复、无别名。
func TestCatalogIsExactlyTwentyKeys(t *testing.T) {
	want := wantCatalog()
	if len(want) != 20 {
		t.Fatalf("测试基准目录长度 = %d，期望 20（测试自身写错）", len(want))
	}

	got := Catalog()
	if len(got) != 20 {
		t.Fatalf("Catalog() 长度 = %d，期望 20：%v", len(got), got)
	}

	sortedWant := append([]string(nil), want...)
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedWant)
	sort.Strings(sortedGot)
	if !reflect.DeepEqual(sortedWant, sortedGot) {
		t.Fatalf("权限目录不一致\n got = %v\nwant = %v", sortedGot, sortedWant)
	}

	seen := make(map[string]bool, len(got))
	for _, key := range got {
		if seen[key] {
			t.Fatalf("目录含重复键 %q", key)
		}
		seen[key] = true
	}

	// 导出常量是其他模块（OpenAPI enum、前端勾选）的唯一引用点，必须与目录逐字一致。
	constKeys := []string{
		PermUsersRead, PermUsersCreate, PermUsersUpdate, PermUsersDisable,
		PermUsersDelete, PermUsersAdjustQuota, PermUsersManageRoles,
		PermUsersResetSecurity, PermGroupsManage,
		PermChannelsRead, PermChannelsManage, PermChannelsManageSensitive,
		PermPricingRead, PermPricingManage, PermRedemptionManage,
		PermOrdersRead, PermOrdersManage, PermLogsRead,
		PermLogPromptView, PermSettingsManage,
	}
	sort.Strings(constKeys)
	if !reflect.DeepEqual(sortedWant, constKeys) {
		t.Fatalf("导出常量与冻结目录不一致\nconsts = %v\nwant   = %v", constKeys, sortedWant)
	}

	// 返回副本：调用方改切片不得污染全局目录。
	mutated := Catalog()
	mutated[0] = "bogus:key"
	if !reflect.DeepEqual(sortedWant, sortedStrings(Catalog())) {
		t.Fatal("Catalog() 返回了内部切片，调用方可篡改全局目录")
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// (a) role=user 即使 Permissions 含 "*" 或所需键，也绝不放行管理动作。
func TestRoleUserNeverPassesAdminActions(t *testing.T) {
	m := NewAuthzMiddleware()
	for _, key := range wantCatalog() {
		for _, p := range []auth.Principal{
			user(7),
			user(7, "*"),
			user(7, key),
		} {
			d := m.Authorize(p, AuthzCtx{}, targetAction(key))
			if d.Effect != "deny" {
				t.Fatalf("user 持有 %v 调 %q：Effect = %q，期望 deny", p.Permissions, key, d.Effect)
			}
			if d.Reason != ReasonPermissionDenied {
				t.Fatalf("user 调 %q：Reason = %q，期望 %q", key, d.Reason, ReasonPermissionDenied)
			}
		}
	}

	rr, called := serve(t, m, ptr(user(7, "*")), PermSettingsManage, targetAction(PermSettingsManage),
		httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if called {
		t.Fatal("user 调管理接口被放行到下游")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
	}
	if got := decodeProblem(t, rr).Type; got != problemTypeForbidden {
		t.Fatalf("problem type = %q，期望 %q", got, problemTypeForbidden)
	}
}

func ptr(p auth.Principal) *auth.Principal { return &p }

// (b) admin 缺所需键 → 403；空权限切片不是 FULL（NULL 才是）。
func TestAdminWithoutRequiredKeyIsDenied(t *testing.T) {
	m := NewAuthzMiddleware()
	cases := []struct {
		name string
		p    auth.Principal
	}{
		{"无任何权限", admin(1, "users:read", "logs:read")},
		{"空数组（非 NULL）", auth.Principal{UserID: 1, Role: RoleAdmin, Permissions: []string{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := m.Authorize(tc.p, AuthzCtx{}, targetAction(PermSettingsManage))
			if d.Effect != "deny" || d.Reason != ReasonPermissionDenied {
				t.Fatalf("decision = %+v，期望 deny/%s", d, ReasonPermissionDenied)
			}
			rr, called := serve(t, m, &tc.p, PermSettingsManage, targetAction(PermSettingsManage),
				httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
			if called || rr.Code != http.StatusForbidden {
				t.Fatalf("called=%v status=%d，期望 false/403", called, rr.Code)
			}
		})
	}
}

// (c) NULL（nil）与 ["*"] 都是 FULL，20 键全通；显式全量列表同样全通。
func TestFullAdminPassesEveryCatalogKey(t *testing.T) {
	m := NewAuthzMiddleware()
	fullSets := [][]string{nil, {"*"}, wantCatalog()}
	for _, perms := range fullSets {
		p := auth.Principal{UserID: 1, Role: RoleAdmin, Permissions: perms}
		for _, key := range wantCatalog() {
			if d := m.Authorize(p, AuthzCtx{}, targetAction(key)); d.Effect != "allow" {
				t.Fatalf("FULL admin（%v）调 %q 被拒：%+v", perms, key, d)
			}
			rr, called := serve(t, m, &p, key, targetAction(key),
				httptest.NewRequest(http.MethodGet, "/api/admin/x", nil))
			if !called || rr.Code != http.StatusNoContent {
				t.Fatalf("FULL admin（%v）经 Handler 调 %q：called=%v status=%d", perms, key, called, rr.Code)
			}
		}
	}
}

// 未知权限键必须失败关闭：拼写错误/未登记的新键不能因为持有 "*" 就默认放行。
func TestUnknownPermissionKeyFailsClosed(t *testing.T) {
	m := NewAuthzMiddleware()
	for _, p := range []auth.Principal{admin(1), admin(1, "*"), user(2, "*")} {
		if d := m.Authorize(p, AuthzCtx{}, targetAction("settings:typo")); d.Effect != "deny" {
			t.Fatalf("未知权限键被放行：%+v", d)
		}
	}
}

// (d) 授予权限必须满足：有 users:manage-roles，且授予集合 ⊆ 自身权限；
// FULL 可授予一切；未知键与 "*" 只允许 FULL 授予。
func TestGrantCannotExceedOwnPermissions(t *testing.T) {
	m := NewAuthzMiddleware()
	cases := []struct {
		name      string
		p         auth.Principal
		requested []string
		want      string
	}{
		{"授权者持有所授键", admin(1, PermUsersManageRoles, PermUsersRead), []string{PermUsersRead}, "allow"},
		{"授予超出自身", admin(1, PermUsersManageRoles, PermUsersRead), []string{PermUsersRead, PermSettingsManage}, "deny"},
		{"缺 manage-roles", admin(1, PermUsersRead, PermSettingsManage), []string{PermUsersRead}, "deny"},
		{"FULL 可授予任意目录键", admin(1), wantCatalog(), "allow"},
		{"FULL 可授予通配", admin(1), []string{"*"}, "allow"},
		{"非 FULL 不可授予通配", admin(1, PermUsersManageRoles), []string{"*"}, "deny"},
		{"未知键一律拒绝", admin(1, PermUsersManageRoles), []string{"users:typo"}, "deny"},
		{"user 不可授权", user(1, "*"), []string{PermUsersRead}, "deny"},
		{"空集合无需扩权", admin(1, PermUsersManageRoles), nil, "allow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := m.AuthorizeGrant(tc.p, tc.requested)
			if d.Effect != tc.want {
				t.Fatalf("Effect = %q，期望 %q（decision=%+v）", d.Effect, tc.want, d)
			}
			if tc.want == "deny" && d.Reason != ReasonPermissionDenied {
				t.Fatalf("deny reason = %q，期望 %q", d.Reason, ReasonPermissionDenied)
			}
		})
	}
}

// (e) ID 寻址他人资源：无管理键时返回 404（与"资源不存在"同形，避免枚举探测）。
func TestOtherOwnerIsHiddenAsNotFound(t *testing.T) {
	m := NewAuthzMiddleware()
	orderRead := AuthzAction{ResourceKind: "order", ActionName: "", TargetOwnerUserID: 42}

	// user 在 self-service 动作（required==""）上访问他人资源 → 404。
	d := m.Authorize(user(7), AuthzCtx{}, orderRead)
	if d.Effect != "deny" || d.Reason != ReasonNotFound {
		t.Fatalf("user 访问他人资源：%+v，期望 deny/not_found", d)
	}

	rr, called := serve(t, m, ptr(user(7)), "", orderRead,
		httptest.NewRequest(http.MethodGet, "/api/orders/42", nil))
	if called {
		t.Fatal("他人资源请求被放行到下游")
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("HTTP 状态 = %d，期望 404", rr.Code)
	}
	prob := decodeProblem(t, rr)
	if prob.Type != problemTypeNotFound {
		t.Fatalf("problem type = %q，期望 %q", prob.Type, problemTypeNotFound)
	}
	if prob.Detail != nil && strings.Contains(*prob.Detail, "42") {
		t.Fatalf("404 响应泄露了目标资源 ID：%q", *prob.Detail)
	}

	// 本人资源放行。
	if d := m.Authorize(user(7), AuthzCtx{}, AuthzAction{ResourceKind: "order", TargetOwnerUserID: 7}); d.Effect != "allow" {
		t.Fatalf("本人资源被拒：%+v", d)
	}
	// 列表类（absent=0）放行，由查询层按 session.user_id 过滤。
	if d := m.Authorize(user(7), AuthzCtx{}, AuthzAction{ResourceKind: "order"}); d.Effect != "allow" {
		t.Fatalf("列表类动作被拒：%+v", d)
	}
	// 持有该资源管理键的 admin 可跨用户访问。
	if d := m.Authorize(admin(1, PermOrdersRead), AuthzCtx{}, AuthzAction{ResourceKind: "order", ActionName: PermOrdersRead, TargetOwnerUserID: 42}); d.Effect != "allow" {
		t.Fatalf("admin 持管理键访问他人资源被拒：%+v", d)
	}
	// admin 走 self-service 动作（无管理键）仍只能访问本人。
	if d := m.Authorize(admin(1), AuthzCtx{}, orderRead); d.Effect != "deny" || d.Reason != ReasonNotFound {
		t.Fatalf("FULL admin 走 self-service 动作访问他人资源：%+v，期望 404", d)
	}
	// 功能级先于数据级：user 调管理动作时是 403，不是 404（不因数据层改写状态码）。
	d = m.Authorize(user(7), AuthzCtx{}, AuthzAction{ResourceKind: "user", ActionName: PermUsersRead, TargetOwnerUserID: 42})
	if d.Effect != "deny" || d.Reason != ReasonPermissionDenied {
		t.Fatalf("user 调管理动作：%+v，期望 403 permission_denied", d)
	}
}

// (f) 写侧 BOPLA：白名单之外的字段（含嵌套）→ 403 field_not_writable，绝不静默丢弃。
func TestWriteFieldBOPLA(t *testing.T) {
	const bodyAction = "users:update"
	m := NewAuthzMiddleware()
	policyAction := AuthzAction{ResourceKind: "user", ActionName: bodyAction}
	m.SetWritePolicy(policyAction, &FieldPolicy{Fields: map[string]*FieldPolicy{
		"display_name": nil,
		"profile": {Fields: map[string]*FieldPolicy{
			"bio": nil,
		}},
	}})
	p := admin(1)

	newWrite := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/users/9", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	t.Run("白名单内字段放行且请求体保留给下游", func(t *testing.T) {
		body := `{"display_name":"Alice","profile":{"bio":"hi"}}`
		var downstreamBody string
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			raw, _ := io.ReadAll(r.Body)
			downstreamBody = string(raw)
			w.WriteHeader(http.StatusNoContent)
		})
		req := newWrite(body).WithContext(auth.WithPrincipal(context.Background(), &p))
		rr := httptest.NewRecorder()
		m.Handler(PermUsersUpdate, policyAction)(next).ServeHTTP(rr, req)
		if !called || rr.Code != http.StatusNoContent {
			t.Fatalf("called=%v status=%d，期望放行", called, rr.Code)
		}
		if downstreamBody != body {
			t.Fatalf("下游读到 body = %q，期望 %q（中间件必须恢复请求体）", downstreamBody, body)
		}
	})

	t.Run("未知顶层字段", func(t *testing.T) {
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, newWrite(`{"display_name":"Alice","role":"admin"}`))
		assertFieldDenied(t, rr, called, "role")
	})

	t.Run("嵌套越权字段", func(t *testing.T) {
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, newWrite(`{"profile":{"bio":"hi","email":"a@b.c"}}`))
		assertFieldDenied(t, rr, called, "profile.email")
	})

	t.Run("只读/其他动作字段", func(t *testing.T) {
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, newWrite(`{"password":"x"}`))
		assertFieldDenied(t, rr, called, "password")
	})

	t.Run("未登记动作的写请求失败关闭", func(t *testing.T) {
		other := AuthzAction{ResourceKind: "channel", ActionName: PermChannelsManage}
		rr, called := serve(t, m, &p, PermChannelsManage, other, newWrite(`{"name":"x"}`))
		assertFieldDenied(t, rr, called, "")
	})

	t.Run("畸形 JSON 失败关闭", func(t *testing.T) {
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, newWrite(`{"display_name":`))
		assertFieldDenied(t, rr, called, "")
	})

	t.Run("子树字段收到标量失败关闭", func(t *testing.T) {
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, newWrite(`{"profile":"not-an-object"}`))
		assertFieldDenied(t, rr, called, "profile")
	})

	t.Run("子树对象数组中的非对象元素失败关闭", func(t *testing.T) {
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, newWrite(`{"profile":[{"bio":"hi"},"scalar"]}`))
		assertFieldDenied(t, rr, called, "profile[1]")
	})

	t.Run("非 JSON 写体一律 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/users/9", strings.NewReader("display_name=Alice"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, req)
		assertFieldDenied(t, rr, called, "")
	})

	t.Run("读请求不触发字段层", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/users/9", nil)
		rr, called := serve(t, m, &p, PermUsersUpdate, policyAction, req)
		if !called || rr.Code != http.StatusNoContent {
			t.Fatalf("GET 被字段层拦截：called=%v status=%d", called, rr.Code)
		}
	})

	t.Run("功能级拒绝优先于字段级", func(t *testing.T) {
		poorAdmin := admin(1, PermUsersRead)
		rr, called := serve(t, m, &poorAdmin, PermUsersUpdate, policyAction, newWrite(`{"role":"admin"}`))
		if called || rr.Code != http.StatusForbidden {
			t.Fatalf("called=%v status=%d", called, rr.Code)
		}
		if got := decodeProblem(t, rr).Type; got != problemTypeForbidden {
			t.Fatalf("problem type = %q，期望功能级 %q（字段级不得抢先）", got, problemTypeForbidden)
		}
	})
}

func assertFieldDenied(t *testing.T, rr *httptest.ResponseRecorder, called bool, wantField string) {
	t.Helper()
	if called {
		t.Fatal("越权字段请求被放行到下游（静默丢弃或忽略）")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
	}
	prob := decodeProblem(t, rr)
	if prob.Type != problemTypeFieldNotWritable {
		t.Fatalf("problem type = %q，期望 %q", prob.Type, problemTypeFieldNotWritable)
	}
	if wantField != "" {
		fields, ok := prob.Get("fields")
		if !ok {
			t.Fatalf("响应缺少 fields 扩展成员：%s", rr.Body.String())
		}
		raw, _ := json.Marshal(fields)
		if !strings.Contains(string(raw), wantField) {
			t.Fatalf("fields = %s，期望包含 %q", raw, wantField)
		}
	}
}

// Authorize 与 CheckFields 的组合必须与 Handler 完全一致，供非 HTTP 调用方复用。
func TestCheckFieldsMatchesHandler(t *testing.T) {
	m := NewAuthzMiddleware()
	action := AuthzAction{ResourceKind: "user", ActionName: PermUsersUpdate}
	m.SetWritePolicy(action, &FieldPolicy{Fields: map[string]*FieldPolicy{"display_name": nil}})

	if d := m.CheckFields(action, map[string]any{"display_name": "Alice"}); d.Effect != "allow" {
		t.Fatalf("白名单字段被拒：%+v", d)
	}
	if d := m.CheckFields(action, map[string]any{"role": "admin"}); d.Effect != "deny" || d.Reason != ReasonFieldNotWritable {
		t.Fatalf("越权字段 decision = %+v，期望 deny/%s", d, ReasonFieldNotWritable)
	}
	if d := m.CheckFields(action, nil); d.Effect != "allow" {
		t.Fatalf("空 body 被拒：%+v", d)
	}
	unregistered := AuthzAction{ResourceKind: "user", ActionName: PermUsersDelete}
	if d := m.CheckFields(unregistered, map[string]any{}); d.Effect != "deny" || d.Reason != ReasonFieldNotWritable {
		t.Fatalf("未登记动作 decision = %+v，期望失败关闭", d)
	}
}

// Handler 的认证前提：上下文无 Principal 时必须拒绝，不能把零值身份当匿名放行。
func TestHandlerRequiresPrincipal(t *testing.T) {
	m := NewAuthzMiddleware()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("无 Principal 仍进入下游") })
	rr := httptest.NewRecorder()
	m.Handler(PermSettingsManage, targetAction(PermSettingsManage))(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
	}
	if got := decodeProblem(t, rr).Type; got != problemTypeForbidden {
		t.Fatalf("problem type = %q，期望 %q", got, problemTypeForbidden)
	}
}

// 上下文采集只信服务端观测值：客户端可伪造的 X-Forwarded-For 不参与；UA 只落哈希。
func TestAuthzContextUsesServerObservedValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.RemoteAddr = "203.0.113.9:41234"
	req.Header.Set("User-Agent", "rbac-test-agent")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "sess-abc"})

	got := ContextFromRequest(req)
	if got.ClientIP != "203.0.113.9" {
		t.Fatalf("ClientIP = %q，期望取自 RemoteAddr（忽略 X-Forwarded-For）", got.ClientIP)
	}
	sum := sha256.Sum256([]byte("rbac-test-agent"))
	if got.UAHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("UAHash = %q，期望 UA 的 SHA-256", got.UAHash)
	}
	if got.SessionID != "sess-abc" {
		t.Fatalf("SessionID = %q，期望会话 Cookie 值", got.SessionID)
	}
}

// TestAuthzContextHonorsTrustedProxies：边缘部署（TRUSTED_PROXIES 已配置）时
// 风控上下文必须与限流/审计使用同一解析器，拿到真实客户端地址而不是 edge IP。
func TestAuthzContextHonorsTrustedProxies(t *testing.T) {
	t.Cleanup(func() { _ = clientip.Configure("") })
	if err := clientip.Configure("10.0.0.0/8"); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.RemoteAddr = "10.0.0.2:41234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")

	if got := ContextFromRequest(req).ClientIP; got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q，期望可信代理解析后的客户端 203.0.113.7", got)
	}
}

// CRITICAL 回归（parser differential）：重复 JSON 键。
//
// map[string]any 对重复键只保留最后一次，而 encoding/json 解码到 struct 时
// 会按出现顺序合并重复父对象；攻击者用两段 profile 可让中间件看到「干净」的
// 最后一份、下游却拿到合并后的越权字段。任何层级的重复键都必须 403 且不调用
// 下游（IS-B-10、TC-B-10/21/22）。
func TestDuplicateJSONKeysFailClosed(t *testing.T) {
	m := NewAuthzMiddleware()
	action := AuthzAction{ResourceKind: "user", ActionName: "me:update"}
	m.SetWritePolicy(action, &FieldPolicy{Fields: map[string]*FieldPolicy{
		"display_name": nil,
		"profile": {Fields: map[string]*FieldPolicy{
			"bio": nil,
		}},
	}})

	cases := []struct {
		name    string
		payload string
	}{
		{
			// 中间件只看最后一份 profile（bio 合法），下游 struct 却会合并出 Role。
			name:    "重复父对象夹带越权字段",
			payload: `{"display_name":"Alice","profile":{"role":"admin"},"profile":{"bio":"hi"}}`,
		},
		{
			name:    "顶层重复键",
			payload: `{"display_name":"Alice","display_name":"Bob"}`,
		},
		{
			name:    "嵌套重复键",
			payload: `{"profile":{"bio":"a","bio":"b"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/api/me", strings.NewReader(tc.payload))
			req.Header.Set("Content-Type", "application/json")
			rr, called := serve(t, m, ptr(admin(1)), PermUsersUpdate, action, req)
			if called {
				t.Fatal("重复 JSON 键请求被放行到下游（parser differential）")
			}
			if rr.Code != http.StatusForbidden {
				t.Fatalf("HTTP 状态 = %d，期望 403", rr.Code)
			}
			if got := decodeProblem(t, rr).Type; got != problemTypeFieldNotWritable {
				t.Fatalf("problem type = %q，期望 %q", got, problemTypeFieldNotWritable)
			}
		})
	}
}

// 协调者补充的契约边界：Permissions 必须由装载层填充；受限 admin
// （仅 users:read）调 users:create 必须 403，而持有键的动作仍应放行。
func TestRestrictedAdminCannotUseUnheldKey(t *testing.T) {
	m := NewAuthzMiddleware()
	p := admin(1, PermUsersRead)

	if d := m.Authorize(p, AuthzCtx{}, targetAction(PermUsersCreate)); d.Effect != "deny" || d.Reason != ReasonPermissionDenied {
		t.Fatalf("受限 admin 调 users:create：%+v，期望 deny/%s", d, ReasonPermissionDenied)
	}
	if d := m.Authorize(p, AuthzCtx{}, targetAction(PermUsersRead)); d.Effect != "allow" {
		t.Fatalf("受限 admin 调其持有的 users:read 被拒：%+v", d)
	}

	rr, called := serve(t, m, &p, PermUsersCreate, targetAction(PermUsersCreate),
		httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{}`)))
	if called || rr.Code != http.StatusForbidden {
		t.Fatalf("Handler called=%v status=%d，期望 false/403", called, rr.Code)
	}
}

// fakePermissionLoader 模拟 Task 6 在会话鉴权后接线的 users.admin_permissions
// 读取器；calls 用于断言 user 角色不触发无意义的 DB 查询。
type fakePermissionLoader struct {
	perms []string
	err   error
	calls []int64
}

func (f *fakePermissionLoader) Load(_ context.Context, userID int64) ([]string, error) {
	f.calls = append(f.calls, userID)
	if f.err != nil {
		return nil, f.err
	}
	return f.perms, nil
}

func TestPermissionLoaderIsAuthoritativeAndFailsClosed(t *testing.T) {
	t.Run("装载值覆盖 Principal 里的未加载 nil", func(t *testing.T) {
		m := NewAuthzMiddleware()
		loader := &fakePermissionLoader{perms: []string{PermUsersRead}}
		m.SetPermissionLoader(loader)

		// 会话路径的 Principal.Permissions 为 nil：若被当成 FULL，
		// users:create 会被误放行。装载器必须覆盖它。
		resolved, err := m.ResolvePermissions(context.Background(), auth.Principal{UserID: 9, Role: RoleAdmin})
		if err != nil {
			t.Fatalf("ResolvePermissions: %v", err)
		}
		if !reflect.DeepEqual(resolved.Permissions, []string{PermUsersRead}) {
			t.Fatalf("resolved.Permissions = %v，期望装载器返回值", resolved.Permissions)
		}
		if d := m.Authorize(resolved, AuthzCtx{}, targetAction(PermUsersCreate)); d.Effect != "deny" {
			t.Fatalf("受限 admin 调 users:create 被放行：%+v", d)
		}
		if d := m.Authorize(resolved, AuthzCtx{}, targetAction(PermUsersRead)); d.Effect != "allow" {
			t.Fatalf("受限 admin 调 users:read 被拒：%+v", d)
		}
		if len(loader.calls) != 1 || loader.calls[0] != 9 {
			t.Fatalf("loader.calls = %v，期望 [9]", loader.calls)
		}
	})

	t.Run("装载器返回 NULL 仍表示 FULL", func(t *testing.T) {
		m := NewAuthzMiddleware()
		m.SetPermissionLoader(&fakePermissionLoader{perms: nil})
		resolved, err := m.ResolvePermissions(context.Background(), admin(9))
		if err != nil {
			t.Fatalf("ResolvePermissions: %v", err)
		}
		if d := m.Authorize(resolved, AuthzCtx{}, targetAction(PermUsersCreate)); d.Effect != "allow" {
			t.Fatalf("DB NULL 装载后应视为 FULL：%+v", d)
		}
	})

	t.Run("装载失败必须拒绝而非按 FULL 放行", func(t *testing.T) {
		m := NewAuthzMiddleware()
		m.SetPermissionLoader(&fakePermissionLoader{err: errors.New("db down")})
		p := admin(9) // Principal 里甚至写着 FULL（nil）
		if _, err := m.ResolvePermissions(context.Background(), p); err == nil {
			t.Fatal("装载失败未返回 error")
		}
		rr, called := serve(t, m, &p, PermUsersCreate, targetAction(PermUsersCreate),
			httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
		if called || rr.Code != http.StatusForbidden {
			t.Fatalf("装载失败 called=%v status=%d，期望 false/403", called, rr.Code)
		}
	})

	t.Run("user 角色不触发装载", func(t *testing.T) {
		m := NewAuthzMiddleware()
		loader := &fakePermissionLoader{perms: []string{"*"}}
		m.SetPermissionLoader(loader)
		if _, err := m.ResolvePermissions(context.Background(), user(3)); err != nil {
			t.Fatalf("ResolvePermissions: %v", err)
		}
		if len(loader.calls) != 0 {
			t.Fatalf("user 角色触发了权限查询：%v", loader.calls)
		}
	})
}

// Handler 成功路径必须同时做到：用 loader 的值判定，并把解析后的 Principal
// 写回请求上下文供下游 handler 使用（否则下游读到的仍是 nil=FULL）。
func TestHandlerLoadsPermissionsAndPropagatesToDownstream(t *testing.T) {
	m := NewAuthzMiddleware()
	loader := &fakePermissionLoader{perms: []string{PermUsersRead}}
	m.SetPermissionLoader(loader)

	// 会话 Principal 的 Permissions 为 nil（未加载）。若 Handler 把 nil 当 FULL，
	// users:create 会被放行；若只用于判定而不写回 context，下游仍会看到 nil。
	p := auth.Principal{UserID: 9, Role: RoleAdmin}

	var seen []string
	var sawPrincipal bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := auth.PrincipalFrom(r.Context())
		sawPrincipal = ok
		if ok {
			seen = got.Permissions
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// users:create → 403，不进入下游。
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil).
		WithContext(auth.WithPrincipal(context.Background(), &p))
	rr := httptest.NewRecorder()
	m.Handler(PermUsersCreate, targetAction(PermUsersCreate))(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("users:create status = %d，期望 403", rr.Code)
	}

	// users:read → 204，下游必须看到 loader 返回的权限集。
	req = httptest.NewRequest(http.MethodGet, "/api/admin/users", nil).
		WithContext(auth.WithPrincipal(context.Background(), &p))
	rr = httptest.NewRecorder()
	m.Handler(PermUsersRead, targetAction(PermUsersRead))(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("users:read status = %d，期望 204", rr.Code)
	}
	if !sawPrincipal || !reflect.DeepEqual(seen, []string{PermUsersRead}) {
		t.Fatalf("下游 Principal.Permissions = %v（present=%v），期望 loader 返回值", seen, sawPrincipal)
	}
	if want := []int64{9, 9}; !reflect.DeepEqual(loader.calls, want) {
		t.Fatalf("loader.calls = %v，期望 %v", loader.calls, want)
	}

	// user 角色走 Handler：loader 零调用（权限数组对 user 无意义）。
	u := user(3, "*")
	before := len(loader.calls)
	req = httptest.NewRequest(http.MethodGet, "/api/admin/users", nil).
		WithContext(auth.WithPrincipal(context.Background(), &u))
	rr = httptest.NewRecorder()
	m.Handler(PermUsersRead, targetAction(PermUsersRead))(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("user 调管理动作 status = %d，期望 403", rr.Code)
	}
	if len(loader.calls) != before {
		t.Fatalf("user 角色触发了权限查询：%v", loader.calls)
	}
}

// HandlerWithTarget 必须在路由匹配之后按请求解析数据级归属。用 required=""
// （自服务动作）隔离出数据级判定：本人放行，ID 指向他人一律 404（而非 403，
// 后者会确认资源存在）。
func TestHandlerWithTargetResolvesOwnershipPerRequest(t *testing.T) {
	m := NewAuthzMiddleware()
	action := AuthzAction{ResourceKind: "user", ActionName: "read"}
	target := func(r *http.Request) int64 {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return 0
		}
		return id
	}

	p := user(7)
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := m.HandlerWithTarget("", action, target)(next)

	req := httptest.NewRequest(http.MethodGet, "/api/users/7", nil)
	req.SetPathValue("id", "7")
	req = req.WithContext(auth.WithPrincipal(context.Background(), &p))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("本人访问 status = %d（next=%v），期望 204", rr.Code, nextCalled)
	}

	nextCalled = false
	req = httptest.NewRequest(http.MethodGet, "/api/users/8", nil)
	req.SetPathValue("id", "8")
	req = req.WithContext(auth.WithPrincipal(context.Background(), &p))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("他人访问 status = %d，期望 404", rr.Code)
	}
	if nextCalled {
		t.Fatal("数据级拒绝后仍进入下游 handler")
	}

	// 不可解析的 ID 视为无归属（target=0），交业务 handler 统一 404；
	// 鉴权层不得因解析失败而放行或误判为他人的资源。
	nextCalled = false
	req = httptest.NewRequest(http.MethodGet, "/api/users/not-a-number", nil)
	req.SetPathValue("id", "not-a-number")
	req = req.WithContext(auth.WithPrincipal(context.Background(), &p))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("非法 ID status = %d（next=%v），期望交下游处理", rr.Code, nextCalled)
	}
}

// HandlerWithTarget 的归属解析必须是每请求独立的：闭包若直接修改捕获的
// action 变量，并发请求会互相覆盖 TargetOwnerUserID（数据竞争 + 跨用户
// 归属污染）。这里让本人/他人请求交错执行，断言状态码始终与各自路径一致。
func TestHandlerWithTargetConcurrentOwnership(t *testing.T) {
	m := NewAuthzMiddleware()
	action := AuthzAction{ResourceKind: "user", ActionName: "read"}
	target := func(r *http.Request) int64 {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return 0
		}
		return id
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := m.HandlerWithTarget("", action, target)(next)
	p := user(7)

	const workers = 64
	var wg sync.WaitGroup
	problems := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := strconv.Itoa(7 + i%2) // 偶数=本人(7)，奇数=他人(8)
			req := httptest.NewRequest(http.MethodGet, "/api/users/"+id, nil)
			req.SetPathValue("id", id)
			req = req.WithContext(auth.WithPrincipal(context.Background(), &p))
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			want := http.StatusNotFound
			if id == "7" {
				want = http.StatusNoContent
			}
			if rr.Code != want {
				problems <- fmt.Sprintf("id=%s status=%d want=%d", id, rr.Code, want)
			}
		}(i)
	}
	wg.Wait()
	close(problems)
	for problem := range problems {
		t.Error(problem)
	}
}
