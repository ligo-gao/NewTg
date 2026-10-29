// R-04 管理员 MFA 强制绑定：管理面兜底中间件。
//
// 登录端点已经对未绑定管理员拒绝签发完整会话（返回 scope=enroll 票据），
// 但以下路径仍可能带着一个「未绑定」的管理员会话到达管理 API：
//
//   - 运营在已有管理员会话在线时把 auth.mfa.admin_required 打开；
//   - 会话在绑定之前通过其它签发路径建立（例如测试/运维脚本）；
//   - 未来新增的会话签发路径忘记复用登录判定。
//
// RequireMFAEnrolled 是纵深防御：管理员会话未绑定 TOTP 时，只放行完成绑定
// 与登出所必需的端点，其余管理 API 一律 403 mfa_enrollment_required。
//
// 只对 role=admin 生效；普通用户、Bearer 令牌路径、以及 admin_required=false
// 的部署完全不受影响（与 R-04「普通用户不受影响 / 关闭时行为回到当前」一致）。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// problemTypeMFAEnrollmentRequired 是未绑定管理员访问管理 API 的唯一拒绝出口。
// 前端据此提示「管理员必须先绑定 MFA」，而不是把它当成权限不足或会话失效。
const problemTypeMFAEnrollmentRequired = "https://newtg.dev/problems/mfa_enrollment_required"

// MFAEnrollmentPolicy 是中间件的判定依赖。
//
// Lookup 返回该用户是否已启用 TOTP；nil 表示强制绑定未装配（纯内存单测、
// 没有 users 表的嵌入式装配），此时中间件整体透传。
// Config 读取 auth.mfa.admin_required；nil 时按注册表默认值 true。
type MFAEnrollmentPolicy struct {
	Lookup func(ctx context.Context, userID int64) (bool, error)
	Config ConfigReader
}

// Enabled 报告中间件是否具备判定能力。
func (p *MFAEnrollmentPolicy) Enabled() bool {
	return p != nil && p.Lookup != nil
}

// AdminMFARequired 读取 auth.mfa.admin_required（注册表默认 true）。
func (p *MFAEnrollmentPolicy) AdminMFARequired() bool {
	if p == nil || p.Config == nil {
		return true
	}
	raw, ok := p.Config.Get("auth.mfa.admin_required")
	if !ok {
		return true
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return true
	}
	return v
}

// RequireMFAEnrolled 是管理员 MFA 绑定状态的兜底中间件。必须在 SessionAuth
// 之后执行（依赖 auth.Principal），放在 CSRF 之前：被拦截的请求不延长会话，
// 也不会先泄露 CSRF 校验结果。
func RequireMFAEnrolled(policy *MFAEnrollmentPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !policy.Enabled() || !policy.AdminMFARequired() {
				next.ServeHTTP(w, r)
				return
			}
			principal, ok := auth.PrincipalFrom(r.Context())
			if !ok || principal.Role != rbac.RoleAdmin {
				next.ServeHTTP(w, r)
				return
			}
			// 白名单先判：放行路径不查库，避免登录页/绑定页的每次轮询都打 users。
			if mfaEnrollmentAllowed(r) {
				next.ServeHTTP(w, r)
				return
			}
			enrolled, err := policy.Lookup(r.Context(), principal.UserID)
			if err != nil {
				// 无法确认绑定状态时 fail-closed，但用 503 而不是 403：
				// 这是基础设施故障，不是「你需要绑定 MFA」。
				writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
					"MFA enrollment status is temporarily unavailable")
				return
			}
			if enrolled {
				next.ServeHTTP(w, r)
				return
			}
			writeProblem(w, http.StatusForbidden, problemTypeMFAEnrollmentRequired,
				"Enroll in two-factor authentication to continue")
		})
	}
}

// mfaEnrollmentAllowed 是未绑定管理员会话的端点白名单（R-04 第 2 条）：
//
//   - /api/2fa/*：完成绑定本身（setup/enable/backup-codes）；
//   - POST /api/auth/logout 与 POST /api/logout：退出（实际路由是后者，
//     前者保留给契约里声明的路径）；
//   - /api/me（GET）与 /api/self、/api/self/*：账户自读与语言等设置；实际
//     身份投影端点是 /api/me（与受限会话的 passwordChangeAllowed 同口径），
//     /api/self 保留给契约声明的路径；
//   - GET /api/status：公开状态（通常不经过本中间件，显式放行以防装配差异）。
//
// 方法参与判定，避免未来给同名路径加写操作时被顺带放行。
func mfaEnrollmentAllowed(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/2fa/"):
		return true
	case path == "/api/auth/logout" || path == "/api/logout":
		return r.Method == http.MethodPost
	case path == "/api/me":
		return r.Method == http.MethodGet
	case path == "/api/self" || strings.HasPrefix(path, "/api/self/"):
		return r.Method == http.MethodGet || r.Method == http.MethodPut
	case path == "/api/status":
		return r.Method == http.MethodGet
	default:
		return false
	}
}

// newMFAEnrollmentPolicy 是生产装配的构造器：Repo 与 DB 二选一（与 2FA 端点
// 同一口径）。两者都缺失时返回 nil，中间件透传（纯内存装配）。
func newMFAEnrollmentPolicy(repo TwoFARepo, db *sql.DB, config ConfigReader) *MFAEnrollmentPolicy {
	if repo == nil && db != nil {
		repo = NewSQLTwoFARepo(db)
	}
	if repo == nil {
		return nil
	}
	return &MFAEnrollmentPolicy{
		Config: config,
		Lookup: func(ctx context.Context, userID int64) (bool, error) {
			user, err := repo.FindUserByID(ctx, userID)
			if err != nil {
				return false, err
			}
			return user != nil && user.TOTPEnabled, nil
		},
	}
}
