package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"newtg/gateway/internal/auth"
)

// 本文件实现 auth.TokenLookup（Bearer 令牌鉴权）与用户权限回源查询（R-P9）。
// 两者都只读，不需要事务；错误文本不得包含 key_hash / 用户输入（GC-6）。

var (
	// ErrTokenNotFound 表示 key_hash 无对应令牌。对外必须与「令牌非法」同语义
	// （relay-01 §3.6），调用方只在内部区分排障。
	ErrTokenNotFound = errors.New("store: token not found")
	// ErrUserNotFound 表示 UserAccess 的目标用户不存在（会话指向已删除用户）。
	ErrUserNotFound = errors.New("store: user not found")
)

// TokenRepo 是 api_tokens / users 的只读仓储。
// 构造签名与 ChannelRepo 一致：NewTokenRepo(platform.OpenDB 返回的 *sql.DB)。
type TokenRepo struct{ db *sql.DB }

// 编译期断言：SessionAuth / relay.BuildRelayContext 只依赖 auth.TokenLookup，
// 实现漂移必须在这里编译失败，而不是等到 wiring。
var _ auth.TokenLookup = (*TokenRepo)(nil)

// NewTokenRepo 构造只读仓储。
func NewTokenRepo(db *sql.DB) *TokenRepo { return &TokenRepo{db: db} }

// Resolve 实现 auth.TokenLookup：入参是 Bearer 明文的 SHA-256（auth.HashTokenKey），
// 出参是鉴权与中继所需的最小投影。
//
// remain_quota 的 NULL 语义（platform-01 §2）：NULL = 不限额度。
// 必须映射为 UnlimitedQuota=true，而不是误读成 0（会把不限额度令牌判成余额耗尽）。
// 状态/过期/额度判定不在本层做：SessionAuth 的 tokenUsable 是唯一消费点，
// 避免两处判定顺序漂移（relay-01 §3.3）。
//
// model_limits / allow_ips 是 relay-01 §3.4 的附加校验输入（M2 final review finding 6）：
// 管理面此前完全不读这两列，导致受限令牌可以调用任意模型、从任意 IP 使用。
// JSON 损坏时返回错误（fail-closed → 401），绝不放行成「无限制」。
func (r *TokenRepo) Resolve(ctx context.Context, keyHash string) (*auth.TokenMeta, error) {
	var (
		meta        auth.TokenMeta
		expires     sql.NullTime
		remain      sql.NullInt64
		modelLimits []byte
		allowIPs    []byte
	)
	err := r.db.QueryRowContext(ctx, `SELECT id, user_id, status, "group", expires_at, remain_quota,
		model_limits, allow_ips
		FROM api_tokens WHERE key_hash = $1`, keyHash).
		Scan(&meta.ID, &meta.UserID, &meta.Status, &meta.Group, &expires, &remain,
			&modelLimits, &allowIPs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: resolve token: %w", err)
	}
	meta.ModelLimits, err = decodeJSONStringArray(modelLimits)
	if err != nil {
		return nil, fmt.Errorf("store: invalid model_limits for token %d: %w", meta.ID, err)
	}
	meta.AllowIPs, err = decodeJSONStringArray(allowIPs)
	if err != nil {
		return nil, fmt.Errorf("store: invalid allow_ips for token %d: %w", meta.ID, err)
	}
	if expires.Valid {
		t := expires.Time
		meta.ExpiresAt = &t
	}
	if remain.Valid {
		meta.RemainQuota = remain.Int64
	} else {
		meta.UnlimitedQuota = true
	}
	return &meta, nil
}

// UserAccess 回源 users.role / admin_permissions / status（R-P9）。
//
// admin_permissions 口径（modules-07 F-07-08）：
//   - SQL NULL          → permissions == nil，表示全权管理员；
//   - ["*"]             → 返回 ["*"]，同样全权；
//   - ["channels:read"] → 精确键集合。
//
// 刻意保留 nil 与空切片的区别：JSON `[]` 解出非 nil 空切片 = 无任何权限，
// 而 NULL = 全权。若把两者压平，一个被清空权限的管理员会意外获得全权。
func (r *TokenRepo) UserAccess(ctx context.Context, userID int64) (string, []string, string, error) {
	var (
		role   string
		raw    []byte
		status string
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT role, admin_permissions, status FROM users WHERE id = $1`, userID).
		Scan(&role, &raw, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, "", ErrUserNotFound
	}
	if err != nil {
		return "", nil, "", fmt.Errorf("store: load user access: %w", err)
	}
	if raw == nil {
		return role, nil, status, nil
	}
	permissions, err := decodeAdminPermissions(raw)
	if err != nil {
		// 数据损坏：返回错误而不是降级为全权（fail-closed，security-01 I12）。
		return "", nil, "", fmt.Errorf("store: invalid admin_permissions for user %d: %w", userID, err)
	}
	return role, permissions, status, nil
}

// decodeAdminPermissions 把 users.admin_permissions 的原始 jsonb 解码为权限集合。
//
// 三种形状必须严格区分（M2 final review finding 15）：
//   - SQL NULL（raw == nil）      → (nil, nil)：**全权管理员**；
//   - jsonb `null`（raw=="null"） → 非 nil 空切片：**没有任何权限**；
//   - JSON 数组                   → 精确键集合（`[]` 同样是非 nil 空切片）。
//
// 为什么不能只依赖 json.Unmarshal：`null` 与 `[]` 都会得到 nil/空结果，
// 若把 jsonb null 与 SQL NULL 压平，一个被显式清空权限的管理员会意外拿回全权。
func decodeAdminPermissions(raw []byte) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// 显式「无权限」：非 nil 空切片，CheckPermission 不会把它当全权。
		return []string{}, nil
	}
	var permissions []string
	if err := json.Unmarshal(raw, &permissions); err != nil {
		return nil, err
	}
	if permissions == nil {
		// 防御：数组字面量不会产生 nil；未知形状按无权限处理（fail-closed）。
		return []string{}, nil
	}
	return permissions, nil
}

// decodeJSONStringArray 解码 api_tokens 的 jsonb 文本数组列（model_limits / allow_ips）。
//
// 语义（platform-01 §2）：SQL NULL = 不限；jsonb `null`/`[]` 同样视为不限。
// 非数组或含非字符串元素视为数据损坏并返回错误（fail-closed），
// 绝不放行成「无限制」。
func decodeJSONStringArray(raw []byte) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, err
	}
	// 去空白并丢弃空项：白名单里的空串没有匹配语义。
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out, nil
}
