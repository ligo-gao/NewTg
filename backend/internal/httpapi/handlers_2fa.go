// 2FA 端点：TOTP 设置向导、登录二次验证挑战、备用码核销/重生成。
//
// 端点契约（modules-01 F-01-08/F-01-09/F-01-10）：
//
//	POST /api/2fa/setup                      （会话）生成 secret + otpauth URI，不落库
//	POST /api/2fa/enable                     （会话）{secret,code} 验证通过后一次性落库
//	POST /api/2fa/disable                    （会话）{password} 清空 2FA 状态
//	POST /api/2fa/backup-codes/regenerate    （会话）{password} 旧码立即失效
//	POST /api/auth/2fa/challenge             （ticket）{ticket,code,type} → 正式会话
//
// 安全口径：
//   - TOTP 种子在库内是 AES-256-GCM envelope 密文（auth.SecretCipher），只在
//     校验/启用时于单次操作作用域内解密；除 setup 响应外绝不回传 secret。
//   - 备用码只存 argon2id 哈希；登录核销用条件 UPDATE（jsonb @> + 快照替换），
//     并发提交同一码时数据库保证恰有一个请求成功。
//   - challenge 的失败计数挂在 ticket 上（Redis INCR）：第 5 次失败即删除 ticket，
//     用户必须重新走口令登录（F-01-08 超限态）。
//   - 同一 TOTP 码在同一步进窗口内只能使用一次（F-01-08「重复使用」异常），
//     以 Redis SET NX 记录 user+code 摘要，TTL 覆盖代码有效期。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/base64"
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
)

// 2FA Redis 键与阈值。ticket 键名与 handlers_auth.go 的 createPending2FA 逐字一致。
const (
	twoFATicketKeyPrefix = "pending_2fa:"
	twoFAFailKeyPrefix   = "pending_2fa:fail:"
	twoFATOTPUsedPrefix  = "2fa:totp:used:"

	// twoFAMaxFailures：同一 ticket 连续失败 5 次即作废（F-01-08/09 共享计数）。
	twoFAMaxFailures = 5

	// TOTP 码的复用防护 TTL：覆盖当前步及一次时钟边界抖动；代码本身在下一步
	// 就无法通过 ±0 校验，60s 已足够。
	twoFATOTPReplayTTL = 2 * auth.TOTPStepSeconds * time.Second

	twoFAMaxCodeRunes = 32
)

// TwoFAUser 是 2FA 流程需要的 users 行投影。TOTPSecretEnc 是 envelope 密文的
// base64 文本（DB 列为 text）；备用码是已解出的 argon2id PHC 哈希数组。
type TwoFAUser struct {
	ID                 int64
	Username           string
	Email              string
	Role               string
	Status             string
	PasswordHash       string
	TOTPSecretEnc      string
	TOTPEnabled        bool
	BackupCodes        []string
	EmailVerified      bool
	MustChangePassword bool
}

// TwoFARepo 是 2FA 端点对 users 表的最小写读面。单测注入内存假实现；
// 生产实现是 SQLTwoFARepo。
type TwoFARepo interface {
	// FindUserByID 返回用户行；不存在时 (nil, nil)。
	FindUserByID(ctx context.Context, userID int64) (*TwoFAUser, error)
	// EnableTwoFA 原子写入密文种子、备用码哈希与 totp_enabled=true。
	// applied=false 表示用户已启用（并发/状态漂移），调用方映射为 409。
	EnableTwoFA(ctx context.Context, userID int64, secretEnc string, backupHashes []string) (applied bool, err error)
	// DisableTwoFA 清空 totp_secret/backup_codes 并置 totp_enabled=false。
	DisableTwoFA(ctx context.Context, userID int64) error
	// ReplaceBackupCodes 整体替换备用码数组（旧码立即失效）。
	// applied=false 表示用户当前未启用 2FA。
	ReplaceBackupCodes(ctx context.Context, userID int64, hashes []string) (applied bool, err error)
	// ConsumeBackupCode 原子核销单个备用码：SQL 内直接从数组删除 matchedHash
	// （backup_codes - $3::text）。绝不能把预读快照算出的 remaining 整体覆盖——
	// 两个并发登录使用不同码时，后写会把先写已删除的码写回（丢失更新）。
	// applied=false 表示并发已被消费/码已重生成。
	ConsumeBackupCode(ctx context.Context, userID int64, matchedHash string) (applied bool, err error)
}

// ---------------------------------------------------------------------------
// SQL 实现
// ---------------------------------------------------------------------------

// SQLTwoFARepo 是 TwoFARepo 的 PostgreSQL 实现（database/sql + pgx stdlib）。
type SQLTwoFARepo struct {
	db *sql.DB
}

// NewSQLTwoFARepo 构造 2FA 仓储。
func NewSQLTwoFARepo(db *sql.DB) *SQLTwoFARepo { return &SQLTwoFARepo{db: db} }

// FindUserByID 读取 2FA 状态。backup_codes 允许 SQL NULL（未启用时），
// 统一投影为空切片；JSON 损坏时返回错误（fail-closed，绝不把损坏当成空数组）。
func (r *SQLTwoFARepo) FindUserByID(ctx context.Context, userID int64) (*TwoFAUser, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, username, COALESCE(email, ''), role, status,
		       COALESCE(password_hash, ''), COALESCE(totp_secret, ''),
		       totp_enabled, COALESCE(backup_codes, '[]'::jsonb),
		       email_verified_at IS NOT NULL, must_change_password
		FROM users
		WHERE id = $1`, userID)

	var u TwoFAUser
	var backupRaw []byte
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.Role, &u.Status,
		&u.PasswordHash, &u.TOTPSecretEnc, &u.TOTPEnabled, &backupRaw,
		&u.EmailVerified, &u.MustChangePassword)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: find user %d for 2fa: %w", userID, err)
	}
	if len(backupRaw) > 0 {
		if err := json.Unmarshal(backupRaw, &u.BackupCodes); err != nil {
			return nil, fmt.Errorf("httpapi: corrupt backup_codes for user %d: %w", userID, err)
		}
	}
	return &u, nil
}

// EnableTwoFA 在单条 UPDATE 内完成三项写入（密文种子、备用码、标志位），
// 满足 F-01-10「验证通过后原子写入」：不存在「已启用但无备用码」的中间态。
func (r *SQLTwoFARepo) EnableTwoFA(ctx context.Context, userID int64, secretEnc string, backupHashes []string) (bool, error) {
	payload, err := marshalBackupCodes(backupHashes)
	if err != nil {
		return false, err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET totp_secret = $2, backup_codes = $3::jsonb,
		    totp_enabled = true, updated_at = now()
		WHERE id = $1 AND totp_enabled = false`,
		userID, secretEnc, payload)
	if err != nil {
		return false, fmt.Errorf("httpapi: enable 2fa for user %d: %w", userID, err)
	}
	return rowsApplied(res)
}

func (r *SQLTwoFARepo) DisableTwoFA(ctx context.Context, userID int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET totp_secret = NULL, backup_codes = NULL,
		    totp_enabled = false, updated_at = now()
		WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("httpapi: disable 2fa for user %d: %w", userID, err)
	}
	return nil
}

func (r *SQLTwoFARepo) ReplaceBackupCodes(ctx context.Context, userID int64, hashes []string) (bool, error) {
	payload, err := marshalBackupCodes(hashes)
	if err != nil {
		return false, err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET backup_codes = $2::jsonb, updated_at = now()
		WHERE id = $1 AND totp_enabled = true`,
		userID, payload)
	if err != nil {
		return false, fmt.Errorf("httpapi: regenerate backup codes for user %d: %w", userID, err)
	}
	return rowsApplied(res)
}

// ConsumeBackupCode 实现 brief 冻结的条件 UPDATE，且在 SQL 内完成元素删除：
//
//	UPDATE users
//	SET backup_codes = backup_codes - $3::text
//	WHERE id = ? AND totp_enabled AND backup_codes @> jsonb_build_array($3::text)
//
// 为什么必须 SQL 内删除而不是「预读快照 → 写回剩余数组」：两个并发登录若各用
// 一个不同码，双方都基于同一快照计算 remaining，后提交者会把先提交者已删除的
// 码写回（丢失更新），使已用码复活。`jsonb - text` 在数据库内对当前值做增量
// 修改，只删除命中元素，天然与并发写互不覆盖。
//
// 并发语义（READ COMMITTED）：第二个事务在第一个提交后重新求值 WHERE，
// 此时数组已不含该哈希，UPDATE 影响 0 行 → 同一码恰有一个调用返回 true。
// totp_enabled 条件让「禁用 2FA 后旧码」无法再被核销。
func (r *SQLTwoFARepo) ConsumeBackupCode(ctx context.Context, userID int64, matchedHash string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET backup_codes = COALESCE(backup_codes, '[]'::jsonb) - $2::text,
		    updated_at = now()
		WHERE id = $1
		  AND totp_enabled = true
		  AND COALESCE(backup_codes, '[]'::jsonb) @> jsonb_build_array($2::text)`,
		userID, matchedHash)
	if err != nil {
		return false, fmt.Errorf("httpapi: consume backup code for user %d: %w", userID, err)
	}
	return rowsApplied(res)
}

func marshalBackupCodes(hashes []string) (string, error) {
	if hashes == nil {
		hashes = []string{}
	}
	payload, err := json.Marshal(hashes)
	if err != nil {
		return "", fmt.Errorf("httpapi: marshal backup codes: %w", err)
	}
	return string(payload), nil
}

func rowsApplied(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("httpapi: read rows affected: %w", err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// Handler 装配
// ---------------------------------------------------------------------------

// TwoFAConfig 是 2FA 处理器的装配输入。Repo 与 DB 二选一（单测注入内存 Repo）；
// Redis/Sessions/EncryptionKey 必填——2FA 依赖 ticket、会话与密钥治理，
// 缺失属于装配错误，必须让进程启动失败而不是带病服务。
type TwoFAConfig struct {
	DB       *sql.DB
	Repo     TwoFARepo
	Redis    *redis.Client
	Sessions *auth.SessionManager

	// EncryptionKey 即 config.CredentialEncryptionKey（CREDENTIAL_ENCRYPTION_KEY）。
	EncryptionKey string
	// Issuer 是 otpauth URI 的发行方标签；空时取 "NewTG"。
	Issuer string
	// Limiter 是登录失败锁定桶；nil 时按 Redis 构造。2FA 挑战与
	// disable/regenerate 的口令确认复用同一桶（modules-01:308「限流同登录桶」）。
	Limiter *LoginLimiter
	// Audit 为 nil 时不写审计（未装配日志库的测试/开发环境）。
	Audit *audit.Logger

	// 以下为测试接缝；生产留 nil 即使用 auth 的默认实现。
	VerifyPassword func(hash, plain string) bool
	Now            func() time.Time
	Logger         *slog.Logger
}

// TwoFAHandlers 是 2FA 端点集合。零值不可用，必须经 NewTwoFAHandlers 构造。
type TwoFAHandlers struct {
	repo           TwoFARepo
	rdb            *redis.Client
	sessions       *auth.SessionManager
	cipher         *auth.SecretCipher
	limiter        *LoginLimiter
	audit          *audit.Logger
	verifyPassword func(hash, plain string) bool
	now            func() time.Time
	issuer         string
	log            *slog.Logger
}

// NewTwoFAHandlers 构造 2FA 处理器。
func NewTwoFAHandlers(cfg TwoFAConfig) (*TwoFAHandlers, error) {
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: TwoFAConfig.Redis is required (pending tickets, failure counters, replay guard)")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("httpapi: TwoFAConfig.Sessions is required (challenge creates the session)")
	}
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: TwoFAConfig.Repo or DB is required")
		}
		repo = NewSQLTwoFARepo(cfg.DB)
	}
	cipher, err := auth.NewSecretCipher(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	verify := cfg.VerifyPassword
	if verify == nil {
		verify = auth.VerifyPassword
	}
	issuer := strings.TrimSpace(cfg.Issuer)
	if issuer == "" {
		issuer = "NewTG"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	limiter := cfg.Limiter
	if limiter == nil {
		limiter = NewLoginLimiter(cfg.Redis)
	}
	return &TwoFAHandlers{
		repo:           repo,
		rdb:            cfg.Redis,
		sessions:       cfg.Sessions,
		cipher:         cipher,
		limiter:        limiter,
		audit:          cfg.Audit,
		verifyPassword: verify,
		now:            now,
		issuer:         issuer,
		log:            log,
	}, nil
}

// TwoFARouteRegistrar 是 Register2FARoutes 需要的最小 mux 面（*http.ServeMux 满足）。
type TwoFARouteRegistrar interface {
	Handle(pattern string, handler http.Handler)
}

// Register2FARoutes 把 2FA 端点挂到路由上，供 routes.go 的装配方调用：
//
//	public  = 会话前的 mux（routes.go 的 rt.mux）——
//	          POST /api/auth/2fa/challenge 以一次性 ticket 为凭据，必须在
//	          SessionAuth → CSRF 链之外注册（否则无会话请求会被 401）。
//	protected = 受保护子 mux（rt.protected）——四个 /api/2fa/* 端点运行在
//	          SessionAuth → CSRF → OriginReferer 链之后，从 context 读 Principal。
//
// 典型装配（在 NewRouter 内）：
//
//	twoFA, err := NewTwoFAHandlers(TwoFAConfig{
//	    DB: cfg.DB, Redis: cfg.Redis, Sessions: cfg.Sessions,
//	    EncryptionKey: cfg.CredentialEncryptionKey, Logger: cfg.Logger,
//	})
//	if err != nil { return nil, err }
//	Register2FARoutes(rt.mux, rt.protected, twoFA)
func Register2FARoutes(public, protected TwoFARouteRegistrar, h *TwoFAHandlers) {
	public.Handle("POST /api/auth/2fa/challenge", http.HandlerFunc(h.HandleChallenge))
	protected.Handle("POST /api/2fa/setup", http.HandlerFunc(h.HandleSetup))
	protected.Handle("POST /api/2fa/enable", http.HandlerFunc(h.HandleEnable))
	protected.Handle("POST /api/2fa/disable", http.HandlerFunc(h.HandleDisable))
	protected.Handle("POST /api/2fa/backup-codes/regenerate", http.HandlerFunc(h.HandleRegenerateBackupCodes))
}

// ---------------------------------------------------------------------------
// 设置向导：setup / enable / disable / regenerate
// ---------------------------------------------------------------------------

type twoFASetupResponse struct {
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauth_url"`
}

type twoFABackupCodesResponse struct {
	TOTPEnabled bool     `json:"totp_enabled"`
	BackupCodes []string `json:"backup_codes"`
}

type twoFADisableResponse struct {
	TOTPEnabled bool `json:"totp_enabled"`
}

type twoFACodeRequest struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

type twoFAPasswordRequest struct {
	Password string `json:"password"`
}

// HandleSetup 生成新的 TOTP secret 与 otpauth URI，不写任何状态。
// secret 只在本次响应中出现；前端渲染二维码后应尽快丢弃。
func (h *TwoFAHandlers) HandleSetup(w http.ResponseWriter, r *http.Request) {
	user, ok := h.principalUser(w, r)
	if !ok {
		return
	}
	// 已启用时再生成 secret 只会在 enable 处被 409 拒绝，属于死路径；
	// 明确返回 409 让前端刷新卡片状态（而不是展示一个永远无法保存的二维码）。
	if user.TOTPEnabled {
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is already enabled")
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		h.log.Error("generate totp secret failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, twoFASetupResponse{
		Secret:     secret,
		OTPAuthURL: twoFAProvisioningURL(h.issuer, user.Username, secret),
	})
}

// HandleEnable 验证 {secret,code} 后一次性写入密文种子、8 组备用码哈希与
// totp_enabled=true，并把明文备用码返回给客户端（唯一一次）。
// 验证失败不产生任何服务端变更（TC-F-01-10-02 / brief (c)）。
func (h *TwoFAHandlers) HandleEnable(w http.ResponseWriter, r *http.Request) {
	user, ok := h.principalUser(w, r)
	if !ok {
		return
	}
	var req twoFACodeRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	// 归一化后用服务端冻结形状校验：32 个 base32 字符、解码 20 字节。
	// 拒绝客户端自选任意字节串当种子（ValidateTOTPSecret）。
	secret := strings.ToUpper(strings.TrimSpace(req.Secret))
	code := strings.TrimSpace(req.Code)
	if !auth.ValidateTOTPSecret(secret) || len([]rune(code)) > twoFAMaxCodeRunes {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid two-factor parameters")
		return
	}
	if user.TOTPEnabled {
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is already enabled")
		return
	}
	// OAuth 即时注册用户没有口令，无法在 disable/regenerate 时做口令确认。
	// 允许启用 2FA 会制造一个「开得掉、关不了」的状态，故在源头拒绝。
	if user.PasswordHash == "" {
		writeProblem(w, http.StatusForbidden, authProblemForbidden,
			"Set a password before enabling two-factor authentication")
		return
	}
	// 先验证再落库：错误码不得形成任何写入（含备用码生成）。
	if !auth.VerifyTOTP(secret, code, h.now()) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid verification code")
		return
	}
	plain, hashes, err := auth.GenerateBackupCodes(8)
	if err != nil {
		h.log.Error("generate backup codes failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	secretEnc, err := h.cipher.Encrypt(secret)
	if err != nil {
		h.log.Error("encrypt totp secret failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	applied, err := h.repo.EnableTwoFA(r.Context(), user.ID, secretEnc, hashes)
	if err != nil {
		h.log.Error("persist 2fa enable failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	if !applied {
		// 并发启用：另一个请求已经写入了自己的 secret，当前请求的码不再对应
		// 库内种子。返回 409 让客户端刷新状态，绝不覆盖已启用状态。
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is already enabled")
		return
	}
	h.afterTwoFAChange(w, r, user.ID)
	h.auditEvent(r, audit.ActionAuth2FAEnable, user, map[string]any{
		"action": "2fa_enabled", "factor": "totp", "change": "add", "backup_codes": len(plain),
	})
	writeJSON(w, http.StatusOK, twoFABackupCodesResponse{TOTPEnabled: true, BackupCodes: plain})
}

// HandleDisable 需要当前口令确认；清空种子/备用码/标志位（F-01-10）。
func (h *TwoFAHandlers) HandleDisable(w http.ResponseWriter, r *http.Request) {
	user, ok := h.principalUser(w, r)
	if !ok {
		return
	}
	if !user.TOTPEnabled {
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is not enabled")
		return
	}
	if !h.confirmPassword(w, r, user) {
		return
	}
	if err := h.repo.DisableTwoFA(r.Context(), user.ID); err != nil {
		h.log.Error("persist 2fa disable failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	h.afterTwoFAChange(w, r, user.ID)
	h.auditEvent(r, audit.ActionAuth2FADisable, user, map[string]any{
		"action": "2fa_disabled", "factor": "totp", "change": "remove",
	})
	writeJSON(w, http.StatusOK, twoFADisableResponse{TOTPEnabled: false})
}

// HandleRegenerateBackupCodes 需要当前口令确认；整体替换哈希数组，
// 旧码在 UPDATE 提交的瞬间全部失效（TC-F-01-10-04 / brief (e)）。
func (h *TwoFAHandlers) HandleRegenerateBackupCodes(w http.ResponseWriter, r *http.Request) {
	user, ok := h.principalUser(w, r)
	if !ok {
		return
	}
	if !user.TOTPEnabled {
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is not enabled")
		return
	}
	if !h.confirmPassword(w, r, user) {
		return
	}
	plain, hashes, err := auth.GenerateBackupCodes(8)
	if err != nil {
		h.log.Error("generate backup codes failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	applied, err := h.repo.ReplaceBackupCodes(r.Context(), user.ID, hashes)
	if err != nil {
		h.log.Error("persist backup code regeneration failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	if !applied {
		// 并发禁用：状态已不是 enabled，新码不能落库。
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is not enabled")
		return
	}
	h.afterTwoFAChange(w, r, user.ID)
	h.auditEvent(r, audit.ActionAuth2FABackupCodesRegenerate, user, map[string]any{
		"action": "backup_codes_regenerated", "factor": "backup_code",
		"change": "reset", "backup_codes": len(plain),
	})
	writeJSON(w, http.StatusOK, twoFABackupCodesResponse{TOTPEnabled: true, BackupCodes: plain})
}

// ---------------------------------------------------------------------------
// 登录挑战
// ---------------------------------------------------------------------------

type twoFAChallengeRequest struct {
	Ticket string `json:"ticket"`
	Code   string `json:"code"`
	Type   string `json:"type"` // "totp" | "backup"；缺省按 totp（兼容早期 {ticket,code} 契约）
}

// HandleChallenge 用一次性 ticket 换正式会话。ticket 由登录端点在口令通过后
// 写入 Redis（pending_2fa:{ticket} → user_id，5 分钟）；本端点不信任任何客户端
// 声明的用户身份。
func (h *TwoFAHandlers) HandleChallenge(w http.ResponseWriter, r *http.Request) {
	var req twoFAChallengeRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	challengeType := strings.ToLower(strings.TrimSpace(req.Type))
	if challengeType == "" {
		challengeType = "totp"
	}
	if challengeType != "totp" && challengeType != "backup" {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid challenge type")
		return
	}
	ticket := strings.TrimSpace(req.Ticket)
	if !validTwoFATicket(ticket) {
		// 畸形 ticket 与过期 ticket 同一响应：不给探测 ticket 存在性的信道。
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification ticket")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)

	// 登录桶（IP-only）先行：无效/伪造 ticket 的高频探测在触达 Redis GET 与
	// 数据库之前就被拒绝，与登录路径的「未知 identifier 只按 IP 计分」同口径。
	if err := h.limiter.Check(ctx, ip, ""); err != nil {
		h.writeLimiterFailure(w, err)
		return
	}

	rawUserID, err := h.rdb.Get(ctx, twoFATicketKeyPrefix+ticket).Result()
	if errors.Is(err, redis.Nil) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification ticket")
		return
	}
	if err != nil {
		h.log.Error("read 2fa ticket failed", "error", err)
		h.writeUnavailable(w)
		return
	}
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err != nil {
		h.log.Error("corrupt 2fa ticket", "error", err)
		h.writeUnavailable(w)
		return
	}

	user, err := h.repo.FindUserByID(ctx, userID)
	if err != nil {
		h.log.Error("find user for 2fa challenge failed", "user_id", userID, "error", err)
		h.writeUnavailable(w)
		return
	}
	if user == nil || user.Status != "enabled" {
		// 账户在签发 ticket 后被禁用/删除：作废 ticket，统一 401，不泄露状态。
		h.discardTicket(ctx, ticket)
		writeUnauthorized(w)
		return
	}

	// 与登录相同的双桶（IP+identifier / identifier）：验证前拦截已锁定账号。
	if err := h.limiter.Check(ctx, ip, user.Username); err != nil {
		h.writeLimiterFailure(w, err)
		return
	}

	var verified bool
	switch challengeType {
	case "totp":
		verified, err = h.verifyTOTPChallenge(ctx, user, req.Code)
	case "backup":
		verified, err = h.verifyBackupChallenge(ctx, user, req.Code)
	}
	if err != nil {
		h.log.Error("2fa challenge verification error", "user_id", user.ID, "type", challengeType, "error", err)
		h.writeUnavailable(w)
		return
	}
	if !verified {
		// 错码计入登录桶（失败侧 fail-closed：Redis 不可用时拒绝而不是放行）。
		if err := h.limiter.Record(ctx, ip, user.Username, false); err != nil {
			h.log.Error("record 2fa limiter failure failed", "user_id", user.ID, "error", err)
			h.writeLimiterFailure(w, err)
			return
		}
		title := "Incorrect verification code"
		if challengeType == "backup" {
			title = "Incorrect or already-used backup code"
		}
		h.rejectChallenge(ctx, w, ticket, title)
		return
	}

	// 成功：清零连续失败计数。失败则按认证面故障 fail-closed（ticket 尚未消费，
	// 但码已被核销/重放防护记录，用户需重新登录）。
	if err := h.limiter.Record(ctx, ip, user.Username, true); err != nil {
		h.log.Error("reset 2fa limiter failed", "user_id", user.ID, "error", err)
		h.writeLimiterFailure(w, err)
		return
	}
	// ticket 原子单次消费：GET+DEL 在 Lua 内比对 user_id。并发提交同一 ticket 的
	// 另一个请求会得到 consumed=false，绝不产生第二个会话。
	consumed, err := h.consumeTicket(ctx, ticket, rawUserID)
	if err != nil {
		h.log.Error("consume 2fa ticket failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	if !consumed {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification ticket")
		return
	}
	if err := h.startTwoFASession(ctx, w, r, user); err != nil {
		h.log.Error("create session after 2fa challenge failed", "user_id", user.ID, "error", err)
		h.writeUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, authSessionResponse{User: twoFAUserView(user)})
}

// verifyTOTPChallenge 解密种子并做 ±0 校验，再以 Redis SET NX 阻止同一步内
// 的重复使用。返回 (false, nil) 表示「码不对/已用过」，错误仅表示基础设施故障。
func (h *TwoFAHandlers) verifyTOTPChallenge(ctx context.Context, user *TwoFAUser, rawCode string) (bool, error) {
	if !user.TOTPEnabled || user.TOTPSecretEnc == "" {
		return false, nil
	}
	secret, err := h.cipher.Decrypt(user.TOTPSecretEnc)
	if err != nil {
		// 密文损坏/tag 不符属数据完整性事件（security-03 §2.5）：fail-closed，
		// 不把它当作「码错误」计入用户失败次数。
		return false, fmt.Errorf("decrypt totp secret: %w", err)
	}
	code := strings.TrimSpace(rawCode)
	if !auth.VerifyTOTP(secret, code, h.now()) {
		return false, nil
	}
	key := twoFATOTPUsedPrefix + strconv.FormatInt(user.ID, 10) + ":" + sha256Hex(code)
	first, err := h.rdb.SetNX(ctx, key, "1", twoFATOTPReplayTTL).Result()
	if err != nil {
		return false, fmt.Errorf("2fa replay guard: %w", err)
	}
	return first, nil
}

// verifyBackupChallenge 对哈希数组做恒定时间比对，命中后条件 UPDATE 原子核销。
// 返回 (false, nil) 表示未命中或并发已被消费。
func (h *TwoFAHandlers) verifyBackupChallenge(ctx context.Context, user *TwoFAUser, rawCode string) (bool, error) {
	matched, ok := auth.VerifyBackupCode(user.BackupCodes, rawCode)
	if !ok {
		return false, nil
	}
	// 核销只传命中哈希：剩余数组由 SQL 对当前值增量计算，避免丢失更新。
	return h.repo.ConsumeBackupCode(ctx, user.ID, matched)
}

// rejectChallenge 记录一次失败；第 5 次失败删除 ticket 并返回 429。
func (h *TwoFAHandlers) rejectChallenge(ctx context.Context, w http.ResponseWriter, ticket, title string) {
	n, err := twoFAFailScript.Run(ctx, h.rdb,
		[]string{twoFAFailKeyPrefix + ticket},
		strconv.FormatInt(pending2FATTL.Milliseconds(), 10),
	).Int64()
	if err != nil {
		h.log.Error("record 2fa failure failed", "error", err)
		h.writeUnavailable(w)
		return
	}
	if n >= twoFAMaxFailures {
		_ = h.rdb.Del(ctx, twoFATicketKeyPrefix+ticket).Err()
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests,
			"Too many verification attempts; please sign in again")
		return
	}
	writeProblem(w, http.StatusBadRequest, authProblemBadRequest, title)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// principalUser 读取会话身份并加载用户行。中间件已保证身份存在，这里仍做
// 防御性检查：handler 被直接挂到公开 mux 时必须 fail-closed。
func (h *TwoFAHandlers) principalUser(w http.ResponseWriter, r *http.Request) (*TwoFAUser, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p.UserID <= 0 {
		writeUnauthorized(w)
		return nil, false
	}
	user, err := h.repo.FindUserByID(r.Context(), p.UserID)
	if err != nil {
		h.log.Error("find user for 2fa failed", "user_id", p.UserID, "error", err)
		h.writeUnavailable(w)
		return nil, false
	}
	if user == nil {
		writeUnauthorized(w)
		return nil, false
	}
	// 会话有效不等于账号可用：禁用账号即使 Cookie 未过期也不得调用 2FA 端点。
	if user.Status != "enabled" {
		writeProblem(w, http.StatusForbidden, authProblemAccountDisabled, "This account has been disabled")
		return nil, false
	}
	return user, true
}

// confirmPassword 实施 F-01-10 的「禁用/重生成需当前口令」。口令为空或用户
// 没有口令（OAuth 即时注册）时拒绝；失败不触发任何写入。
//
// 与登录共用失败锁定桶（modules-01:308「限流同登录桶」）：错误口令会累计到
// 该用户名/IP 的连续失败计数，成功则清零。这防止已劫持会话的攻击者在线爆破
// 当前口令来关闭 2FA。
func (h *TwoFAHandlers) confirmPassword(w http.ResponseWriter, r *http.Request, user *TwoFAUser) bool {
	var req twoFAPasswordRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return false
	}
	if user.PasswordHash == "" {
		writeProblem(w, http.StatusForbidden, authProblemForbidden,
			"Password confirmation is not available for this account")
		return false
	}
	ctx := r.Context()
	ip := clientIP(r)
	if err := h.limiter.Check(ctx, ip, user.Username); err != nil {
		h.writeLimiterFailure(w, err)
		return false
	}
	if !h.verifyPassword(user.PasswordHash, req.Password) {
		if err := h.limiter.Record(ctx, ip, user.Username, false); err != nil {
			h.log.Error("record 2fa reauth failure failed", "user_id", user.ID, "error", err)
			h.writeLimiterFailure(w, err)
			return false
		}
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid password")
		return false
	}
	if err := h.limiter.Record(ctx, ip, user.Username, true); err != nil {
		h.log.Error("reset 2fa reauth limiter failed", "user_id", user.ID, "error", err)
		h.writeLimiterFailure(w, err)
		return false
	}
	return true
}

// startTwoFASession 建 Redis 会话并签发 CSRF 令牌，写两个 Cookie。
// 与 handlers_auth.go 的 startSession 同口径；独立实现是因为 2FA 处理器
// 不持有 authHandlers（路由拥有者可以单独装配本文件）。
func (h *TwoFAHandlers) startTwoFASession(ctx context.Context, w http.ResponseWriter, r *http.Request, user *TwoFAUser) error {
	sid, err := h.sessions.Create(ctx, user.ID, auth.SessionOpts{
		UA:   r.UserAgent(),
		IP:   clientIP(r),
		Role: user.Role, // 角色只来自 DB 行，绝不读请求体
	})
	if err != nil {
		return err
	}
	token := auth.NewCSRFToken()
	if err := h.sessions.IssueCSRF(ctx, sid, token); err != nil {
		_ = h.sessions.Revoke(ctx, sid)
		return err
	}
	setSessionCookies(w, sid, token)
	return nil
}

func (h *TwoFAHandlers) discardTicket(ctx context.Context, ticket string) {
	// best-effort：ticket 本身有 5 分钟 TTL；失败时写日志而不是让已成功的
	// 挑战返回错误（码已被核销/重放防护，残留 ticket 不可再利用同一凭据）。
	if err := h.rdb.Del(ctx, twoFATicketKeyPrefix+ticket, twoFAFailKeyPrefix+ticket).Err(); err != nil {
		h.log.Error("discard 2fa ticket failed", "error", err)
	}
}

// consumeTicket 在成功路径上原子单次消费 ticket：Lua 内比对 user_id 后删除
// ticket 与失败计数。两个并发请求同时通过验证时，只有一个能把 ticket 换成
// 会话（另一个拿到 consumed=false），不会从同一 ticket 派生两个会话。
func (h *TwoFAHandlers) consumeTicket(ctx context.Context, ticket, rawUserID string) (bool, error) {
	n, err := twoFAConsumeTicketScript.Run(ctx, h.rdb,
		[]string{twoFATicketKeyPrefix + ticket, twoFAFailKeyPrefix + ticket},
		rawUserID,
	).Int64()
	if err != nil {
		return false, fmt.Errorf("httpapi: consume 2fa ticket: %w", err)
	}
	return n == 1, nil
}

// afterTwoFAChange 实施 I13（MFA 因子新增/移除/重置后吊销该用户除当前会话外的
// 全部会话）与 TC-A-04（2FA 状态变更后轮换 CSRF 令牌）。
//
// keepSID 取当前请求的会话 Cookie；Bearer 路径（无 Cookie）时 keepSID 为空，
// RevokeUser 会吊销该用户全部会话——这正是 I13 对「非会话路径」的要求。
// 吊销/轮换失败只记录日志：因子变更已经提交，改报 503 只会诱导无意义重试；
// 且 Redis 故障期间 SessionAuth 本身 fail-closed，旧会话无法被使用。
func (h *TwoFAHandlers) afterTwoFAChange(w http.ResponseWriter, r *http.Request, userID int64) {
	ctx := r.Context()
	keepSID := ""
	if sid, ok := sessionID(r); ok {
		keepSID = sid
	}
	if err := h.sessions.RevokeUser(ctx, userID, keepSID); err != nil {
		h.log.Error("revoke sessions after 2fa change failed", "user_id", userID, "error", err)
	}
	if keepSID == "" {
		return // Bearer 路径没有 CSRF Cookie
	}
	token := auth.NewCSRFToken()
	if err := h.sessions.IssueCSRF(ctx, keepSID, token); err != nil {
		h.log.Error("rotate csrf after 2fa change failed", "user_id", userID, "error", err)
		return
	}
	setSessionCookies(w, keepSID, token)
}

// auditEvent 写 2FA 审计（best-effort：日志库独立且无跨库事务，失败只记日志）。
// detail 只放状态与计数，绝不含 secret / 备用码明文。
func (h *TwoFAHandlers) auditEvent(r *http.Request, action string, user *TwoFAUser, detail map[string]any) {
	if h.audit == nil {
		return
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		h.log.Warn("marshal 2fa audit detail failed", "action", action, "error", err)
		return
	}
	event := audit.Event{
		Action:    action,
		Actor:     user.ID,
		Username:  user.Username,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Method:    r.Method,
		Route:     r.Pattern,
		Detail:    raw,
	}
	if err := h.audit.LogEvent(r.Context(), event); err != nil {
		h.log.Warn("2fa audit write failed", "action", action, "actor", user.ID, "error", err)
	}
}

// writeLimiterFailure 与登录路径同口径：锁定 → 429 + Retry-After；
// 其它 Redis 故障 → 503 fail-closed（security-07 §2.3）。
func (h *TwoFAHandlers) writeLimiterFailure(w http.ResponseWriter, err error) {
	var locked *LockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", retrySeconds(locked.RetryAfter))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests,
			"Too many failed verification attempts")
		return
	}
	h.log.Error("2fa limiter unavailable", "error", err)
	h.writeUnavailable(w)
}

func (h *TwoFAHandlers) writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
		"Two-factor authentication is temporarily unavailable")
}

func twoFAUserView(u *TwoFAUser) authUserView {
	return authUserView{
		ID:                 u.ID,
		Username:           u.Username,
		Email:              u.Email,
		Role:               u.Role,
		EmailVerified:      u.EmailVerified,
		MustChangePassword: u.MustChangePassword,
	}
}

// validTwoFATicket 校验 ticket 形态（base64url(32 bytes)，与 createPending2FA
// 的生成口径一致）。除尽早拒绝畸形输入外，这防止任意字符串被拼进 Redis 键。
func validTwoFATicket(ticket string) bool {
	if len(ticket) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	return err == nil && len(raw) == 32
}

// twoFAProvisioningURL 构造 otpauth://totp/... URI 供前端渲染二维码。
func twoFAProvisioningURL(issuer, account, secret string) string {
	u := url.URL{
		Scheme: "otpauth",
		Host:   "totp",
		Path:   "/" + issuer + ":" + account,
	}
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(auth.TOTPDigits))
	q.Set("period", strconv.Itoa(auth.TOTPStepSeconds))
	u.RawQuery = q.Encode()
	return u.String()
}

// twoFAFailScript 原子地 INCR 失败计数并在首次失败时设置 TTL。
var twoFAFailScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n
`)

// twoFAConsumeTicketScript 原子单次消费 ticket：仅当键值与签发时的 user_id
// 一致才删除。GET+DEL 分开执行会留下「两个请求都读到 ticket」的窗口。
var twoFAConsumeTicketScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == ARGV[1] then
  redis.call('DEL', KEYS[1])
  redis.call('DEL', KEYS[2])
  return 1
end
return 0
`)
