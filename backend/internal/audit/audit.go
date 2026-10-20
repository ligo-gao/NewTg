// Package audit 写入 append-only 的 audit_logs（日志库）。
//
// 设计权威：docs/modules-05-用量日志与统计-子功能设计.md §7（事件分类、
// before/after 脱敏快照）、docs/platform-01-数据库字段汇总.md §5（14 列）。
//
// 审计行是合规追溯面：本包只做 INSERT（无 UPDATE/DELETE），并且在落库前
// 对 detail/before/after 做递归脱敏——口令、API key、key_hash、TOTP secret、
// 备用码等材料绝不允许进入审计表。动作名（action）用于推导 category
// （modules-05 §7.1 的四类），并由 writer 统一注入 detail.action（裁决 R-43：
// 调用侧不得自行定义动作名，避免同一事件出现两个动作名真源）。
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"
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

	// ActionLogExport 是 modules-05 F-05-07 / §9.1 的日志导出事件。detail
	// 只允许 scope/format/row_count/筛选维度名，绝不记录导出行内容（设计
	// 明确「不记日志内容」，避免审计表成为敏感数据的第二副本）。
	ActionLogExport = "log.export"

	// ActionAuthzDenied 是 RBAC 授权拒绝事件（security-09 §2.7 设计 B /
	// security-02 TC-B-17）：并入既有 security 类，不新增第 5 个 category
	// （platform-01 §5 的 CHECK 变更成本高）。detail 由装配层写成固定形状
	// {event:"authz_denied", required_permission, route}；detail.action 仍由
	// writer 统一注入为本常量值（裁决 R-43）。
	ActionAuthzDenied = "authz.denied"

	// ActionSettingsUpdate 是 PUT /api/settings 的写审计（modules-08 §1.3 /
	// §3.8 F-08-22：所有设置写都必须留下 before/after）。归 operation 类，
	// 与用户/渠道/分组的配置变更同组。
	ActionSettingsUpdate = "settings.update"

	// ActionSettingsTestWebhook 是 POST /api/settings/test/webhook 的审计
	// （modules-08 F-08-20 / D2-14）。detail 只允许 event/status_code/ok/host：
	// 密钥、路径、query 与响应体原文都不得进入审计。
	ActionSettingsTestWebhook = "settings.test_webhook"

	// ActionNotificationTemplateUpdate 是通知模板编辑的写审计
	// （modules-08 §3.9 / F-08-25；brief 要求所有写操作审计）。模板行虽不在
	// system_settings，但属于管理配置，归 operation 类。
	ActionNotificationTemplateUpdate = "notification_template.update"

	// M6d 模型部署控制面（modules-08 §3.20.6）。两类事件都归 operation：
	// 设置更新是管理配置，连接测试是对外部 provider 的探测（也是管理动作）。
	// detail 只允许 enabled/has_secret 这类布尔事实，密钥值与其任何片段
	// 绝不进入审计（§3.20.5）。
	ActionDeploySettingsUpdate = "deploy.settings_update"
	ActionDeployConnectionTest = "deploy.connection_test"
	// T4 的管理面写操作（modules-08 §3.20.6）。detail 只允许非秘密字段：
	// 镜像地址剥离 userinfo/query，秘密值与环境变量键名一律不记。
	ActionDeployCreate = "deploy.create"
	ActionDeployUpdate = "deploy.update"
	ActionDeployRename = "deploy.rename"
	ActionDeployExtend = "deploy.extend"
	ActionDeployDelete = "deploy.delete"

	// M6d 任务插件管理面（modules-08 §3.10 / relay-06 §2.6）。全部归 operation：
	// 上传/激活/删除版本与市场操作都是管理配置变更。detail 只允许 key/version/
	// source_hash 前 8 位这类非秘密事实；source_text 与市场包体绝不进入审计
	// （S5：插件列表/详情/审计都不含源码正文），避免审计表成为源码的第二副本。
	ActionPluginUpload             = "plugin.upload"
	ActionPluginActivate           = "plugin.activate"
	ActionPluginVersionDelete      = "plugin.version_delete"
	ActionPluginEnable             = "plugin.enable"
	ActionPluginDisable            = "plugin.disable"
	ActionPluginMarketSourceUpdate = "plugin.market_source_update"
	ActionPluginMarketInstall      = "plugin.market_install"
	ActionPluginDryRun             = "plugin.dryrun"

	// D1b 模型目录 / 供应商目录（modules-06 F-06-22～26）。全部归 operation：
	// 目录元数据是管理配置，同步是对目录的批量写入。detail 只允许 id / 名称 /
	// 影响计数，绝不记录整行 JSON（图标/描述等展示内容无审计价值，且会把
	// 目录内容复制进审计表）。
	ActionModelCreate      = "model.create"
	ActionModelUpdate      = "model.update"
	ActionModelDelete      = "model.delete"
	ActionModelBatchDelete = "model.batch_delete"
	ActionModelSync        = "model.sync"

	ActionVendorCreate     = "vendor.create"
	ActionVendorUpdate     = "vendor.update"
	ActionVendorDelete     = "vendor.delete"
	ActionVendorOperations = "vendor.operations"

	// D3a 性能管理（modules-08 §3.19 / F-08-48～50）。动作名沿用 log.export
	// 的「域.动词」风格，由 writer 注入 detail.action（audit_logs 没有独立
	// action 列；§3.19.2 的审计写入映射）。三类都是运维动作，归 operation。
	// detail 只允许计数/字节数/耗时/实例标识，绝不记录缓存或日志正文
	// （§3.19.2 明确要求）。
	ActionSystemDiskCacheClear = "system.disk_cache.clear"
	ActionSystemGCRun          = "system.gc.run"
	ActionSystemLogCleanupRun  = "system.log_cleanup.run"

	// D5b 订阅套餐管理面与用户扣费偏好（billing-08 §15.1/§15.2）。全部归
	// operation：套餐是管理配置，偏好是用户设置。detail 只允许 id/plan_key/
	// 受影响计数；用户偏好的 before/after 只允许四值枚举，绝不记录整行或
	// 请求上下文（§15.4 的可见性口径同样约束审计）。
	ActionSubscriptionPlanCreate     = "subscription_plan.create"
	ActionSubscriptionPlanUpdate     = "subscription_plan.update"
	ActionSubscriptionPlanDelete     = "subscription_plan.delete"
	ActionSubscriptionPlanEnable     = "subscription_plan.enable"
	ActionSubscriptionPlanDisable    = "subscription_plan.disable"
	ActionSubscriptionPlanResetQuota = "subscription_plan.reset_quota"
	ActionBillingPreferenceUpdate    = "billing_preference.update"
	// 用户周期末取消（billing-08 §15.2 P1）。属用户自助的资金/权益状态变更，
	// 仍归 operation；detail 仅记录订阅 id，不复制行内价格/额度。
	ActionSubscriptionCancel = "subscription.cancel"

	// D7a 站点公告（modules-08 F-08-04 / R-D7-08）。全部归 operation：
	// 创建/编辑/发布/下线/删除都是管理配置变更。detail 只允许 id/title/
	// status——公告正文可能含未公开内容，审计表不得成为它的第二副本。
	ActionAnnouncementCreate  = "announcement.create"
	ActionAnnouncementUpdate  = "announcement.update"
	ActionAnnouncementPublish = "announcement.publish"
	ActionAnnouncementOffline = "announcement.offline"
	ActionAnnouncementDelete  = "announcement.delete"
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

	ActionLogExport: categoryOperation,

	ActionAuthzDenied: categorySecurity,

	ActionSettingsUpdate:      categoryOperation,
	ActionSettingsTestWebhook: categoryOperation,

	ActionNotificationTemplateUpdate: categoryOperation,

	ActionDeploySettingsUpdate: categoryOperation,
	ActionDeployConnectionTest: categoryOperation,
	ActionDeployCreate:         categoryOperation,
	ActionDeployUpdate:         categoryOperation,
	ActionDeployRename:         categoryOperation,
	ActionDeployExtend:         categoryOperation,
	ActionDeployDelete:         categoryOperation,

	ActionPluginUpload:             categoryOperation,
	ActionPluginActivate:           categoryOperation,
	ActionPluginVersionDelete:      categoryOperation,
	ActionPluginEnable:             categoryOperation,
	ActionPluginDisable:            categoryOperation,
	ActionPluginMarketSourceUpdate: categoryOperation,
	ActionPluginMarketInstall:      categoryOperation,
	ActionPluginDryRun:             categoryOperation,

	ActionModelCreate:      categoryOperation,
	ActionModelUpdate:      categoryOperation,
	ActionModelDelete:      categoryOperation,
	ActionModelBatchDelete: categoryOperation,
	ActionModelSync:        categoryOperation,

	ActionVendorCreate:     categoryOperation,
	ActionVendorUpdate:     categoryOperation,
	ActionVendorDelete:     categoryOperation,
	ActionVendorOperations: categoryOperation,

	ActionSystemDiskCacheClear: categoryOperation,
	ActionSystemGCRun:          categoryOperation,
	ActionSystemLogCleanupRun:  categoryOperation,

	ActionSubscriptionPlanCreate:     categoryOperation,
	ActionSubscriptionPlanUpdate:     categoryOperation,
	ActionSubscriptionPlanDelete:     categoryOperation,
	ActionSubscriptionPlanEnable:     categoryOperation,
	ActionSubscriptionPlanDisable:    categoryOperation,
	ActionSubscriptionPlanResetQuota: categoryOperation,
	ActionBillingPreferenceUpdate:    categoryOperation,
	ActionSubscriptionCancel:         categoryOperation,

	ActionAnnouncementCreate:  categoryOperation,
	ActionAnnouncementUpdate:  categoryOperation,
	ActionAnnouncementPublish: categoryOperation,
	ActionAnnouncementOffline: categoryOperation,
	ActionAnnouncementDelete:  categoryOperation,
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

// Configured 报告 Logger 是否已绑定写库。装配层据此 fail-fast：一个非 nil
// 但未绑定 exec 的 Logger 会让每一次审计都返回 ErrNotConfigured，等价于
// 静默无审计，必须在启动期拒绝而不是上线后才发现。
func (l *Logger) Configured() bool { return l != nil && l.exec != nil }

// insertSQL 是唯一写路径：append-only（modules-05 §7、platform-01 §5）。
// jsonb 参数显式 ::jsonb 转型，避免驱动把 []byte 当成 bytea。
const insertSQL = `INSERT INTO audit_logs
	(user_id, username, category, ip, user_agent, method, route, status_code, result, detail, before, after)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb)`

// Log 写入一条简单审计事件（冻结签名）。detail 为空时不会写 NULL：writer
// 统一注入 {"action": action}（裁决 R-43），保证每条审计都可按动作检索。
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
	// Actor<=0 是未认证事件（登录失败等）。此时解析器必然查不到用户，却会为
	// 每一条匿名审计行打一次主库查询——把失败/匿名流量放大成 DB 负载。
	// 直接走 user#<id> 兜底，不触发 UsernameResolver。
	if username == "" && e.Actor > 0 && l.UsernameResolver != nil {
		if resolved, err := l.UsernameResolver(ctx, e.Actor); err == nil && strings.TrimSpace(resolved) != "" {
			username = strings.TrimSpace(resolved)
		}
	}
	if username == "" {
		// 快照缺失时也必须满足 NOT NULL；user#<id> 明确表示「未解析」而不是伪造用户名。
		username = fmt.Sprintf("user#%d", e.Actor)
	}

	detail, err := sanitizeDetailJSON(e.Detail, e.Action)
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

	// UA 无 Envoy/代理侧上限时可能长达数 KB，而审计保留 12 个月；在入库前
	// 截断到 512 字节，既够区分客户端，又不让日志库承担无限输入。
	userAgent := truncateUTF8(e.UserAgent, maxUserAgentBytes)

	args := []any{
		e.Actor,
		username,
		category,
		nullableText(e.IP),
		nullableText(userAgent),
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

// maxUserAgentBytes 是 audit_logs.user_agent 的写入侧上限：UA 是客户端完全
// 可控的字符串，不设上限会让 12 个月保留期的日志库承受无限输入。
const maxUserAgentBytes = 512

// truncateUTF8 按 UTF-8 边界把 s 截断到至多 max 字节。直接切 s[:max] 可能
// 把多字节字符切成半个 rune，产生非法 UTF-8（PostgreSQL text 会拒绝或留下
// 乱码）；这里回退到最后一个 rune 的起始字节。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// injectDetailAction 落实裁决 R-43：detail.action 恒等于冻结动作枚举。
//   - Detail 为空（含 JSON null）→ 生成仅含 action 的对象；
//   - Detail 是 JSON 对象 → 覆盖 action，保留其余键；
//   - 其他 JSON 形态 → 原样保留（现有契约允许任意 JSON，不做破坏性变更）。
func injectDetailAction(detail any, action string) any {
	if detail == nil {
		return map[string]any{"action": action}
	}
	if obj, ok := detail.(map[string]any); ok {
		obj["action"] = action
	}
	return detail
}

// sanitizeDetailJSON 是 detail 的专用入口：在通用的校验+脱敏之后，由 writer
// 统一注入 detail.action（裁决 R-43：调用侧不得自行定义动作名，避免同一个
// 事件出现两个动作名真源）。注入发生在解码后的对象上、重新编码之前，因此
// 空详情会得到 {action} 对象，而非 NULL。
func sanitizeDetailJSON(raw json.RawMessage, action string) (any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		out, err := json.Marshal(map[string]any{"action": action})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
		}
		return string(out), nil
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	value = injectDetailAction(redactValue(value), action)
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	return string(out), nil
}

// secretKeyNormalized 是脱敏键的归一化集合（小写、去掉 _ 与 -）。
// 「key」「token」等通用词也在清单内：审计宁可少记，也不能漏记。
//
// 复合凭据键（access_token/refresh_token/id_token/bearer_token/token_hash 等）
// 归一化后是独立的组合词，不会被「token」命中，必须逐项登记——审计保留 12
// 个月，一次 OAuth 凭据误入 detail 就是长期泄露。`backupcodes` 继续保留：
// 既有的 backup_codes 详情必须强制脱敏；计数类调用侧改用
// `backup_codes_count`（归一化后不在集合内，自然保留数值，modules-05 §7.4）。
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
	"accesstoken":     {},
	"refreshtoken":    {},
	"idtoken":         {},
	"bearertoken":     {},
	"tokenhash":       {},
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
	// 键名来自调用侧手工构造的 map，可能带空白（如 " access_token "）；
	// 先裁剪再归一化，避免空白让敏感键绕过匹配原样落库。
	key = strings.TrimSpace(key)
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
