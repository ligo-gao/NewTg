// 本文件实现密码生命周期端点（modules-01 F-01-05/06/16）与全量再认证门的
// HTTP 面辅助。
//
// 路由挂载：Task 8 不修改 routes.go。装配方在 NewRouter 之后、开始服务之前调用
//
//	rt.RegisterPasswordRoutes(cfg.PublicBaseURL)
//
// 三个重置端点注册到公开 mux（与 /api/auth/email-code 同级，豁免 CSRF/Origin：
// 它们只凭一次性 token/邮箱工作，不依赖既有会话）；POST /api/password 注册到
// 受保护子树，沿用 SessionAuth → CSRF → OriginRefererCheck 链。
//
// 安全口径：
//   - 找回密码对「邮箱是否存在」给出逐字相同的响应（防枚举）；限流先行，
//     已注册与未注册邮箱同样计入 IP/邮箱桶。
//   - 重置 token 是 256-bit CSPRNG（32 字节 base64url），Redis 30 分钟、
//     一次性消费（GET+DEL 原子脚本），明文只出现在发给用户的链接里，
//     绝不写日志。
//   - 改密/重置成功后吊销会话：重置吊销全部；改密保留当前会话并轮换 CSRF
//     （security-01 §2.2「刷新时机」）。
//   - 口令写入只存 argon2id 哈希（GC-9），并清除 must_change_password
//     （R-02：临时口令在用户自选新口令后自然解除）。
package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
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

// 冻结常量。阈值唯一来源 modules-08 F-08-16（独立限流桶表）：
// 找回密码 5 次/分钟/IP；改密限流来自 modules-01 F-01-16（5 次/10 分钟/IP+user）。
const (
	// passwordResetTokenBytes：256-bit token（modules-01 F-01-05 冻结）。
	passwordResetTokenBytes = 32
	// passwordResetTokenTTL：重置链接 30 分钟有效（F-01-05 冻结）。
	passwordResetTokenTTL = 30 * time.Minute
	// passwordResetResendAfter：同邮箱重发冷却（UI 30s 倒计时，服务端取 60s，
	// 与邮箱验证码同口径 F-01-02/F-08-16）。
	passwordResetResendAfter = 60 * time.Second
	// passwordResetIPLimit / passwordResetIPWindow：5 次/分钟/IP（F-08-16）。
	passwordResetIPLimit  = 5
	passwordResetIPWindow = time.Minute
	// passwordChangeLimit / passwordChangeWindow：5 次/10 分钟/IP+user（F-01-16）。
	passwordChangeLimit  = 5
	passwordChangeWindow = 10 * time.Minute
)

// Redis 键前缀。邮箱/IP 一律先 SHA-256 再入键，避免 PII 明文进入 Redis 快照。
// 重置 token 键名按设计冻结为 pwd_reset:{token}（无独立索引，token 本身即句柄）。
const (
	passwordResetKeyPrefix     = "pwd_reset:"
	passwordResetCooldownKey   = "pwd_reset:cooldown:"
	passwordResetIPKey         = "pwd_reset:ip:"
	passwordChangeIPUserKey    = "password_change:ipuser:"
	problemTypeResetToken      = "https://newtg.dev/problems/invalid_reset_token"
	problemTypeCurrentPassword = "https://newtg.dev/problems/invalid_current_password"
	problemTypeReauthRequired  = "https://newtg.dev/problems/reauth_required"
)

// ErrPasswordUserNotFound 表示口令更新目标行不存在（用户已被并发删除）。
// 改密/重置据此返回失败而不是静默「成功」。
var ErrPasswordUserNotFound = errors.New("httpapi: user not found for password update")

// PasswordResetSender 是「发送重置链接」的专用接缝。它刻意与
// EmailCodeSender（6 位验证码）分开：重置邮件模板渲染的是 reset_url，
// 把链接塞进验证码参数会让模板语义漂移。生产 SMTP 实现应两者都实现。
type PasswordResetSender interface {
	SendPasswordReset(ctx context.Context, email, resetURL string) error
}

// PasswordRepo 是密码端点对 users 表的扩展依赖面。AuthRepo 只覆盖注册/登录
// 所需的按 identifier 查找；密码端点还需要按 id/邮箱精确定位与写回哈希。
//
// 生产实现：*SQLAuthRepo（本文件提供三个方法）。单测注入内存假实现。
type PasswordRepo interface {
	// FindUserByEmail 按注册邮箱精确查找（大小写不敏感）；不存在返回 (nil, nil)。
	FindUserByEmail(ctx context.Context, email string) (*UserRecord, error)
	// FindUserByID 按主键查找；不存在返回 (nil, nil)。
	FindUserByID(ctx context.Context, userID int64) (*UserRecord, error)
	// UpdatePasswordHash 写入新 argon2id 哈希并清除 must_change_password。
	// 行不存在返回 ErrPasswordUserNotFound。
	UpdatePasswordHash(ctx context.Context, userID int64, passwordHash string) error
}

// passwordUserColumns 与 FindUserByIdentifier 的投影保持同一口径，避免两个
// 读取路径对 NULL/布尔列的解释漂移。
const passwordUserColumns = `id, username, COALESCE(email, ''), COALESCE(password_hash, ''),
	role, status, email_verified_at IS NOT NULL, totp_enabled, must_change_password`

// passwordRowScanner 抽象 *sql.Row，便于统一处理 sql.ErrNoRows → (nil, nil)。
type passwordRowScanner interface {
	Scan(dest ...any) error
}

func scanPasswordUser(row passwordRowScanner) (*UserRecord, error) {
	var u UserRecord
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash,
		&u.Role, &u.Status, &u.EmailVerified, &u.TOTPEnabled, &u.MustChangePassword)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// FindUserByEmail 实现 PasswordRepo。
func (r *SQLAuthRepo) FindUserByEmail(ctx context.Context, email string) (*UserRecord, error) {
	u, err := scanPasswordUser(r.db.QueryRowContext(ctx,
		`SELECT `+passwordUserColumns+` FROM users
		 WHERE email IS NOT NULL AND lower(email) = lower($1)
		 LIMIT 1`, email))
	if err != nil {
		return nil, fmt.Errorf("httpapi: find user by email: %w", err)
	}
	return u, nil
}

// FindUserByID 实现 PasswordRepo。
func (r *SQLAuthRepo) FindUserByID(ctx context.Context, userID int64) (*UserRecord, error) {
	u, err := scanPasswordUser(r.db.QueryRowContext(ctx,
		`SELECT `+passwordUserColumns+` FROM users WHERE id = $1 LIMIT 1`, userID))
	if err != nil {
		return nil, fmt.Errorf("httpapi: find user by id: %w", err)
	}
	return u, nil
}

// UpdatePasswordHash 实现 PasswordRepo。用 UPDATE ... RETURNING 区分「落库」与
// 「行已不存在」，避免把并发删除当成改密成功。
func (r *SQLAuthRepo) UpdatePasswordHash(ctx context.Context, userID int64, passwordHash string) error {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`UPDATE users
		 SET password_hash = $1, must_change_password = false, updated_at = now()
		 WHERE id = $2
		 RETURNING id`, passwordHash, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPasswordUserNotFound
	}
	if err != nil {
		return fmt.Errorf("httpapi: update password hash: %w", err)
	}
	return nil
}

// consumePasswordResetScript 原子地读取并删除重置 token：并发提交同一 token
// 时只有一个请求能拿到 user_id，其余得到空（400），满足「一次性」。
var consumePasswordResetScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then
  return false
end
redis.call('DEL', KEYS[1])
return v
`)

// passwordHandlers 持有密码端点依赖。它包装 Task 6 的 authHandlers，复用其
// 仓储、会话、Redis、发送器、泄露口令检查器与日志，避免在 RouterConfig 里
// 再复制一套装配参数。
type passwordHandlers struct {
	base     *authHandlers
	repo     PasswordRepo
	rdb      *redis.Client
	sessions *auth.SessionManager
	sender   EmailCodeSender
	checker  auth.CompromisedPasswordChecker
	log      *slog.Logger

	verifyPassword func(hash, plain string) bool
	hashPassword   func(plain string) (string, error)

	// publicBaseURL 是重置链接的固定源。为空时拒绝发信（见 resetLink）：
	// 用请求 Host 拼链接会把 Host 头注入变成「重置 token 钓到攻击者域名」。
	publicBaseURL string
}

func newPasswordHandlers(base *authHandlers, repo PasswordRepo, publicBaseURL string) *passwordHandlers {
	return &passwordHandlers{
		base:           base,
		repo:           repo,
		rdb:            base.rdb,
		sessions:       base.sessions,
		sender:         base.sender,
		checker:        base.checker,
		log:            base.log,
		verifyPassword: base.verifyPassword,
		hashPassword:   base.hashPassword,
		publicBaseURL:  strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
	}
}

// RegisterPasswordRoutes 注册 Task 8 的全部端点。装配方在 NewRouter 之后调用
// 一次；返回装配错误（缺少 PasswordRepo）而不是带病运行。
//
// publicBaseURL 用于拼重置链接（如 https://app.example.com）。为空时不发重置
// 邮件但仍返回统一成功态——宁可让运维发现 PUBLIC_BASE_URL 未配置，也不能把
// 攻击者可控的 Host 头写进受害者邮件。
func (rt *Router) RegisterPasswordRoutes(publicBaseURL string) error {
	if rt == nil || rt.handlers == nil {
		return errors.New("httpapi: router is not initialized")
	}
	repo, ok := rt.handlers.repo.(PasswordRepo)
	if !ok {
		return errors.New("httpapi: password routes require a PasswordRepo (FindUserByEmail/FindUserByID/UpdatePasswordHash)")
	}
	h := newPasswordHandlers(rt.handlers, repo, publicBaseURL)

	// 公开端点：不挂 CSRF/Origin（无会话可依赖，token/邮箱即凭据）。
	rt.mux.HandleFunc("POST /api/auth/password-reset/request", h.handleResetRequest)
	rt.mux.HandleFunc("POST /api/auth/password-reset/validate", h.handleResetValidate)
	rt.mux.HandleFunc("POST /api/auth/password-reset/confirm", h.handleResetConfirm)
	// 登录态端点：沿用受保护链（SessionAuth → CSRF → Origin）。
	rt.HandleProtected("POST /api/password", http.HandlerFunc(h.handleChangePassword))
	return nil
}

// writeReauthRequired 是 Task 10（换邮箱/删账号）复用的 403 出口：
// ReauthGate 返回 auth.ErrReauthRequired 时调用。
func writeReauthRequired(w http.ResponseWriter) {
	writeProblem(w, http.StatusForbidden, problemTypeReauthRequired, "Reauthentication required")
}

// ---------------------------------------------------------------------------
// 找回密码（F-01-05）
// ---------------------------------------------------------------------------

type passwordResetRequestBody struct {
	Email string `json:"email"`
}

// handleResetRequest 生成一次性重置链接并发信。无论邮箱是否注册，成功态
// 完全一致（204 空体）；限流与格式错误是仅有的差异化响应。
func (h *passwordHandlers) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	var req passwordResetRequestBody
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)

	// 同邮箱 60s 冷却。先占用冷却键（无论邮箱是否存在），使两条路径的
	// 限流行为一致，不引入新的枚举信道。
	cooldownKey := passwordResetCooldownKey + sha256Hex(email)
	acquired, err := h.rdb.SetNX(ctx, cooldownKey, "1", passwordResetResendAfter).Result()
	if err != nil {
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if !acquired {
		retry := h.base.ttlOr(ctx, cooldownKey, passwordResetResendAfter)
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests,
			"Please wait before requesting another reset email")
		return
	}

	// 同 IP 5 次/分钟（F-08-16 唯一真源）。
	ipKey := passwordResetIPKey + sha256Hex(ip)
	n, err := emailIPLimitScript.Run(ctx, h.rdb, []string{ipKey},
		strconv.Itoa(int(passwordResetIPWindow.Seconds()))).Int64()
	if err != nil {
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if n > passwordResetIPLimit {
		retry := h.base.ttlOr(ctx, ipKey, passwordResetIPWindow)
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests,
			"Too many reset requests")
		return
	}

	user, err := h.repo.FindUserByEmail(ctx, email)
	if err != nil {
		h.log.Error("find user by email failed", "error", err)
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if user == nil {
		// 不存在：与已注册邮箱相同的成功态。不发信、不落 token。
		w.WriteHeader(http.StatusNoContent)
		return
	}

	token, err := newPasswordResetToken()
	if err != nil {
		h.log.Error("generate password reset token failed", "error", err)
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if err := h.rdb.Set(ctx, passwordResetKeyPrefix+token,
		strconv.FormatInt(user.ID, 10), passwordResetTokenTTL).Err(); err != nil {
		h.log.Error("store password reset token failed", "error", err)
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}

	if link, ok := h.resetLink(token); ok {
		h.sendResetLink(ctx, email, link)
	} else {
		// 与成功态一致：配置缺失只影响投递，不向客户端泄露账号存在性。
		h.log.Warn("PUBLIC_BASE_URL is not configured; password reset email not sent")
	}
	w.WriteHeader(http.StatusNoContent)
}

// resetLink 用配置的固定源拼重置链接；未配置时返回 ok=false（拒绝发信）。
func (h *passwordHandlers) resetLink(token string) (string, bool) {
	if h.publicBaseURL == "" {
		return "", false
	}
	return h.publicBaseURL + "/reset-password?token=" + url.QueryEscape(token), true
}

// sendResetLink 通过专用发送器投递；发送失败只记日志，不让响应产生差异
// （否则「发送失败」会成为账号存在性的旁路）。明文 token 绝不入日志。
func (h *passwordHandlers) sendResetLink(ctx context.Context, email, link string) {
	sender, ok := h.sender.(PasswordResetSender)
	if !ok {
		if h.sender != nil {
			h.log.Warn("email sender does not implement PasswordResetSender; password reset link not sent")
		}
		return
	}
	if err := sender.SendPasswordReset(ctx, email, link); err != nil {
		h.log.Error("send password reset email failed", "error", err)
	}
}

func newPasswordResetToken() (string, error) {
	buf := make([]byte, passwordResetTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("httpapi: generate password reset token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// validPasswordResetToken 只接受本系统产出的 32 字节 base64url 形态：畸形输入
// 在访问 Redis 前即被拒绝，避免把任意字符串拼进键名。
func validPasswordResetToken(token string) bool {
	if token == "" || len(token) > 128 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == passwordResetTokenBytes
}

type passwordResetTokenBody struct {
	Token string `json:"token"`
}

// handleResetValidate 供重置页加载时校验链接，不消费 token。
// 200 {"valid":true}；无效/过期一律 400（页面进入错误态）。
func (h *passwordHandlers) handleResetValidate(w http.ResponseWriter, r *http.Request) {
	var req passwordResetTokenBody
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	if !validPasswordResetToken(req.Token) {
		h.writeInvalidResetToken(w)
		return
	}
	n, err := h.rdb.Exists(r.Context(), passwordResetKeyPrefix+req.Token).Result()
	if err != nil {
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if n == 0 {
		h.writeInvalidResetToken(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

type passwordResetConfirmBody struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
	Confirm     string `json:"confirm"`
}

// handleResetConfirm 消费一次性 token、更新口令并吊销该用户全部会话。
//
// 顺序：输入校验 → 原子消费 token → 查找用户 → 计算哈希 → 吊销会话 → 落库。
// 先消费 token 保证并发/重放只有一个请求能生效；先吊销再落库保证「成功响应
// 一定意味着旧会话已失效」——反向顺序在 Redis 故障时会留下密码已换但旧会话
// 仍存活的窗口。
func (h *passwordHandlers) handleResetConfirm(w http.ResponseWriter, r *http.Request) {
	var req passwordResetConfirmBody
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	// 输入错误不得消费 token：用户手滑后仍可用同一链接重试。
	if !validPasswordResetToken(req.Token) {
		h.writeInvalidResetToken(w)
		return
	}
	if !auth.ValidatePasswordLength(req.NewPassword) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Password must be between 8 and 128 characters")
		return
	}
	if req.NewPassword != req.Confirm {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Passwords do not match")
		return
	}
	if h.checker.Check(req.NewPassword) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Password is too weak or has been compromised")
		return
	}
	ctx := r.Context()

	uidRaw, err := consumePasswordResetScript.Run(ctx, h.rdb,
		[]string{passwordResetKeyPrefix + req.Token}).Text()
	if errors.Is(err, redis.Nil) {
		h.writeInvalidResetToken(w)
		return
	}
	if err != nil {
		h.log.Error("consume password reset token failed", "error", err)
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(uidRaw), 10, 64)
	if err != nil {
		// 值不可解析说明 Redis 数据损坏；按无效链接处理，不泄露内部状态。
		h.log.Error("corrupt password reset token record", "error", err)
		h.writeInvalidResetToken(w)
		return
	}

	user, err := h.repo.FindUserByID(ctx, userID)
	if err != nil {
		h.log.Error("find reset user failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password reset failed")
		return
	}
	if user == nil {
		h.writeInvalidResetToken(w)
		return
	}

	hash, err := h.hashPassword(req.NewPassword)
	if err != nil {
		h.log.Error("hash new password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password reset failed")
		return
	}
	if err := h.sessions.RevokeUser(ctx, user.ID, ""); err != nil {
		h.log.Error("revoke sessions after reset failed", "error", err)
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if err := h.repo.UpdatePasswordHash(ctx, user.ID, hash); err != nil {
		h.log.Error("update password after reset failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password reset failed")
		return
	}
	// 凭证已变更：旧凭证换来的再认证窗口不得继续授权敏感操作（best-effort）。
	if err := auth.ClearReauthWindow(ctx, h.rdb, user.ID); err != nil {
		h.log.Error("clear reauth window after reset failed", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *passwordHandlers) writeInvalidResetToken(w http.ResponseWriter) {
	writeProblem(w, http.StatusBadRequest, problemTypeResetToken,
		"This reset link is invalid or has expired")
}

// ---------------------------------------------------------------------------
// 修改密码（F-01-16）
// ---------------------------------------------------------------------------

type passwordChangeBody struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword 校验当前密码、更新哈希、吊销除当前外全部会话并轮换
// 当前会话的 CSRF 令牌。
func (h *passwordHandlers) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeUnauthorized(w)
		return
	}
	var req passwordChangeBody
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	ctx := r.Context()

	// 限流先行：5 次/10 分钟/IP+user，计数包含失败与成功（防在线爆破）。
	// Redis 故障 fail-closed（503），绝不放行无计数的口令尝试。
	limitKey := passwordChangeIPUserKey + sha256Hex(clientIP(r)+"\x00"+strconv.FormatInt(principal.UserID, 10))
	n, err := emailIPLimitScript.Run(ctx, h.rdb, []string{limitKey},
		strconv.Itoa(int(passwordChangeWindow.Seconds()))).Int64()
	if err != nil {
		h.writeUnavailable(w, "Password change is temporarily unavailable")
		return
	}
	if n > passwordChangeLimit {
		retry := h.base.ttlOr(ctx, limitKey, passwordChangeWindow)
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many password change attempts")
		return
	}

	user, err := h.repo.FindUserByID(ctx, principal.UserID)
	if err != nil {
		h.log.Error("find user for password change failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password change failed")
		return
	}
	if user == nil {
		// 会话指向已删除用户：身份不再有效。
		writeUnauthorized(w)
		return
	}

	// 当前密码校验。无口令用户仍跑一次等价 KDF，保持时序一致。
	if user.PasswordHash == "" {
		_ = h.verifyPassword(dummyPasswordHash(), req.CurrentPassword)
		h.writeCurrentPasswordIncorrect(w)
		return
	}
	if !h.verifyPassword(user.PasswordHash, req.CurrentPassword) {
		h.writeCurrentPasswordIncorrect(w)
		return
	}
	if !auth.ValidatePasswordLength(req.NewPassword) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Password must be between 8 and 128 characters")
		return
	}
	if req.NewPassword == req.CurrentPassword {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"New password must differ from the current one")
		return
	}
	if h.checker.CheckWithContext(req.NewPassword, user.Username) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Password is too weak or has been compromised")
		return
	}

	hash, err := h.hashPassword(req.NewPassword)
	if err != nil {
		h.log.Error("hash new password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password change failed")
		return
	}

	sid, hasSession := sessionID(r)
	// 先吊销（保留当前会话）再落库：任何 204 都保证其他会话已经失效。
	if err := h.sessions.RevokeUser(ctx, user.ID, sid); err != nil {
		h.log.Error("revoke other sessions failed", "error", err)
		h.writeUnavailable(w, "Password change is temporarily unavailable")
		return
	}
	// CSRF 轮换也放在落库之前：Redis 故障时不能出现「密码已改、旧同步器令牌
	// 仍有效」的半状态。IssueCSRF 只改会话 hash、不延长会话寿命。
	// Bearer 路径没有 Cookie 会话可轮换（同步器令牌只属于 Cookie 路径）。
	var csrfToken string
	if hasSession {
		csrfToken = auth.NewCSRFToken()
		if err := h.sessions.IssueCSRF(ctx, sid, csrfToken); err != nil {
			h.log.Error("rotate csrf token failed", "error", err)
			h.writeUnavailable(w, "Password change is temporarily unavailable")
			return
		}
		// 先写响应头：即使后续落库失败，客户端也能拿到与服务端一致的令牌，
		// 不会卡在「旧令牌已被服务端作废」的状态。
		setCSRFCookie(w, csrfToken)
	}
	if err := h.repo.UpdatePasswordHash(ctx, user.ID, hash); err != nil {
		h.log.Error("update password hash failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password change failed")
		return
	}
	if err := auth.ClearReauthWindow(ctx, h.rdb, user.ID); err != nil {
		h.log.Error("clear reauth window after password change failed", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *passwordHandlers) writeCurrentPasswordIncorrect(w http.ResponseWriter) {
	writeProblem(w, http.StatusBadRequest, problemTypeCurrentPassword, "Current password is incorrect")
}

func (h *passwordHandlers) writeUnavailable(w http.ResponseWriter, title string) {
	w.Header().Set("Retry-After", "5")
	writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable, title)
}

// setCSRFCookie 只下发轮换后的 CSRF Cookie（属性与 startSession 一致），
// 不触碰会话 Cookie。
func setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // 前端必须能读取后写入 X-CSRF-Token
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
