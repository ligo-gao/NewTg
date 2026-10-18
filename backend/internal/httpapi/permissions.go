package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

// 本文件实现 R-P9 的最小权限检查：
//   - 只需要 reading auth.Principal，不引入完整 RBAC 框架；
//   - Principal.Permissions 为空时（Cookie 会话永远为空；Bearer 路径也为空）
//     按 UserID 回源 users 表，NULL 或 ["*"] = 全权；
//   - 非 admin / 缺键 → 403 RFC7807；未鉴权 → 401（绝不能把零值 Principal 当匿名放行）。
//
// 为什么不在 SessionAuth 里塞权限：会话记录刻意不复制 admin_permissions
// （auth.Principal 注释，计划 GC-3），users 表是唯一真源；否则角色/权限变更后
// 会出现第二真源与陈旧授权。

// 权限键冻结自 modules-07 F-07-08 的权限目录（本模块只消费 channels 三键）。
const (
	// PermChannelsRead 覆盖渠道列表/详情查询（modules-06 F-06-01、F-06-03 回显）。
	PermChannelsRead = "channels:read"
	// PermChannelsManage 覆盖渠道创建/编辑/启停/测试（modules-06 F-06-02/03/06/08）。
	PermChannelsManage = "channels:manage"
	// PermChannelsManageSensitive 覆盖渠道删除与凭据敏感操作（modules-06 F-06-02；
	// modules-07 明确要求与 channels:manage 分离，防权限过宽）。
	PermChannelsManageSensitive = "channels:manage_sensitive"
)

// UserAccessStore 是权限回源接缝。生产实现是 *store.TokenRepo；
// 测试注入内存 fake（R-P10：不要求真 PG）。
type UserAccessStore interface {
	// UserAccess 返回 users.role、admin_permissions（SQL NULL → nil）与 users.status。
	UserAccess(ctx context.Context, userID int64) (role string, permissions []string, status string, err error)
}

var (
	// ErrNoPrincipal 表示请求未经过 SessionAuth（或中间件未挂载）。
	ErrNoPrincipal = errors.New("httpapi: principal missing")
	// ErrPermissionDenied 表示已认证但角色/权限不足。
	ErrPermissionDenied = errors.New("httpapi: permission denied")
	// ErrAccessStore 包装回源存储故障：调用方应映射为 500 而不是 403，
	// 以免把数据库故障伪装成「没有权限」。对外仍不返回内部错误文本。
	ErrAccessStore = errors.New("httpapi: user access store failed")
	// ErrUserDeleted 表示已认证身份对应的 users 行已不存在（会话/令牌指向
	// 已删除用户）。与 ErrNoPrincipal 一样映射 401：这不是权限不足，
	// 而是身份已失效（M2 final review finding 9，避免塌缩成 500）。
	ErrUserDeleted = errors.New("httpapi: authenticated user no longer exists")
)

// CheckPermission 判定当前请求是否持有 required 权限键。
//
// 判定顺序：
//  1. 无 Principal → ErrNoPrincipal（401）。
//  2. Principal.Permissions 非空 → 直接使用（角色必须为 admin）。
//     这条快路径服务未来在 Principal 上携带权限的调用方；当前 SessionAuth 不填。
//  3. Permissions 为空 → 回源 users；role 必须为 admin 且 status=enabled，
//     权限集合为 NULL 或含 "*" 时全权，否则精确成员判定。
func CheckPermission(ctx context.Context, access UserAccessStore, required string) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok || p == nil {
		return ErrNoPrincipal
	}
	if len(p.Permissions) > 0 {
		if p.Role != "admin" {
			return ErrPermissionDenied
		}
		if slices.Contains(p.Permissions, "*") || slices.Contains(p.Permissions, required) {
			return nil
		}
		return ErrPermissionDenied
	}

	if access == nil {
		// fail-closed：不能确认权限时一律拒绝（security-01 I12）。
		return ErrPermissionDenied
	}
	role, permissions, status, err := access.UserAccess(ctx, p.UserID)
	if err != nil {
		// 用户已被删除：身份失效 → 401（finding 9），不再塌缩成 500。
		if errors.Is(err, store.ErrUserNotFound) {
			return ErrUserDeleted
		}
		return fmt.Errorf("%w: %v", ErrAccessStore, err)
	}
	if role != "admin" || status != "enabled" {
		return ErrPermissionDenied
	}
	// permissions == nil 代表 SQL NULL = 全权；空切片代表显式无权限。
	if permissions == nil || slices.Contains(permissions, "*") || slices.Contains(permissions, required) {
		return nil
	}
	return ErrPermissionDenied
}

// RequirePermission 是与 CheckPermission 同语义的中间件形态，供 phase 8B 的
// relay/管理端点按路由挂载；本任务的渠道 handler 直接调用 CheckPermission，
// 以便把存储故障与权限不足映射成不同状态码。
func RequirePermission(access UserAccessStore, required string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := CheckPermission(r.Context(), access, required)
			switch {
			case err == nil:
				next.ServeHTTP(w, r)
			case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
				writeUnauthorized(w)
			case errors.Is(err, ErrAccessStore):
				// 不向客户端泄漏存储错误；fail-closed。
				writeProblem(w, http.StatusInternalServerError,
					"https://newtg.dev/problems/internal_error", "Internal Server Error")
			default:
				writeForbidden(w)
			}
		})
	}
}

// writeForbidden 是所有授权失败的统一出口。type URI 未在设计里冻结，
// 沿用 modules-03/09 的 https://newtg.dev/problems/<slug> 约定。
func writeForbidden(w http.ResponseWriter) {
	writeProblem(w, http.StatusForbidden,
		"https://newtg.dev/problems/forbidden", "Forbidden")
}
