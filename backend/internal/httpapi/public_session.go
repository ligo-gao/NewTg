package httpapi

// 公开页的「软鉴权」解析（R-PUB-05；M6c Task 7）。
//
// 公开定价目录对匿名访客开放，但登录用户需要看到自己分组的实付价。因此公开
// 路由**不能**走 SessionAuth（它会对匿名返回 401），也不能把会话解析失败当成
// 错误：
//
//   - 有有效会话 → 解析出用户 id 与分组；
//   - 无会话 / 会话失效 / Redis 故障 / 用户行缺失 → 一律按匿名处理；
//   - 本文件不主动写任何数据（不 Touch、不续期、不落审计行）；唯一的写副作用
//     来自 SessionManager.Get 的既有行为——逻辑过期会话会被顺手清理。
//
// 【为什么不复用 SessionAuth】SessionAuth 的契约是 fail-closed：任何无法确认
// 会话的情形都必须 401。公开页的契约相反——定价是公开数据，会话只是「个性化
// 增量」。把两种策略塞进同一中间件会让其中一方被迫接受另一方的失败语义。
//
// 【为什么不 Touch】Touch 会刷新 sliding timeout。公开页可能被爬虫/匿名刷新
// 高频访问，替用户续期会话等于把「用户是否活跃」的判断交给第三方流量。

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"newtg/gateway/internal/auth"
)

// PublicUserGroupLookup 是软鉴权读取用户分组的接缝（单测注入内存实现）。
// found=false 表示用户行不存在或已停用；调用方按匿名处理。
type PublicUserGroupLookup interface {
	UserGroup(ctx context.Context, userID int64) (group string, found bool, err error)
}

// publicIdentity 是一次公开请求的身份视图。
type publicIdentity struct {
	UserID int64
	Group  string
	// Authenticated=true 表示会话有效；Group 可能为空（用户行缺失/查询失败），
	// 此时只影响个性化价格，不影响任何访问控制。
	Authenticated bool
}

// publicSessionResolver 组合会话读取与用户分组查询。
type publicSessionResolver struct {
	sessions *auth.SessionManager
	groups   PublicUserGroupLookup
}

// resolve 解析请求身份。任何失败都降级为匿名，绝不返回错误：公开只读面不能
// 因为 Redis/DB 的会话查询故障而整体不可用。
func (r *publicSessionResolver) resolve(ctx context.Context, req *http.Request) publicIdentity {
	if r == nil || r.sessions == nil || req == nil {
		return publicIdentity{}
	}
	sid, ok := sessionID(req)
	if !ok {
		return publicIdentity{}
	}
	sess, err := r.sessions.Get(ctx, sid)
	if err != nil || sess == nil || sess.UserID == 0 {
		// 会话不存在/过期/Redis 故障都按匿名处理（公开数据仍然可读）。
		return publicIdentity{}
	}
	identity := publicIdentity{UserID: sess.UserID, Authenticated: true}
	if r.groups != nil {
		group, found, err := r.groups.UserGroup(ctx, sess.UserID)
		if err == nil && found {
			identity.Group = group
		}
	}
	return identity
}

// sqlPublicUserGroupLookup 从主库读取 users."group"。
type sqlPublicUserGroupLookup struct {
	db *sql.DB
}

// UserGroup 只读取启用用户的当前分组；停用用户按 found=false 处理。
func (l *sqlPublicUserGroupLookup) UserGroup(ctx context.Context, userID int64) (string, bool, error) {
	if l == nil || l.db == nil {
		return "", false, nil
	}
	var group string
	err := l.db.QueryRowContext(ctx,
		`SELECT "group" FROM users WHERE id = $1 AND status = 'enabled'`, userID).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		// 调用方按匿名处理；不把 DB 细节带进公开响应。
		return "", false, err
	}
	return group, true, nil
}
