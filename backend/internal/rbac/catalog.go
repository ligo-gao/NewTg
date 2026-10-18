// Package rbac 实现管理面的权限目录与三层鉴权判定（BE-7）。
//
// 三层判定（security-02 §3.1）：
//  1. 功能级：调用者是否持有该动作所需的权限键（角色是外层门，权限键是内层门）；
//  2. 数据级：按资源归属隔离，ID 寻址的他人资源对无管理键者一律表现为「不存在」；
//  3. 字段级：写侧 BOPLA——请求体字段必须属于该控制器/动作登记的可写白名单，
//     越权或未知字段直接 403 field_not_writable，绝不静默丢弃（security-02 §2.6.1）。
//
// 本包不做认证（身份由 auth.SessionAuth 注入），也不直接读库：Principal.Permissions
// 的唯一真源是 users.admin_permissions，由装配层加载后传入，避免在鉴权包里引入
// store 依赖与第二真源缓存。
package rbac

import "newtg/gateway/internal/auth"

// 权限目录的 20 个键。唯一来源是 modules-07 F-07-08（security-02 §3.2 直接引用，
// 本包不重定义、不新增）。键名是前后端与 OpenAPI enum 的契约，改动属于设计变更。
const (
	PermUsersRead          = "users:read"
	PermUsersCreate        = "users:create"
	PermUsersUpdate        = "users:update"
	PermUsersDisable       = "users:disable"
	PermUsersDelete        = "users:delete"
	PermUsersAdjustQuota   = "users:adjust-quota"
	PermUsersManageRoles   = "users:manage-roles"
	PermUsersResetSecurity = "users:reset-security"

	PermGroupsManage = "groups:manage"

	PermChannelsRead            = "channels:read"
	PermChannelsManage          = "channels:manage"
	PermChannelsManageSensitive = "channels:manage_sensitive"

	PermPricingRead   = "pricing:read"
	PermPricingManage = "pricing:manage"

	PermRedemptionManage = "redemption:manage"

	PermOrdersRead   = "orders:read"
	PermOrdersManage = "orders:manage"

	PermLogsRead      = "logs:read"
	PermLogPromptView = "log.prompt.view"

	PermSettingsManage = "settings:manage"
)

// Role 取值（security-02 §3.2 的 ROLE = {user, admin}）。
// root 只是字段分区投影角色、没有独立身份来源，因此不参与功能级判定。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// Decision.Effect 的取值。
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

// Decision.Reason 的机器可读拒绝原因；Handler 据此选择状态码与 RFC 7807 type。
const (
	ReasonPermissionDenied = "permission_denied"
	ReasonNotFound         = "not_found"
	ReasonFieldNotWritable = "field_not_writable"
)

// WildcardPermission 是 ADMIN_PERMS_FULL 的显式形态（另一种形态是 JSON NULL /
// 数据库 NULL，在 Go 里表现为 nil 切片）。
const WildcardPermission = "*"

// permissionCatalog 保持 modules-07 表格顺序。顺序本身不承载语义，但保持与
// 文档一致便于人工比对；对外只经 Catalog() 以副本形式暴露。
var permissionCatalog = []string{
	PermUsersRead,
	PermUsersCreate,
	PermUsersUpdate,
	PermUsersDisable,
	PermUsersDelete,
	PermUsersAdjustQuota,
	PermUsersManageRoles,
	PermUsersResetSecurity,
	PermGroupsManage,
	PermChannelsRead,
	PermChannelsManage,
	PermChannelsManageSensitive,
	PermPricingRead,
	PermPricingManage,
	PermRedemptionManage,
	PermOrdersRead,
	PermOrdersManage,
	PermLogsRead,
	PermLogPromptView,
	PermSettingsManage,
}

var permissionSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(permissionCatalog))
	for _, key := range permissionCatalog {
		set[key] = struct{}{}
	}
	return set
}()

// Catalog 返回 20 键目录的副本。返回副本而不是内部切片：权限目录是安全边界，
// 调用方若把它当作可变的展示列表（排序、过滤）不应污染全局判定。
func Catalog() []string {
	out := make([]string, len(permissionCatalog))
	copy(out, permissionCatalog)
	return out
}

// IsPermissionKey 报告 key 是否属于冻结目录。鉴权层用它实现失败关闭：
// 拼写错误或尚未登记的新键不能被 FULL 管理员的 "*" 顺带放行。
func IsPermissionKey(key string) bool {
	_, ok := permissionSet[key]
	return ok
}

// HasPermission 报告 admin 身份是否持有 key。role=user 永远为 false——
// 权限数组只对 admin 有意义；user 行即使残留 admin_permissions 也不得提权。
func HasPermission(p auth.Principal, key string) bool {
	if p.Role != RoleAdmin || !IsPermissionKey(key) {
		return false
	}
	return hasFullPermissions(p.Permissions) || containsPermission(p.Permissions, key)
}

// hasFullPermissions 实现 ADMIN_PERMS_FULL 判定：NULL（nil）或含 "*"。
//
// 注意 nil 与空切片的区别是刻意的：nil 对应数据库 NULL = 全权；空数组
// [] 是一个明确的「没有任何权限」集合，不是 FULL。把 len()==0 当作 FULL
// 会让「刚被清空权限的管理员」意外保留全部权限。
func hasFullPermissions(perms []string) bool {
	if perms == nil {
		return true
	}
	for _, p := range perms {
		if p == WildcardPermission {
			return true
		}
	}
	return false
}

func containsPermission(perms []string, key string) bool {
	for _, p := range perms {
		if p == key {
			return true
		}
	}
	return false
}
