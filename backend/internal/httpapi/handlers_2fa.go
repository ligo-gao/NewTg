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

	twoFAMaxSecretBytes = 128
	twoFAMaxCodeRunes   = 32
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
	// ConsumeBackupCode 条件核销：仅当 users.backup_codes 仍包含 matchedHash
	// 时把它替换为 remaining。applied=false 表示并发已被消费/码已重生成。
	ConsumeBackupCode(ctx context.Context, userID int64, matchedHash string, remaining []string) (applied bool, err error)
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

// ConsumeBackupCode 实现 brief 冻结的条件 UPDATE：
//
//	UPDATE users SET backup_codes = <去掉该码后的数组>
//	WHERE id = ? AND backup_codes @> <该码哈希>
//
// 并发语义（READ COMMITTED）：第二个事务在第一个提交后重新求值 WHERE，
// 此时数组已不含该哈希，UPDATE 影响 0 行 → 恰有一个调用返回 true。
// totp_enabled 条件让「禁用 2FA 后旧码」无法再被核销。
func (r *SQLTwoFARepo) ConsumeBackupCode(ctx context.Context, userID int64, matchedHash string, remaining []string) (bool, error) {
	payload, err := marshalBackupCodes(remaining)
	if err != nil {
		return false, err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET backup_codes = $2::jsonb, updated_at = now()
		WHERE id = $1
		  AND totp_enabled = true
		  AND COALESCE(backup_codes, '[]'::jsonb) @> jsonb_build_array($3::text)`,
		userID, payload, matchedHash)
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
	return &TwoFAHandlers{
		repo:           repo,
		rdb:            cfg.Redis,
		sessions:       cfg.Sessions,
		cipher:         cipher,
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
	secret := strings.TrimSpace(req.Secret)
	code := strings.TrimSpace(req.Code)
	if secret == "" || len(secret) > twoFAMaxSecretBytes || len([]rune(code)) > twoFAMaxCodeRunes {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid two-factor parameters")
		return
	}
	if user.TOTPEnabled {
		writeProblem(w, http.StatusConflict, authProblemConflict, "Two-factor authentication is already enabled")
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
	// 因子变更后吊销其它会话（security-01 §2.1 生命周期），保留当前会话：
	// 失败只记录——禁用 2FA 已生效，把成功操作改报 503 只会诱导无意义重试。
	if sid, ok := sessionID(r); ok {
		if err := h.sessions.RevokeUser(r.Context(), user.ID, sid); err != nil {
			h.log.Error("revoke sessions after 2fa disable failed", "user_id", user.ID, "error", err)
		}
	}
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
		title := "Incorrect verification code"
		if challengeType == "backup" {
			title = "Incorrect or already-used backup code"
		}
		h.rejectChallenge(ctx, w, ticket, title)
		return
	}

	// 成功：先消费 ticket（即使随后建会话失败也不得重复使用），再建正式会话。
	h.discardTicket(ctx, ticket)
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
	remaining := removeBackupHash(user.BackupCodes, matched)
	return h.repo.ConsumeBackupCode(ctx, user.ID, matched, remaining)
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
	return user, true
}

// confirmPassword 实施 F-01-10 的「禁用/重生成需当前口令」。口令为空或用户
// 没有口令（OAuth 即时注册）时拒绝；失败不触发任何写入。
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
	if !h.verifyPassword(user.PasswordHash, req.Password) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid password")
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

// removeBackupHash 返回去掉 matchedHash 全部出现后的新数组。
// 复制而非原地修改，保证调用方传入的快照不被改变。
func removeBackupHash(hashes []string, matchedHash string) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if h != matchedHash {
			out = append(out, h)
		}
	}
	return out
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
