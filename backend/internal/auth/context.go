package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// security-01 §3.1 冻结的线协议常量。Cookie 名与请求头名是前后端契约的一部分，
// 改动属于设计变更：__Host- 前缀强制 Secure + Path=/ + 无 Domain（host-only），
// 防止子域覆盖与 Cookie 固定（security-08 §2.7）。
const (
	// SessionCookieName 是服务端不透明会话 id 的 Cookie（HttpOnly，服务端只存 Redis）。
	SessionCookieName = "__Host-nkt_sess"
	// CSRFCookieName 是同步器令牌的 Cookie（非 HttpOnly，前端读取后写入请求头）。
	CSRFCookieName = "__Host-nkt_csrf"
	// CSRFHeaderName 是前端提交 CSRF 令牌的冻结头名。
	CSRFHeaderName = "X-CSRF-Token"
)

// Principal 是一次请求的已认证身份，由鉴权中间件注入 request.Context()。
//
// 字段口径（计划 GC-3）：
//   - UserID/TokenID 一律 int64（DB bigint），禁止 string/uint64 混用；
//   - TokenID 只在 Bearer 路径非 nil（relay-01 §1.5：会话路径 token_id 为空）；
//   - Permissions 刻意不放进会话存储：权限的唯一真源是 users.admin_permissions，
//     会话里再复制一份会在角色/权限变更后变成第二真源。RBAC 层按 UserID 读取权威值。
type Principal struct {
	UserID      int64
	Role        string
	Permissions []string
	TokenID     *int64

	// ModelLimits 是令牌的模型白名单投影（api_tokens.model_limits）。
	// 由 RelayAuth 从 TokenMeta 复制；空/nil 表示不限模型。
	// 管理面会话路径不填（会话没有令牌白名单语义）。
	ModelLimits []string
}

// principalContextKey 是私有空结构：避免与其他包的 context key 碰撞，
// 也避免字符串键被外部伪造。
type principalContextKey struct{}

// WithPrincipal 返回携带身份的新 context。nil 身份是 no-op 而不是注入 nil：
// 这样「忘了传身份」不会被下游误判为已认证。
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFrom 取出当前身份。ok=false 表示请求未经过鉴权（或中间件未挂载），
// 调用方必须据此拒绝，而不是把零值 Principal 当作匿名用户放行。
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*Principal)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}

// TokenMeta 是令牌鉴权所需的最小元数据投影。其余令牌字段（模型限制、IP 白名单等）
// 由中继鉴权阶段消费，不属于管理面会话中间件的职责。
type TokenMeta struct {
	ID             int64
	UserID         int64
	Status         string
	Group          string
	ExpiresAt      *time.Time
	RemainQuota    int64
	UnlimitedQuota bool
	// ModelLimits / AllowIPs 是 relay-01 §3.4 的附加校验输入：
	// model_limits 是模型白名单（nil/空 = 不限），allow_ips 是客户端 IP
	// 白名单（nil/空 = 不限，元素可为 IP 或 CIDR）。两者都来自 api_tokens
	// 的 jsonb 列，SQL NULL 与 jsonb [] 均表示「不限」。
	ModelLimits []string
	AllowIPs    []string
}

// TokenLookup 由 Task 12 的令牌存储实现：入参是 Bearer 明文的 SHA-256 哈希
// （DB 只存哈希，明文仅在创建响应出现一次，modules-02 §1）。
//
// 为什么定义在 auth 而不是 httpapi（SessionAuth 所在的包）：
// tokenstore 需要返回 *TokenMeta 才能实现本接口；若类型定义在 httpapi，
// tokenstore 必须 import httpapi，而 httpapi 的令牌 handler 又 import tokenstore，
// 形成 import cycle。auth 是两侧都依赖的叶子包，把契约放这里可保持依赖单向。
type TokenLookup interface {
	Resolve(ctx context.Context, keyHash string) (*TokenMeta, error)
}

// HashTokenKey 返回写 api_tokens.key_hash 的 SHA-256 小写十六进制。
//
// 与 HashSessionID 使用同一编码口径，避免「会话哈希用 hex、令牌哈希用 base64」
// 这类漂移；Bearer 路径在查询前必须先经本函数，绝不可以明文访问存储。
func HashTokenKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
