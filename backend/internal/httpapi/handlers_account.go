// Package httpapi 的账号生命周期端点（Task 10）。
//
// 端点面（modules-01 F-01-15 / F-01-17 / F-01-22，security-01 §2.6 全量再认证门）：
//
//	DELETE /api/account                     {confirm_username,password,totp_code?}
//	                                        → 硬删除 users/api_tokens/认证族附属行 +
//	                                          吊销会话 + 保留面 PII 匿名化；403 reauth_required
//	PUT    /api/email                       {new_email,code,current_password,totp_code?}
//	GET    /api/notification-preferences
//	PUT    /api/notification-preferences    稀疏 JSON 合并
//	POST   /api/notification-preferences/test  {channel,target?}
//
// 路由挂载（并行任务负责 routes.go）。装配方在 NewRouter 之后、开始服务之前：
//
//	db, _ := sql.Open("pgx", cfg.DatabaseURL)
//	logs, _ := sql.Open("pgx", cfg.DBLogsDSN)
//	h, err := NewAccountHandlers(AccountHandlerConfig{
//	    DB: db, LogDB: logs, Redis: cfg.Redis, Sessions: cfg.Sessions,
//	    Audit: audit.New(logs), EncryptionKey: cfg.CredentialEncryptionKey,
//	    SessionSecret: []byte(cfg.SessionSecret), NotificationSender: sender,
//	})
//	// Router.protected 已挂好 SessionAuth → CSRF → OriginRefererCheck：
//	h.RegisterAccountRoutes(rt.protected, nil)
//
// 也可以传裸 mux + wrap（如 Protect(...)）把五个端点包进同一条鉴权链。
//
// 安全口径：
//   - 删除与换绑邮箱都走 Task 8 的 ReauthGate（当前密码 + 已启用 MFA 时的 TOTP）。
//     TOTP secret 由 SQLAccountReauthSource 用 CREDENTIAL_ENCRYPTION_KEY 解密后
//     只交给校验器，绝不落日志/审计/响应。
//   - 删除是硬删除（GC-4 / R-04，无 30 天宽限期）：users 行与 api_tokens 行物理
//     消失，同名可重新注册；wallet_transactions / orders / usage_logs 保留以维持
//     对账，audit_logs.username 改写为 deleted_{id}（users.email 随行一起消失）。
//   - 审计只记事件与结果，绝不记录口令、TOTP、验证码或新邮箱明文（PII 最小化：
//     审计保留 12 个月，地址本身不是安全事件所需的追溯信息）。
//   - 任何基础设施故障（Redis/DB/匿名化失败）都 fail-closed（503），绝不在
//     「会话未吊销」或「PII 未匿名化」的半状态下返回成功。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 通知测试消息的限流（modules-01 F-01-22：3 次/分钟）。
const (
	notificationTestLimit     = 3
	notificationTestWindow    = time.Minute
	notificationTestKeyPrefix = "notif_test:user:"
	maxNotificationTargetLen  = 2048
)

// problemTypeNotificationTestFailed 是测试消息投递失败的 RFC 7807 type。
// 与 writeProblem 共用全库 https://newtg.dev/problems/<slug> 约定。
const problemTypeNotificationTestFailed = "https://newtg.dev/problems/notification_test_failed"

// 账号存储错误哨兵。单测注入内存假实现，生产由 SQLAccountRepo 返回。
var (
	// ErrAccountNotFound 表示 users 行不存在（并发删除/残留会话）。
	ErrAccountNotFound = errors.New("httpapi: account not found")
	// ErrEmailTaken 表示目标邮箱已被其他账号占用（users_email_uq 23505）。
	ErrEmailTaken = errors.New("httpapi: email already in use")
	// errCorruptNotificationPreferences 表示库内 jsonb 不是对象：属数据完整性
	// 事件（500），不能当成用户输入错误回 400。
	errCorruptNotificationPreferences = errors.New("httpapi: corrupt notification preferences")
)

// AccountRecord 是账号生命周期端点需要的 users 投影（零敏感字段）。
type AccountRecord struct {
	ID       int64
	Username string
	Email    *string
	Role     string
	Status   string
}

// AccountRepo 是账号端点对 users/api_tokens/认证族附属表的最小读写面。
// 单测注入内存假实现；生产实现是 SQLAccountRepo。
type AccountRepo interface {
	// GetAccount 按主键读取账号投影；不存在返回 (nil, nil)。
	GetAccount(ctx context.Context, userID int64) (*AccountRecord, error)
	// ChangeEmail 写入新邮箱并置 email_verified_at（验证码已证明新地址归属）。
	// 行不存在返回 ErrAccountNotFound；唯一冲突返回 ErrEmailTaken。
	ChangeEmail(ctx context.Context, userID int64, newEmail string) (time.Time, error)
	// DeleteAccount 在同一事务内硬删除 users 行、api_tokens 与认证族附属行，
	// 返回被删除的令牌 id（供缓存失效广播）。users 行已不存在返回 ErrAccountNotFound。
	DeleteAccount(ctx context.Context, userID int64) ([]int64, error)
	// NotificationPreferences 返回 users.notification_preferences 原始 jsonb；
	// NULL/空值归一为 "{}"。行不存在返回 ErrAccountNotFound。
	NotificationPreferences(ctx context.Context, userID int64) (json.RawMessage, error)
	// UpdateNotificationPreferences 写入合并后的完整 JSON 对象。
	UpdateNotificationPreferences(ctx context.Context, userID int64, prefs json.RawMessage) error
}

// PIIAnonymizer 对保留面（日志库 audit_logs）做删除后的 PII 匿名化。
// 主库里保留的钱包流水/订单没有 username/email 列，无需改写；audit_logs.username
// 是唯一留存的操作者名快照（platform-01 §5）。
type PIIAnonymizer interface {
	AnonymizeUser(ctx context.Context, userID int64, anonymizedUsername string) error
}

// NotificationTestSender 是「发送测试消息」的专用接缝（真实渠道实现属模块 08）。
// nil 表示未配置：测试端点返回 503，偏好读写不受影响。
type NotificationTestSender interface {
	SendTestNotification(ctx context.Context, channel, target string) error
}

// ---------------------------------------------------------------------------
// PostgreSQL 实现
// ---------------------------------------------------------------------------

// SQLAccountRepo 是 AccountRepo 的 PostgreSQL 实现（database/sql + pgx stdlib）。
type SQLAccountRepo struct{ db *sql.DB }

// NewSQLAccountRepo 构造账号仓储。
func NewSQLAccountRepo(db *sql.DB) *SQLAccountRepo { return &SQLAccountRepo{db: db} }

// GetAccount 读取账号投影。
func (r *SQLAccountRepo) GetAccount(ctx context.Context, userID int64) (*AccountRecord, error) {
	var (
		rec   AccountRecord
		email sql.NullString
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, email, role, status FROM users WHERE id = $1`, userID).
		Scan(&rec.ID, &rec.Username, &email, &rec.Role, &rec.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: get account %d: %w", userID, err)
	}
	if email.Valid {
		rec.Email = &email.String
	}
	return &rec, nil
}

// ChangeEmail 更新邮箱并置 email_verified_at。验证码已经由端点消费，因此新
// 地址已经过验证；时间使用数据库 now()，避免应用与数据库时钟漂移。
func (r *SQLAccountRepo) ChangeEmail(ctx context.Context, userID int64, newEmail string) (time.Time, error) {
	var verifiedAt time.Time
	err := r.db.QueryRowContext(ctx, `
		UPDATE users
		SET email = $2, email_verified_at = now(), updated_at = now()
		WHERE id = $1
		RETURNING email_verified_at`, userID, newEmail).Scan(&verifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrAccountNotFound
	}
	if err != nil {
		// 唯一索引 users_email_uq 是并发下的最终裁判：预检查只能给出友好提示。
		if isUniqueViolation(err) {
			return time.Time{}, ErrEmailTaken
		}
		return time.Time{}, fmt.Errorf("httpapi: change email for user %d: %w", userID, err)
	}
	return verifiedAt, nil
}

// DeleteAccount 在一个事务内完成硬删除。
//
// 先删 api_tokens 并收集 id（缓存失效需要），再清理 auth 族附属行（login_sessions
// 注释明确「硬删账号时清理」；OAuth 绑定/Passkey/签到是用户数据），最后删 users
// 行并以 RETURNING 区分「并发已删除」（404）与成功。任何一步失败整体回滚。
// wallet_transactions / orders 保留以维持对账；它们没有 username/email 列。
func (r *SQLAccountRepo) DeleteAccount(ctx context.Context, userID int64) ([]int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("httpapi: begin delete account tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`DELETE FROM api_tokens WHERE user_id = $1 RETURNING id`, userID)
	if err != nil {
		return nil, fmt.Errorf("httpapi: delete api tokens for user %d: %w", userID, err)
	}
	var tokenIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("httpapi: scan deleted token id: %w", err)
		}
		tokenIDs = append(tokenIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("httpapi: iterate deleted token ids: %w", err)
	}
	rows.Close()

	// 【R-W10-09 A】与管理员删除同一口径：不触碰 audit_logs。审计行保留
	// 12 个月、ip 90 天后由保留任务置 NULL、username 快照保留；删除账号
	// 不做即时擦除，避免改写 append-only 的合规追溯链。
	for _, stmt := range []string{
		`DELETE FROM login_sessions WHERE user_id = $1`,
		`DELETE FROM user_oauth_bindings WHERE user_id = $1`,
		`DELETE FROM user_passkeys WHERE user_id = $1`,
		`DELETE FROM checkins WHERE user_id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, userID); err != nil {
			return nil, fmt.Errorf("httpapi: delete account dependents: %w", err)
		}
	}

	var username string
	err = tx.QueryRowContext(ctx,
		`DELETE FROM users WHERE id = $1 RETURNING username`, userID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: delete user %d: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("httpapi: commit delete account: %w", err)
	}
	return tokenIDs, nil
}

// NotificationPreferences 读取原始 jsonb。SQL NULL 归一为 "{}"，让客户端无需
// 区分「从未保存」与「空偏好」。
func (r *SQLAccountRepo) NotificationPreferences(ctx context.Context, userID int64) (json.RawMessage, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT notification_preferences FROM users WHERE id = $1`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: get notification preferences for user %d: %w", userID, err)
	}
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	return append(json.RawMessage(nil), raw...), nil
}

// UpdateNotificationPreferences 整体写入合并后的对象（仍是合法的 sparse JSON）。
func (r *SQLAccountRepo) UpdateNotificationPreferences(ctx context.Context, userID int64, prefs json.RawMessage) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET notification_preferences = $2::jsonb, updated_at = now()
		WHERE id = $1`, userID, []byte(prefs))
	if err != nil {
		return fmt.Errorf("httpapi: update notification preferences for user %d: %w", userID, err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrAccountNotFound
	}
	return nil
}

// SQLPIIAnonymizer 对日志库 audit_logs 做删除后的匿名化。
//
// 只改 username：审计行的 user_id/时间/ip 是合规追溯所需的 who/when/where，
// 用户名快照替换为 deleted_{id} 后仍可对账。users.email 随 users 行物理删除，
// 保留面没有 email 列（wallet_transactions/orders/platform-01 §11/§10）。
type SQLPIIAnonymizer struct{ db *sql.DB }

// NewSQLPIIAnonymizer 构造匿名化器。db 必须是日志库连接（audit_logs 所在库）。
func NewSQLPIIAnonymizer(db *sql.DB) *SQLPIIAnonymizer { return &SQLPIIAnonymizer{db: db} }

// AnonymizeUser 把该用户全部历史审计行的 username 快照改写为 deleted_{id}。
func (a *SQLPIIAnonymizer) AnonymizeUser(ctx context.Context, userID int64, anonymizedUsername string) error {
	if a == nil || a.db == nil {
		return errors.New("httpapi: PII anonymizer is not configured")
	}
	if _, err := a.db.ExecContext(ctx,
		`UPDATE audit_logs SET username = $2 WHERE user_id = $1`, userID, anonymizedUsername); err != nil {
		return fmt.Errorf("httpapi: anonymize audit rows for user %d: %w", userID, err)
	}
	return nil
}

// SQLAccountReauthSource 实现 auth.ReauthUserSource：读取口令哈希，并在 MFA
// 启用时用 CREDENTIAL_ENCRYPTION_KEY 解密 TOTP 种子后交给 ReauthGate。
// 解密失败返回 error（fail-closed），绝不把损坏密文当作空 secret 放行。
type SQLAccountReauthSource struct {
	db     *sql.DB
	cipher *auth.SecretCipher
}

// NewSQLAccountReauthSource 构造再认证材料源。
func NewSQLAccountReauthSource(db *sql.DB, encryptionKey string) (*SQLAccountReauthSource, error) {
	if db == nil {
		return nil, errors.New("httpapi: reauth source requires a database connection")
	}
	cipher, err := auth.NewSecretCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	return &SQLAccountReauthSource{db: db, cipher: cipher}, nil
}

// ReauthMaterial 实现 auth.ReauthUserSource。用户不存在返回 (nil, nil)，
// 与 ReauthGate 的契约一致（随后映射为 403/404，而不是基础设施故障）。
func (s *SQLAccountReauthSource) ReauthMaterial(ctx context.Context, userID int64) (*auth.ReauthMaterial, error) {
	var (
		passwordHash string
		totpEnabled  bool
		totpSecret   string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(password_hash, ''), totp_enabled, COALESCE(totp_secret, '')
		FROM users WHERE id = $1`, userID).Scan(&passwordHash, &totpEnabled, &totpSecret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: load reauth material for user %d: %w", userID, err)
	}
	material := &auth.ReauthMaterial{PasswordHash: passwordHash, TOTPEnabled: totpEnabled}
	if totpEnabled && totpSecret != "" {
		plain, err := s.cipher.Decrypt(totpSecret)
		if err != nil {
			// 密文损坏属数据完整性事件：fail-closed，绝不退化为「无 MFA」。
			return nil, fmt.Errorf("httpapi: decrypt totp secret for user %d: %w", userID, err)
		}
		material.TOTPSecret = plain
	}
	return material, nil
}

// ---------------------------------------------------------------------------
// Handler 装配
// ---------------------------------------------------------------------------

// AccountHandlerConfig 是账号生命周期端点的装配输入。
//
// Repo 与 DB 二选一（Repo 优先，供单测注入）；Redis（再认证窗口/邮箱验证码/
// 测试消息限流）与 Sessions（删除时吊销全部会话）必填。
//
// 再认证门：Reauth 可直接注入（测试/复用既有实例）；否则由 ReauthUsers 或
// 「DB + EncryptionKey」构造 SQLAccountReauthSource，并注入 auth.VerifyTOTP。
// PII 匿名化：Anonymizer 与 LogDB 二选一；生产必须提供 LogDB（审计表在日志库），
// 单测注入假实现。二者都缺失属于装配错误——不能在删除成功后静默跳过匿名化。
type AccountHandlerConfig struct {
	DB   *sql.DB
	Repo AccountRepo

	// LogDB 是日志库连接（audit_logs 所在库），用于删除后的 PII 匿名化。
	LogDB *sql.DB

	Redis    *redis.Client
	Sessions *auth.SessionManager

	// Audit 为 nil 时不写审计（仅测试；生产必须接线）。
	Audit *audit.Logger
	// Anonymizer 覆盖默认的 SQLPIIAnonymizer（测试接缝）。
	Anonymizer PIIAnonymizer

	// Config 是 configcenter.Center 的读取面；换绑邮箱复用注册路径的域名
	// 白名单（auth.email_domain_restrict_enabled / auth.email_domain_allowlist，
	// docs/system-config-mapping.md:169）。nil 时按默认值放行。
	Config ConfigReader

	// EncryptionKey 即 config.CredentialEncryptionKey（CREDENTIAL_ENCRYPTION_KEY）。
	EncryptionKey string
	// SessionSecret 用于邮箱验证码 HMAC（config.SessionSecret）。
	SessionSecret []byte

	// Reauth 为 nil 时由以下两项构造。VerifyTOTP 为 nil 时使用 auth.VerifyTOTP。
	Reauth      *auth.ReauthGate
	ReauthUsers auth.ReauthUserSource
	VerifyTOTP  auth.TOTPVerifier

	// NotificationSender 是测试消息通道；nil 时该端点 503（M1 未配置渠道）。
	NotificationSender NotificationTestSender

	Logger *slog.Logger
}

// AccountHandlers 持有账号生命周期端点依赖，可并发使用。
type AccountHandlers struct {
	repo       AccountRepo
	rdb        *redis.Client
	sessions   *auth.SessionManager
	reauth     *auth.ReauthGate
	anonymizer PIIAnonymizer
	audit      *audit.Logger
	codes      *emailCodeStore
	sender     NotificationTestSender
	config     ConfigReader
	log        *slog.Logger
}

// NewAccountHandlers 构造账号生命周期处理器。缺少必需依赖时返回 error，
// 让进程启动失败而不是带病提供服务。
func NewAccountHandlers(cfg AccountHandlerConfig) (*AccountHandlers, error) {
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: AccountHandlerConfig.Redis is required (reauth window, email codes, test rate limit)")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("httpapi: AccountHandlerConfig.Sessions is required (account deletion revokes sessions)")
	}
	// SESSION_SECRET 必须显式提供：newEmailCodeStore 在空密钥时会退化为
	// 进程内随机 HMAC，跨副本/重启后验证码全部失效——这是装配错误而不是
	// 可用降级，必须在启动期拒绝。
	if len(cfg.SessionSecret) == 0 {
		return nil, errors.New("httpapi: AccountHandlerConfig.SessionSecret is required (email code HMAC)")
	}
	// Config 为 nil 会让换绑邮箱的域名白名单静默失效（fail-open）。白名单是
	// 安全配置，缺读取面属于装配错误。
	if cfg.Config == nil {
		return nil, errors.New("httpapi: AccountHandlerConfig.Config is required (email domain allowlist fail-closed)")
	}
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: AccountHandlerConfig.Repo or DB is required")
		}
		repo = NewSQLAccountRepo(cfg.DB)
	}

	gate := cfg.Reauth
	if gate == nil {
		source := cfg.ReauthUsers
		if source == nil {
			if cfg.DB == nil || strings.TrimSpace(cfg.EncryptionKey) == "" {
				return nil, errors.New("httpapi: account reauth requires Reauth, ReauthUsers, or DB+EncryptionKey")
			}
			src, err := NewSQLAccountReauthSource(cfg.DB, cfg.EncryptionKey)
			if err != nil {
				return nil, err
			}
			source = src
		}
		verify := cfg.VerifyTOTP
		if verify == nil {
			verify = auth.VerifyTOTP
		}
		gate = auth.NewReauthGate(cfg.Redis, source, verify)
	}

	anonymizer := cfg.Anonymizer
	if anonymizer == nil {
		if cfg.LogDB == nil {
			return nil, errors.New("httpapi: account deletion requires Anonymizer or LogDB for PII anonymization")
		}
		anonymizer = NewSQLPIIAnonymizer(cfg.LogDB)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &AccountHandlers{
		repo:       repo,
		rdb:        cfg.Redis,
		sessions:   cfg.Sessions,
		reauth:     gate,
		anonymizer: anonymizer,
		audit:      cfg.Audit,
		codes:      newEmailCodeStore(cfg.Redis, cfg.SessionSecret),
		sender:     cfg.NotificationSender,
		config:     cfg.Config,
		log:        log,
	}, nil
}

// RegisterAccountRoutes 注册 Task 10 的全部端点。
//
// wrap 为 nil 时假定 mux 已经处于 SessionAuth（及 CSRF/Origin）之后；非 nil 时
// 每个端点都会被 wrap 包裹（推荐传 Protect(...) 或至少 SessionAuth）。所有
// handler 都假定 request.Context 里已有 auth.Principal，缺失时一律 401
// （fail-closed，绝不把零值身份当匿名放行）。
func (h *AccountHandlers) RegisterAccountRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	mount("DELETE /api/account", http.HandlerFunc(h.handleDeleteAccount))
	mount("PUT /api/email", http.HandlerFunc(h.handleChangeEmail))
	mount("GET /api/notification-preferences", http.HandlerFunc(h.handleGetNotificationPreferences))
	mount("PUT /api/notification-preferences", http.HandlerFunc(h.handlePutNotificationPreferences))
	mount("POST /api/notification-preferences/test", http.HandlerFunc(h.handleTestNotification))
}

// ---------------------------------------------------------------------------
// DELETE /api/account
// ---------------------------------------------------------------------------

type accountDeleteRequest struct {
	ConfirmUsername string `json:"confirm_username"`
	Password        string `json:"password"`
	TOTPCode        string `json:"totp_code"`
}

// handleDeleteAccount 实现 F-01-17：全量再认证 → 吊销全部会话 → PII 匿名化 →
// 硬删除（users/api_tokens/认证族附属行）→ 失效令牌缓存 → 审计。
//
// 顺序理由：跨 Redis/主库/日志库没有分布式事务，任何一步失败都不会返回成功，
// 已完成的步骤按 fail-closed 方向保留（例如会话已吊销但匿名化失败时账号仍在，
// 用户可重新登录后重试）；但绝不会出现「账号已消失、历史审计仍带真名」的终态，
// 因为匿名化先于 users 行删除。
func (h *AccountHandlers) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req accountDeleteRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	confirmUsername := strings.TrimSpace(req.ConfirmUsername)
	if confirmUsername == "" {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "confirm_username is required")
		return
	}
	// 空口令不能落进 ReauthGate 的「沿用 5 分钟窗口」分支：删除账号必须现场
	// 提供当前口令（design F-01-17 的字段契约），否则窗口复用会削弱确认强度。
	if req.Password == "" {
		writeReauthRequired(w)
		return
	}

	ctx := r.Context()
	rec, err := h.repo.GetAccount(ctx, p.UserID)
	if err != nil {
		h.writeInternal(w, "load account for deletion", err)
		return
	}
	if rec == nil {
		// 幂等终态：行已被并发删除（或会话残留）。401 由中间件在会话失效时返回，
		// 走到这里说明 Principal 仍有效但 users 行已不存在。
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if rec.Role == rbac.RoleAdmin {
		// F-01-17 权限假设：管理员必须先由模块 07 降权，防止误删管理入口。
		writeProblem(w, http.StatusForbidden, authProblemForbidden,
			"Administrator accounts cannot be deleted here; ask another administrator to demote the account first")
		return
	}
	if confirmUsername != rec.Username {
		// 用户名不匹配是输入错误，不是再认证失败；不产生任何副作用。
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Username does not match")
		return
	}
	if err := h.reauth.Authorize(ctx, p.UserID, auth.ReauthFactor{
		Password: req.Password,
		TOTPCode: req.TOTPCode,
	}); err != nil {
		h.writeReauthError(w, "account deletion", err)
		return
	}

	if err := h.sessions.RevokeUser(ctx, p.UserID, ""); err != nil {
		h.writeUnavailable(w, "Revoke sessions before account deletion", err)
		return
	}
	anonymized := fmt.Sprintf("deleted_%d", p.UserID)
	if err := h.anonymizer.AnonymizeUser(ctx, p.UserID, anonymized); err != nil {
		h.writeUnavailable(w, "Anonymize retained records before account deletion", err)
		return
	}
	tokenIDs, err := h.repo.DeleteAccount(ctx, p.UserID)
	if errors.Is(err, ErrAccountNotFound) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err != nil {
		h.writeInternal(w, "delete account", err)
		return
	}

	h.invalidateTokens(ctx, tokenIDs)
	// 再认证窗口随账号一起消失（best-effort：行已删除，窗口键 5 分钟后自然过期）。
	if err := auth.ClearReauthWindow(ctx, h.rdb, p.UserID); err != nil {
		h.log.Error("clear reauth window after account deletion failed", "user_id", p.UserID, "error", err)
	}
	// 删除事件是唯一保留真实用户名的审计行（who/when/ip 供合规追溯）；历史行
	// 已在上一步改写为 deleted_{id}。
	h.auditEvent(r, audit.ActionUserDelete, p.UserID, rec.Username, map[string]any{
		"label":           "account_deleted",
		"tokens_deleted":  len(tokenIDs),
		"anonymized_name": anonymized,
	}, http.StatusNoContent)
	// 删号成功后浏览器侧的会话 Cookie 已无对应服务端会话，必须显式清除；
	// Clear-Site-Data 同时清空前端存储（TC-I-06 固定三项）。
	setClearSiteData(w)
	clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// PUT /api/email
// ---------------------------------------------------------------------------

type emailChangeRequest struct {
	NewEmail        string `json:"new_email"`
	Code            string `json:"code"`
	CurrentPassword string `json:"current_password"`
	TOTPCode        string `json:"totp_code"`
}

type emailChangeResponse struct {
	Email           string    `json:"email"`
	EmailVerified   bool      `json:"email_verified"`
	EmailVerifiedAt time.Time `json:"email_verified_at"`
}

// handleChangeEmail 实现 F-01-15 换绑邮箱：先全量再认证，再消费新邮箱验证码，
// 最后落库。未过再认证时不消费验证码、不写库（403 reauth_required）。
func (h *AccountHandlers) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req emailChangeRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	newEmail := normalizeEmail(req.NewEmail)
	if !validEmail(newEmail) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address")
		return
	}
	// 换绑与注册共用同一份域名白名单（system-config-mapping.md:169：绑定邮箱
	// 时也要校验）；未启用时放行。422 与注册路径的 TYPE 逐字一致。
	if !accountEmailDomainAllowed(h.config, newEmail) {
		writeProblem(w, http.StatusUnprocessableEntity, authProblemEmailDomainDenied,
			"Email domain is not allowed")
		return
	}
	if !sixDigitPattern.MatchString(strings.TrimSpace(req.Code)) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
		return
	}
	if req.CurrentPassword == "" {
		// 与删除一致：不允许空口令复用 5 分钟窗口。
		writeReauthRequired(w)
		return
	}

	ctx := r.Context()
	rec, err := h.repo.GetAccount(ctx, p.UserID)
	if err != nil {
		h.writeInternal(w, "load account for email change", err)
		return
	}
	if rec == nil {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err := h.reauth.Authorize(ctx, p.UserID, auth.ReauthFactor{
		Password: req.CurrentPassword,
		TOTPCode: req.TOTPCode,
	}); err != nil {
		h.writeReauthError(w, "email change", err)
		return
	}

	// 验证码在再认证通过之后才消费：失败路径不得烧掉用户刚收到的码。
	// purpose=bind 与 POST /api/auth/email-code 的绑定语义一致。
	okCode, err := h.codes.consume(ctx, newEmail, "bind", strings.TrimSpace(req.Code))
	if err != nil {
		h.writeUnavailable(w, "Verify new email address", err)
		return
	}
	if !okCode {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
		return
	}

	// 换绑邮箱是 security-01 §2.1 会话生命周期状态机的失效触发（与改密/2FA
	// 变更同档）：被盗会话不能在受害者换绑（本意是切断找回通道）后继续有效。
	// 顺序与改密一致——先吊销、失败即 503，再落库；保留当前会话（凭据仍在
	// 用户手里）。Bearer 路径没有 Cookie，keepSID 为空，吊销全部会话。
	keepSID, _ := sessionID(r)
	if err := h.sessions.RevokeUser(ctx, p.UserID, keepSID); err != nil {
		h.writeUnavailable(w, "Revoke other sessions before email change", err)
		return
	}

	verifiedAt, err := h.repo.ChangeEmail(ctx, p.UserID, newEmail)
	switch {
	case errors.Is(err, ErrAccountNotFound):
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	case errors.Is(err, ErrEmailTaken):
		// 409 与注册的重复邮箱同口径；不泄露占用者的任何信息。
		writeProblem(w, http.StatusConflict, authProblemConflict, "Email is already in use")
		return
	case err != nil:
		h.writeInternal(w, "change email", err)
		return
	}

	h.auditEvent(r, audit.ActionUserUpdate, p.UserID, rec.Username, map[string]any{
		// 人读标签写 detail.label；detail.action 由审计 writer 统一注入为冻结
		// 动作名（modules-05 §7 / 裁决 R-43）。
		"label":  "email_changed",
		"field":  "email",
		"result": "success",
		"reauth": "passed",
		// 不记录 new_email：审计保留 12 个月，地址属于 PII，安全事件不需要它。
	}, http.StatusOK)
	writeJSON(w, http.StatusOK, emailChangeResponse{
		Email:           newEmail,
		EmailVerified:   true,
		EmailVerifiedAt: verifiedAt,
	})
}

// ---------------------------------------------------------------------------
// 通知偏好
// ---------------------------------------------------------------------------

// 注意：security_alert 的**实际投递**（邮件/Webhook 等）依赖模块 08 的通知
// 通道，M1 未实现，登记为延后项；本任务只保证偏好读写、测试通道接缝，以及
// 「安全通知不可关闭」的服务端约束。

// handleGetNotificationPreferences 返回存储的稀疏偏好对象；NULL 归一为 {}。
// 站点级默认值（security_alert 默认开等）由客户端合并，服务端只持久化用户
// 显式设置过的键。
func (h *AccountHandlers) handleGetNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	prefs, err := h.repo.NotificationPreferences(r.Context(), p.UserID)
	if errors.Is(err, ErrAccountNotFound) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err != nil {
		h.writeInternal(w, "get notification preferences", err)
		return
	}
	if len(prefs) == 0 || strings.TrimSpace(string(prefs)) == "null" {
		prefs = json.RawMessage("{}")
	}
	writeJSON(w, http.StatusOK, prefs)
}

// handlePutNotificationPreferences 对偏好做稀疏 JSON 合并：请求中出现的键
// 覆盖/合并已有值，未出现的键保持不动。未知键与非法值一律 400（静默丢弃会
// 让前端拼写错误永远不生效）。
func (h *AccountHandlers) handlePutNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	ctx := r.Context()
	existing, err := h.repo.NotificationPreferences(ctx, p.UserID)
	if errors.Is(err, ErrAccountNotFound) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err != nil {
		h.writeInternal(w, "get notification preferences", err)
		return
	}
	merged, err := mergeNotificationPreferences(existing, body)
	if err != nil {
		if errors.Is(err, errCorruptNotificationPreferences) {
			h.writeInternal(w, "parse stored notification preferences", err)
			return
		}
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, notificationErrorTitle(err))
		return
	}
	if err := h.repo.UpdateNotificationPreferences(ctx, p.UserID, merged); err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
			return
		}
		h.writeInternal(w, "update notification preferences", err)
		return
	}
	// 审计行保留用户名快照；查不到行时 audit.Logger 回退为 user#<id>。
	username := ""
	if rec, err := h.repo.GetAccount(ctx, p.UserID); err == nil && rec != nil {
		username = rec.Username
	}
	h.auditEvent(r, audit.ActionUserUpdate, p.UserID, username, map[string]any{
		"label": "notification_pref_updated",
	}, http.StatusOK)
	writeJSON(w, http.StatusOK, merged)
}

type notificationTestRequest struct {
	Channel string `json:"channel"`
	// Target 与 TargetAddress 是同一语义的两个键名：设计文档只写「目标地址」，
	// 前端历史字段为 target；两者都接受，Target 优先。
	Target        string `json:"target"`
	TargetAddress string `json:"target_address"`
}

type notificationTestResponse struct {
	Channel string `json:"channel"`
	Sent    bool   `json:"sent"`
}

// handleTestNotification 发送一条测试消息。渠道实现由 NotificationTestSender
// 注入（模块 08 的真实通道）；M1 未配置时返回 503，偏好读写不受影响。
func (h *AccountHandlers) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req notificationTestRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	if !validNotificationChannel(channel) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid notification channel")
		return
	}
	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = strings.TrimSpace(req.TargetAddress)
	}
	if len(target) > maxNotificationTargetLen {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Notification target is too long")
		return
	}
	if channel == "email" {
		if target == "" {
			// 未指定目标时回落到账号邮箱（设计：Email 渠道地址显示账号邮箱）。
			rec, err := h.repo.GetAccount(r.Context(), p.UserID)
			if err != nil {
				h.writeInternal(w, "load account for notification test", err)
				return
			}
			if rec == nil {
				writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
				return
			}
			if rec.Email == nil || strings.TrimSpace(*rec.Email) == "" {
				writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "This account has no email address")
				return
			}
			target = *rec.Email
		}
		// 显式目标同样必须过邮箱格式校验：否则 CR/LF 或多收件人写法会直接
		// 进入 SMTP 实现（头注入/滥发面）。
		target = normalizeEmail(target)
		if !validEmail(target) {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address")
			return
		}
	} else if target == "" || !validNotificationTargetURL(target) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "A valid target URL is required for this channel")
		return
	}

	// 发送器未配置时直接 503，不消耗用户的测试配额。
	// W13-EMAIL：nil 仍是未配置；email 渠道还要按配置中心快照判断 SMTP 是否
	// 可投递（生产注入的适配器可能非 nil 但 host/from_email 为空）。
	if h.sender == nil || (channel == "email" && !emailDeliveryConfigured(r.Context(), h.sender)) {
		h.log.Error("notification test requested but email delivery is not configured",
			"channel", channel, "user_id", p.UserID)
		writeEmailUnavailable(w)
		return
	}

	// 3 次/分钟/用户（F-01-22）；与邮箱验证码共用 INCR+PEXPIRE 脚本。
	// 放在参数校验之后：非法请求不消耗用户的发送配额。
	limitKey := notificationTestKeyPrefix + strconv.FormatInt(p.UserID, 10)
	n, err := emailIPLimitScript.Run(r.Context(), h.rdb, []string{limitKey},
		strconv.Itoa(int(notificationTestWindow.Seconds()))).Int64()
	if err != nil {
		h.writeUnavailable(w, "Send test notification", err)
		return
	}
	if n > notificationTestLimit {
		w.Header().Set("Retry-After", retrySeconds(notificationTestWindow))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests,
			"Too many test notifications")
		return
	}

	if err := h.sender.SendTestNotification(r.Context(), channel, target); err != nil {
		// 渠道错误（如 Webhook 404）返回给本人用于行内提示；不回显内部堆栈。
		h.log.Warn("send test notification failed", "channel", channel, "user_id", p.UserID, "error", err)
		switch {
		case errors.Is(err, ErrEmailUnavailable):
			// SMTP 在检查与发送之间被清空：固定 fail-closed 出口。
			writeEmailUnavailable(w)
		case errors.Is(err, ErrNotificationChannelUnavailable):
			// 本部署只装配 SMTP（email）；其它渠道没有测试出口。
			h.writeUnavailable(w, "Notification channel is not available", err)
		default:
			writeProblem(w, http.StatusBadGateway, problemTypeNotificationTestFailed,
				"Test notification delivery failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, notificationTestResponse{Channel: channel, Sent: true})
}

// ---------------------------------------------------------------------------
// 通知偏好：稀疏合并与校验
// ---------------------------------------------------------------------------

// notificationError 是偏好校验错误；Title 直接作为 400 的 RFC 7807 title
// （字段级提示需要可读，且不含任何服务端内部信息）。
type notificationError struct{ Title string }

func (e *notificationError) Error() string { return e.Title }

func notificationErrorTitle(err error) string {
	var ne *notificationError
	if errors.As(err, &ne) {
		return ne.Title
	}
	return "Invalid notification preferences"
}

// 允许的通知渠道（modules-01 F-01-22 冻结枚举）。
var notificationChannelsAllowed = map[string]struct{}{
	"email": {}, "webhook": {}, "bark": {}, "gotify": {},
}

func validNotificationChannel(channel string) bool {
	_, ok := notificationChannelsAllowed[channel]
	return ok
}

// notificationCategoryKeys 是每个通知类别允许的子键集合。
var notificationCategoryKeys = map[string]map[string]struct{}{
	"balance_warning": {
		"enabled": {}, "threshold": {}, "channels": {}, "custom_email": {},
	},
	"usage_digest": {
		"enabled": {}, "channels": {},
	},
	"security_alert": {
		"enabled": {},
	},
}

func notificationErr(title string) error { return &notificationError{Title: title} }

// mergeNotificationPreferences 把稀疏 patch 合并进 existing，返回规范化 JSON。
//
//   - 未知顶层键/类别子键 → 错误（拼写错误必须可见）；
//   - 类别对象逐子键合并（只提交 threshold 不会清掉 channels）；
//   - security_alert.enabled=false 被拒绝（安全通知不可关闭）；
//   - enabled=true 且显式 channels=[] → 错误；channels 缺省时补 ["email"]
//     （F-01-22 默认值）。
func mergeNotificationPreferences(existing json.RawMessage, patch map[string]json.RawMessage) (json.RawMessage, error) {
	doc := map[string]json.RawMessage{}
	if len(existing) > 0 && string(existing) != "null" {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("%w: %v", errCorruptNotificationPreferences, err)
		}
	}
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}

	for key, raw := range patch {
		if rules, isCategory := notificationCategoryKeys[key]; isCategory {
			mergedCategory, err := mergeNotificationCategory(doc[key], rules, key, raw)
			if err != nil {
				return nil, err
			}
			doc[key] = mergedCategory
			continue
		}
		switch key {
		case "webhook_url", "bark_url", "gotify_url":
			// null 或空串 = 清除该渠道配置。否则用户存过（可能含密钥的）URL 后
			// 在稀疏合并下永远删不掉。
			if notificationClearsValue(raw) {
				delete(doc, key)
				continue
			}
			value, err := notificationString(raw)
			if err != nil || !validNotificationTargetURL(value) {
				return nil, notificationErr("Notification URL must be a valid http(s) URL")
			}
			doc[key] = raw
		case "gotify_token":
			if notificationClearsValue(raw) {
				delete(doc, key)
				continue
			}
			if _, err := notificationString(raw); err != nil {
				return nil, notificationErr("gotify_token must be a string")
			}
			doc[key] = raw
		case "gotify_priority":
			if strings.TrimSpace(string(raw)) == "null" {
				delete(doc, key)
				continue
			}
			if _, err := notificationInt(raw); err != nil {
				return nil, notificationErr("gotify_priority must be an integer")
			}
			doc[key] = raw
		default:
			return nil, notificationErr("Unknown notification preference key")
		}
	}

	if err := validateMergedNotificationPreferences(doc); err != nil {
		return nil, err
	}
	merged, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("httpapi: marshal notification preferences: %w", err)
	}
	return merged, nil
}

// notificationClearsValue 判断标量是否是「清除」意图：JSON null 或空字符串。
// 与 custom_email 的 null 语义一致。
func notificationClearsValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "null" || trimmed == `""`
}

// mergeNotificationCategory 逐子键合并一个类别对象。
func mergeNotificationCategory(existing json.RawMessage, rules map[string]struct{}, name string, patch json.RawMessage) (json.RawMessage, error) {
	if string(patch) == "null" {
		return nil, notificationErr(name + " must be an object")
	}
	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(patch, &incoming); err != nil || incoming == nil {
		return nil, notificationErr(name + " must be an object")
	}
	current := map[string]json.RawMessage{}
	if len(existing) > 0 && string(existing) != "null" {
		if err := json.Unmarshal(existing, &current); err != nil {
			return nil, notificationErr(name + " is corrupt")
		}
	}
	for subkey, raw := range incoming {
		if _, allowed := rules[subkey]; !allowed {
			return nil, notificationErr("Unknown " + name + " preference key")
		}
		switch subkey {
		case "enabled":
			if _, err := notificationBool(raw); err != nil {
				return nil, notificationErr(name + ".enabled must be a boolean")
			}
			current[subkey] = raw
		case "threshold":
			value, err := notificationInt(raw)
			if err != nil || value < 0 {
				return nil, notificationErr(name + ".threshold must be a non-negative integer")
			}
			current[subkey] = raw
		case "channels":
			channels, err := notificationChannels(raw)
			if err != nil {
				return nil, notificationErr(name + ".channels must be a non-empty array of email/webhook/bark/gotify")
			}
			encoded, _ := json.Marshal(channels)
			current[subkey] = encoded
		case "custom_email":
			if string(raw) == "null" {
				delete(current, subkey)
				continue
			}
			value, err := notificationString(raw)
			if err != nil || (value != "" && !validEmail(normalizeEmail(value))) {
				return nil, notificationErr(name + ".custom_email must be a valid email address or null")
			}
			current[subkey] = raw
		}
	}
	return json.Marshal(current)
}

// validateMergedNotificationPreferences 校验合并后的整体语义（跨字段规则）。
func validateMergedNotificationPreferences(doc map[string]json.RawMessage) error {
	for _, name := range []string{"balance_warning", "usage_digest"} {
		raw, present := doc[name]
		if !present || string(raw) == "null" {
			continue
		}
		var category map[string]json.RawMessage
		if err := json.Unmarshal(raw, &category); err != nil {
			return notificationErr(name + " is corrupt")
		}
		enabled := false
		if value, ok := category["enabled"]; ok {
			parsed, err := notificationBool(value)
			if err != nil {
				return notificationErr(name + ".enabled must be a boolean")
			}
			enabled = parsed
		}
		channelsRaw, hasChannels := category["channels"]
		if hasChannels {
			channels, err := notificationChannels(channelsRaw)
			if err != nil {
				return notificationErr(name + ".channels must be a non-empty array of email/webhook/bark/gotify")
			}
			if enabled && len(channels) == 0 {
				return notificationErr(name + ".channels must contain at least one channel when enabled")
			}
		} else if enabled {
			// F-01-22 默认渠道是 Email。
			category["channels"] = json.RawMessage(`["email"]`)
		}
		if _, hasThreshold := category["threshold"]; hasThreshold {
			value, err := notificationInt(category["threshold"])
			if err != nil || value < 0 {
				return notificationErr(name + ".threshold must be a non-negative integer")
			}
		}
		if value, ok := category["custom_email"]; ok && string(value) != "null" {
			email, err := notificationString(value)
			if err != nil || (email != "" && !validEmail(normalizeEmail(email))) {
				return notificationErr(name + ".custom_email must be a valid email address or null")
			}
		}
		encoded, err := json.Marshal(category)
		if err != nil {
			return notificationErr(name + " is corrupt")
		}
		doc[name] = encoded
	}

	if raw, present := doc["security_alert"]; present && string(raw) != "null" {
		var category map[string]json.RawMessage
		if err := json.Unmarshal(raw, &category); err != nil {
			return notificationErr("security_alert is corrupt")
		}
		if value, ok := category["enabled"]; ok {
			enabled, err := notificationBool(value)
			if err != nil {
				return notificationErr("security_alert.enabled must be true")
			}
			if !enabled {
				return notificationErr("security_alert notifications cannot be disabled")
			}
		}
		encoded, err := json.Marshal(category)
		if err != nil {
			return notificationErr("security_alert is corrupt")
		}
		doc["security_alert"] = encoded
	}
	return nil
}

func notificationBool(raw json.RawMessage) (bool, error) {
	if string(raw) == "null" {
		return false, errors.New("null is not a boolean")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, err
	}
	return value, nil
}

func notificationInt(raw json.RawMessage) (int64, error) {
	if string(raw) == "null" {
		return 0, errors.New("null is not an integer")
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func notificationString(raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", errors.New("null is not a string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

// notificationChannels 解码渠道数组，去重并拒绝未知渠道。null/非数组/空数组
// 都是错误（调用方按 enabled 语义决定是否允许空）。
func notificationChannels(raw json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, errors.New("channels must be an array")
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		channel, err := notificationString(entry)
		if err != nil || !validNotificationChannel(channel) {
			return nil, errors.New("invalid channel")
		}
		if _, dup := seen[channel]; dup {
			continue
		}
		seen[channel] = struct{}{}
		out = append(out, channel)
	}
	return out, nil
}

// validNotificationTargetURL 只接受 http(s) 且带 host 的绝对 URL；拒绝 userinfo
// （避免把凭据写进偏好/日志）。
func validNotificationTargetURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// accountEmailDomainAllowed 复用注册路径的域名白名单判定。
//
// emailDomainAllowed 是 *authHandlers 的方法，但它只依赖 config 字段；用一个
// 最小实例转调而不是复制「通配/精确匹配」规则，避免注册与换绑两条路径的
// 域名语义漂移（system-config-mapping.md:169 要求绑定邮箱时同样校验）。
func accountEmailDomainAllowed(config ConfigReader, email string) bool {
	if config == nil {
		return true
	}
	return (&authHandlers{config: config}).emailDomainAllowed(email)
}

// principal 取当前身份。所有端点都假定中间件已注入 Principal；缺失时 401
// （fail-closed，防止路由被误挂到公开 mux 时零值身份被当匿名放行）。
func (h *AccountHandlers) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeUnauthorized(w)
		return nil, false
	}
	return p, true
}

// writeReauthError 复用共享的 writeReauthFailure（403/429/503 口径与改密等
// 其它再认证端点保持一致），只额外记录非预期的基础设施错误。
func (h *AccountHandlers) writeReauthError(w http.ResponseWriter, op string, err error) {
	if !errors.Is(err, auth.ErrReauthRequired) && !errors.Is(err, auth.ErrReauthLocked) {
		h.log.Error("reauthentication unavailable", "operation", op, "error", err)
	}
	writeReauthFailure(w, err)
}

// writeInternal 记录并返回通用 500；绝不把 SQL/内部错误回显给客户端。
func (h *AccountHandlers) writeInternal(w http.ResponseWriter, op string, err error) {
	h.log.Error(op+" failed", "error", err)
	writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
}

// writeUnavailable 用于 Redis/DB 故障：fail-closed + 有界 Retry-After。
// err 只进日志，绝不回显给客户端。
func (h *AccountHandlers) writeUnavailable(w http.ResponseWriter, title string, err error) {
	h.log.Error(title, "error", err)
	w.Header().Set("Retry-After", "5")
	writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable, title)
}

// invalidateTokens 通过 token:changed:<id> 广播即时失效（GC-11）。删除后进程
// 内令牌缓存最多还会存活 60s，这里让 ≤1s 生效；广播失败只记日志（缓存 TTL 兜底）。
func (h *AccountHandlers) invalidateTokens(ctx context.Context, tokenIDs []int64) {
	for _, id := range tokenIDs {
		channel := "token:changed:" + strconv.FormatInt(id, 10)
		if err := h.rdb.Publish(ctx, channel, strconv.FormatInt(id, 10)).Err(); err != nil {
			h.log.Warn("publish token invalidation failed", "token_id", id, "error", err)
		}
	}
}

// auditEvent 写审计（best-effort：日志库独立且无跨库事务，失败只记日志）。
// username 为空时由 audit.Logger 的回退规则处理（user#<id>）；status 是本次
// 请求实际返回的 HTTP 状态码（204/200），供消费方按结果过滤。
func (h *AccountHandlers) auditEvent(r *http.Request, action string, actor int64, username string, detail map[string]any, status int) {
	if h.audit == nil {
		return
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		h.log.Warn("marshal audit detail failed", "action", action, "error", err)
		return
	}
	event := audit.Event{
		Action:     action,
		Actor:      actor,
		Username:   username,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		Detail:     raw,
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("account audit write failed", "action", action, "actor", actor, "error", err)
	}
}
