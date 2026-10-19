package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件验证 B12（RBAC 拒绝审计接线）与 B11 的装配形状：受限身份访问管理
// 端点必须 403，且拒绝事件以 authz.denied 落日志库（security 分类）。

type denyAuditResult int64

func (r denyAuditResult) LastInsertId() (int64, error) { return 0, nil }
func (r denyAuditResult) RowsAffected() (int64, error) { return int64(r), nil }

type denyAuditCapture struct {
	mu   sync.Mutex
	rows [][]any
}

func (c *denyAuditCapture) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, append([]any(nil), args...))
	return denyAuditResult(1), nil
}

func (c *denyAuditCapture) snapshot() [][]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]any(nil), c.rows...)
}

type emptyPermissionLoader struct{}

func (emptyPermissionLoader) Load(context.Context, int64) ([]string, error) {
	return []string{}, nil
}

// TestAuthzDenyAuditorWritesSecurityEvent：受限 admin 被功能级权限拒绝时，
// rbac 必须调用装配的 deny auditor，审计行含冻结 detail 键。
func TestAuthzDenyAuditorWritesSecurityEvent(t *testing.T) {
	capture := &denyAuditCapture{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authz := newAuthzMiddleware(audit.New(capture), logger)
	authz.SetPermissionLoader(emptyPermissionLoader{})

	handler := authz.Handler(rbac.PermUsersRead,
		rbac.AuthzAction{ResourceKind: "user", ActionName: "list"})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req = req.WithContext(auth.WithPrincipal(req.Context(),
		&auth.Principal{UserID: 7, Role: rbac.RoleAdmin}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	rows := capture.snapshot()
	if len(rows) != 1 {
		t.Fatalf("审计行 = %d，期望 1（拒绝必须留痕）", len(rows))
	}
	args := rows[0]
	if got, ok := args[0].(int64); !ok || got != 7 {
		t.Fatalf("user_id = %v（%T），期望 7", args[0], args[0])
	}
	if args[2] != "security" {
		t.Fatalf("category = %v，期望 security（authz.denied）", args[2])
	}
	if args[8] != "failed" {
		t.Fatalf("result = %v，期望 failed", args[8])
	}
	if status, ok := args[7].(int16); !ok || status != http.StatusForbidden {
		t.Fatalf("status_code = %v（%T），期望 403", args[7], args[7])
	}

	var raw []byte
	switch value := args[9].(type) {
	case []byte:
		raw = value
	case string:
		raw = []byte(value)
	default:
		t.Fatalf("detail 类型 = %T，期望 string/[]byte", args[9])
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("detail 不是合法 JSON: %v（%q）", err, raw)
	}
	// 键名是 security-09 §2.7 冻结契约；event 必须是 authz_denied。
	if detail["event"] != "authz_denied" {
		t.Fatalf("detail.event = %v，期望 authz_denied", detail["event"])
	}
	if detail["required_permission"] != rbac.PermUsersRead {
		t.Fatalf("detail.required_permission = %v，期望 %s", detail["required_permission"], rbac.PermUsersRead)
	}
	if detail["reason"] != rbac.DenyReasonPermission {
		t.Fatalf("detail.reason = %v，期望 %s", detail["reason"], rbac.DenyReasonPermission)
	}
	if _, present := detail["fields"]; !present {
		t.Fatal("detail.fields 必须存在（可为 null）")
	}
}

// TestAuthzDenyAuditorNilLoggerDoesNotPanic：未装配审计时拒绝路径仍正常，
// 中间件不因回调缺失而改变 403 语义。
func TestAuthzDenyAuditorNilLoggerDoesNotPanic(t *testing.T) {
	authz := newAuthzMiddleware(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := authz.Handler(rbac.PermUsersRead, rbac.AuthzAction{ResourceKind: "user", ActionName: "list"})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("下游不得执行") }))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/users", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（未认证）", rr.Code)
	}
}

// TestAuthzDenyAuditorDataScopeUses404：数据级拒绝（他人资源、无管理键）按
// 契约返回 404；审计 status_code 必须与客户端实际收到的状态一致。
func TestAuthzDenyAuditorDataScopeUses404(t *testing.T) {
	capture := &denyAuditCapture{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authz := newAuthzMiddleware(audit.New(capture), logger)
	authz.SetPermissionLoader(emptyPermissionLoader{})

	action := rbac.AuthzAction{ResourceKind: "token"}
	handler := authz.HandlerWithTarget("", action, func(*http.Request) int64 { return 99 })(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("下游不得执行") }))
	req := httptest.NewRequest(http.MethodGet, "/api/tokens/1", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(),
		&auth.Principal{UserID: 7, Role: rbac.RoleUser}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（数据级拒绝）", rr.Code)
	}
	rows := capture.snapshot()
	if len(rows) != 1 {
		t.Fatalf("审计行 = %d，期望 1", len(rows))
	}
	if status, ok := rows[0][7].(int16); !ok || status != http.StatusNotFound {
		t.Fatalf("status_code = %v（%T），期望 404", rows[0][7], rows[0][7])
	}
	detail := rows[0][9].(string)
	if !strings.Contains(detail, `"reason":"data_scope"`) {
		t.Fatalf("detail.reason 不是 data_scope: %s", detail)
	}
}

// recordingInvalidator 记录 Invalidate 调用次数（F3 装配测试）。
type recordingInvalidator struct{ calls int }

func (r *recordingInvalidator) Invalidate() { r.calls++ }

// TestPricingOnChangeWiredToSourceInvalidate 冻结 R-63 的装配接缝：
// newGatewayAPI 必须把定价源的 Invalidate 作为管理面写后回调（同一个函数值），
// 且 nil 源时给出 nil 回调（只靠 30s TTL），而不是空实现。
func TestPricingOnChangeWiredToSourceInvalidate(t *testing.T) {
	source := &recordingInvalidator{}
	onChange := pricingOnChange(source)
	if onChange == nil {
		t.Fatal("pricingOnChange(source) = nil, want source.Invalidate")
	}
	onChange()
	onChange()
	if source.calls != 2 {
		t.Fatalf("Invalidate calls = %d, want 2（回调必须直接送达定价源）", source.calls)
	}
	if got := pricingOnChange(nil); got != nil {
		t.Fatal("pricingOnChange(nil) must be nil（未接源时只靠 TTL）")
	}
}
