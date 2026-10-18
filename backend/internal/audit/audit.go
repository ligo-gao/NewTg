// Package audit 写入 append-only 的 audit_logs（日志库）。
//
// 设计权威：docs/modules-05-用量日志与统计-子功能设计.md §7（事件分类、
// before/after 脱敏快照）、docs/platform-01-数据库字段汇总.md §5（14 列）。
//
// 审计行是合规追溯面：本包只做 INSERT（无 UPDATE/DELETE），并且在落库前
// 对 detail/before/after 做递归脱敏——口令、API key、key_hash、TOTP secret、
// 备用码等材料绝不允许进入审计表。动作名（action）落 detail.action 之外，
// 同时用于推导 category（modules-05 §7.1 的四类）。
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// redacted 是敏感字段的替换值。测试依赖该常量。
const redacted = "[REDACTED]"

var (
	// ErrUnknownAction 表示动作名不在冻结枚举内：拒绝写入，避免自由文本动作
	// 让审计分类漂移。
	ErrUnknownAction = errors.New("audit: unknown action")
	// ErrNotConfigured 表示包级 Log 尚未安装默认 Logger。
	ErrNotConfigured = errors.New("audit: default logger not configured")
	// ErrInvalidJSON 表示 detail/before/after 不是合法 JSON。
	ErrInvalidJSON = errors.New("audit: invalid JSON payload")
)

// 冻结的动作枚举（本计划 Task 12 冻结，见实现计划 Interfaces）。
const (
	ActionAuthLogin          = "auth.login"
	ActionAuthLogout         = "auth.logout"
	ActionAuthRegister       = "auth.register"
	ActionAuthPasswordChange = "auth.password_change"
	ActionAuthPasswordReset  = "auth.password_reset"
	ActionAuth2FAEnable      = "auth.2fa_enable"
	ActionAuth2FADisable     = "auth.2fa_disable"
	// ActionAuth2FABackupCodesRegenerate 是 modules-01 F-01-10 的
	// backup_codes_regenerated 事件（备用码整体重置，旧码立即失效）。
	ActionAuth2FABackupCodesRegenerate = "auth.2fa_backup_codes_regenerate"

	ActionTokenCreate       = "token.create"
	ActionTokenUpdate       = "token.update"
	ActionTokenDelete       = "token.delete"
	ActionTokenResetKey     = "token.reset_key"
	ActionTokenEnable       = "token.enable"
	ActionTokenDisable      = "token.disable"
	ActionTokenBatchEnable  = "token.batch_enable"
	ActionTokenBatchDisable = "token.batch_disable"
	ActionTokenBatchDelete  = "token.batch_delete"

	ActionUserCreate        = "user.create"
	ActionUserUpdate        = "user.update"
	ActionUserDisable       = "user.disable"
	ActionUserEnable        = "user.enable"
	ActionUserDelete        = "user.delete"
	ActionUserQuotaAdjust   = "user.quota_adjust"
	ActionUserReset2FA      = "user.reset_2fa"
	ActionUserResetPassword = "user.reset_password"

	// ActionGroup* 是 modules-07 F-07-10 的用户分组管理事件（operation 类）。
	ActionGroupCreate = "group.create"
	ActionGroupUpdate = "group.update"
	ActionGroupDelete = "group.delete"

	// ActionSystemInitialized 是 modules-01 F-01-20 冻结的首次初始化事件。
	// 它不属于某个用户的日常操作，但在审计面上必须可查（验收：初始化动作
	// 在审计日志可查），故归于 operation 类。
	ActionSystemInitialized = "system_initialized"
)

// categoryLogin 等是 audit_logs.category 的 CHECK 白名单（platform-01 §5 R3）。
const (
	categoryLogin       = "login"
	categorySecurity    = "security"
	categoryOperation   = "operation"
	categoryAccessToken = "access_token"
)

// actionCategories 是动作 → 分类的唯一映射表。
//
// 口径来自 modules-05 §7.1：登录/登出/注册属 login；改密/重置/2FA 属 security；
// 令牌与用户管理属 operation。access_token 类别留给未来的 PAT（模块 01 假设），
// 本计划冻结的动作枚举里没有成员，故不映射。
var actionCategories = map[string]string{
	ActionAuthLogin:                    categoryLogin,
	ActionAuthLogout:                   categoryLogin,
	ActionAuthRegister:                 categoryLogin,
	ActionAuthPasswordChange:           categorySecurity,
	ActionAuthPasswordReset:            categorySecurity,
	ActionAuth2FAEnable:                categorySecurity,
	ActionAuth2FADisable:               categorySecurity,
	ActionAuth2FABackupCodesRegenerate: categorySecurity,

	ActionTokenCreate:       categoryOperation,
	ActionTokenUpdate:       categoryOperation,
	ActionTokenDelete:       categoryOperation,
	ActionTokenResetKey:     categoryOperation,
	ActionTokenEnable:       categoryOperation,
	ActionTokenDisable:      categoryOperation,
	ActionTokenBatchEnable:  categoryOperation,
	ActionTokenBatchDisable: categoryOperation,
	ActionTokenBatchDelete:  categoryOperation,

	ActionUserCreate:        categoryOperation,
	ActionUserUpdate:        categoryOperation,
	ActionUserDisable:       categoryOperation,
	ActionUserEnable:        categoryOperation,
	ActionUserDelete:        categoryOperation,
	ActionUserQuotaAdjust:   categoryOperation,
	ActionUserReset2FA:      categoryOperation,
	ActionUserResetPassword: categoryOperation,

	ActionGroupCreate: categoryOperation,
	ActionGroupUpdate: categoryOperation,
	ActionGroupDelete: categoryOperation,

	ActionSystemInitialized: categoryOperation,
}

// Category 返回动作名对应的审计分类；未知动作 ok=false。
func Category(action string) (string, bool) {
	category, ok := actionCategories[action]
	return category, ok
}

// Execer 是 Logger 需要的数据库能力。*sql.DB 直接满足；单测用 fake 替代，
// 保证未打 integration 标签的测试不依赖外部数据库（GC-8）。
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Event 是一次审计写入的完整输入。
//
// Logger.Log（冻结签名）只覆盖 action/actor/detail；需要 before/after 的
// 变更类事件用 LogEvent 传入完整快照。
type Event struct {
	Action     string
	Actor      int64
	Username   string // 操作者用户名快照；空时回退 UsernameResolver 或 user#<id>
	Result     string // success（默认）| failed
	IP         string
	UserAgent  string
	Method     string
	Route      string
	StatusCode int
	Detail     json.RawMessage
	Before     json.RawMessage
	After      json.RawMessage
}

// Logger 写入日志库 audit_logs。可并发使用。
type Logger struct {
	exec Execer
	// UsernameResolver 可选：把 actor ID 解析为用户名快照。nil 时回退 user#<id>。
	UsernameResolver func(ctx context.Context, userID int64) (string, error)
}

// New 构造审计写入器。exec 为日志库连接（platform-01 §5：审计表在日志库）。
func New(exec Execer) *Logger {
	return &Logger{exec: exec}
}

// insertSQL 是唯一写路径：append-only（modules-05 §7、platform-01 §5）。
// jsonb 参数显式 ::jsonb 转型，避免驱动把 []byte 当成 bytea。
const insertSQL = `INSERT INTO audit_logs
	(user_id, username, category, ip, user_agent, method, route, status_code, result, detail, before, after)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb)`

// Log 写入一条简单审计事件（冻结签名）。detail 为 nil 时写 NULL。
func (l *Logger) Log(ctx context.Context, action string, actor int64, detail json.RawMessage) error {
	return l.LogEvent(ctx, Event{Action: action, Actor: actor, Detail: detail})
}

// LogEvent 写入完整事件。任何敏感材料在落库前被替换为 [REDACTED]。
func (l *Logger) LogEvent(ctx context.Context, e Event) error {
	category, ok := actionCategories[e.Action]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAction, e.Action)
	}
	if l == nil || l.exec == nil {
		return ErrNotConfigured
	}

	result := e.Result
	if result == "" {
		result = "success"
	}
	if result != "success" && result != "failed" {
		return fmt.Errorf("audit: invalid result %q", result)
	}

	username := strings.TrimSpace(e.Username)
	if username == "" && l.UsernameResolver != nil {
		if resolved, err := l.UsernameResolver(ctx, e.Actor); err == nil && strings.TrimSpace(resolved) != "" {
			username = strings.TrimSpace(resolved)
		}
	}
	if username == "" {
		// 快照缺失时也必须满足 NOT NULL；user#<id> 明确表示「未解析」而不是伪造用户名。
		username = fmt.Sprintf("user#%d", e.Actor)
	}

	detail, err := sanitizeJSON(e.Detail)
	if err != nil {
		return err
	}
	before, err := sanitizeJSON(e.Before)
	if err != nil {
		return err
	}
	after, err := sanitizeJSON(e.After)
	if err != nil {
		return err
	}

	args := []any{
		e.Actor,
		username,
		category,
		nullableText(e.IP),
		nullableText(e.UserAgent),
		nullableText(e.Method),
		nullableText(e.Route),
		nullableStatus(e.StatusCode),
		result,
		detail,
		before,
		after,
	}
	_, err = l.exec.ExecContext(ctx, insertSQL, args...)
	return err
}

// 包级默认 Logger 支撑冻结的 audit.Log 形态；应用装配时 SetDefault。
var defaultLogger atomic.Pointer[Logger]

// SetDefault 安装包级默认 Logger（nil 表示卸载）。
func SetDefault(l *Logger) { defaultLogger.Store(l) }

// Log 是冻结的包级入口：audit.Log(ctx, action, actor, detail)。
func Log(ctx context.Context, action string, actor int64, detail json.RawMessage) error {
	l := defaultLogger.Load()
	if l == nil {
		return ErrNotConfigured
	}
	return l.Log(ctx, action, actor, detail)
}

func nullableText(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullableStatus(code int) any {
	if code <= 0 {
		return nil
	}
	return int16(code)
}

// secretKeyNormalized 是脱敏键的归一化集合（小写、去掉 _ 与 -）。
// 「key」「token」等通用词也在清单内：审计宁可少记，也不能漏记。
var secretKeyNormalized = map[string]struct{}{
	"password":        {},
	"newpassword":     {},
	"oldpassword":     {},
	"currentpassword": {},
	"confirmpassword": {},
	"passwordhash":    {},
	"passwd":          {},
	"key":             {},
	"keyhash":         {},
	"apikey":          {},
	"plain":           {},
	"plaintext":       {},
	"token":           {},
	"secret":          {},
	"totpsecret":      {},
	"backupcodes":     {},
	"authorization":   {},
	"privatekey":      {},
	"clientsecret":    {},
	"credential":      {},
	"credentials":     {},
}

func normalizeSecretKey(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	return key
}

// redactValue 递归替换敏感键的值。数组保留结构（只替换元素内的敏感键）。
func redactValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		for k, child := range typed {
			if _, secret := secretKeyNormalized[normalizeSecretKey(k)]; secret {
				typed[k] = redacted
				continue
			}
			typed[k] = redactValue(child)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = redactValue(child)
		}
		return typed
	default:
		return v
	}
}

// sanitizeJSON 校验并脱敏一个 jsonb 载荷；nil/空/`null` 统一写 NULL。
func sanitizeJSON(raw json.RawMessage) (any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	value = redactValue(value)
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	return string(out), nil
}
