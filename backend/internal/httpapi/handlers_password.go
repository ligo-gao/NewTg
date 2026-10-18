// 本文件实现密码生命周期端点（modules-01 F-01-05/06/16）与全量再认证门的
// HTTP 面辅助。
//
// 路由挂载：Task 8 不修改 routes.go。装配方在 NewRouter 之后、开始服务之前调用
//
//	rt.RegisterPasswordRoutes(cfg.PublicBaseURL, auditLogger)
//
// 三个重置端点注册到公开 mux（与 /api/auth/email-code 同级，豁免 CSRF/Origin：
// 它们只凭一次性 token/邮箱工作，不依赖既有会话）；POST /api/password 注册到
// 受保护子树，沿用 SessionAuth → CSRF → OriginRefererCheck 链。
//
// 安全口径：
//   - 找回密码对「邮箱是否存在」给出逐字相同的响应（防枚举）；限流先行，
//     已注册与未注册邮箱同样计入 IP/邮箱桶。重置邮件在响应写出后异步投递，
//     SMTP 延迟不进入响应时序。
//   - 重置 token 是 256-bit CSPRNG（32 字节 base64url），Redis 30 分钟、
//     一次性消费（GET+DEL 原子脚本），明文只出现在发给用户的链接里，
//     绝不写日志。
//   - 改密/重置成功后吊销会话：重置吊销全部；改密保留当前会话并轮换 CSRF
//     （security-01 §2.2「刷新时机」），并写审计事件（不含任何口令材料）。
//   - 口令写入只存 argon2id 哈希（GC-9），并清除 must_change_password
//     （R-02：临时口令在用户自选新口令后自然解除）。
package httpapi

import (
	"context"
	"crypto/rand"
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
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
)

// 冻结常量。阈值唯一来源 modules-08 F-08-16（独立限流桶表）：
// 找回密码 5 次/分钟/IP；改密限流来自 modules-01 F-01-16（5 次/10 分钟/IP+user）。
const (
	// passwordResetTokenBytes：256-bit token（modules-01 F-01-05 冻结）。
	passwordResetTokenBytes = 32
	// passwordResetTokenTTL：重置链接 30 分钟有效（F-01-05 冻结）。
	passwordResetTokenTTL = 30 * time.Minute
	// passwordResetResendAfter：同邮箱重发冷却。设计 UI 文案写 30s，但服务端
	// 口径取 60s（与邮箱验证码 F-01-02/F-08-16 一致）；前端倒计时需按 60s
	// 实现——这是对 modules-01 F-01-05「30s 重发倒计时」的有意收紧，已登记
	// 在任务报告中。
	passwordResetResendAfter = 60 * time.Second
	// passwordResetIPLimit / passwordResetIPWindow：5 次/分钟/IP（F-08-16）。
	passwordResetIPLimit  = 5
	passwordResetIPWindow = time.Minute
	// passwordChangeLimit / passwordChangeWindow：5 次/10 分钟/IP+user（F-01-16）。
	passwordChangeLimit  = 5
	passwordChangeWindow = 10 * time.Minute

	// 重置邮件异步投递：响应路径只做非阻塞入队，SMTP 延迟不参与响应时序
	// （消除「已注册邮箱比未注册慢」的枚举侧信道）。队列满时丢弃并计数。
	passwordResetSendWorkers = 4
	passwordResetSendQueue   = 128
	passwordResetSendTimeout = 30 * time.Second
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
	audit    *audit.Logger
	log      *slog.Logger

	verifyPassword func(hash, plain string) bool
	hashPassword   func(plain string) (string, error)

	// publicBaseURL 是重置链接的固定源（已归一化为单一 scheme://host）。
	// 不能用请求 Host 拼链接：Host 头可伪造，会把重置 token 钓到攻击者域名。
	publicBaseURL string

	// 异步投递队列与分类计数：失败/丢弃只记数量，绝不记录收件地址、
	// 重置 URL 或 sender 错误原文（SMTP 实现可能把链接带进错误串）。
	sendQueue    chan passwordResetJob
	sendFailures atomic.Int64
	sendDropped  atomic.Int64
}

type passwordResetJob struct {
	email string
	link  string
}

func newPasswordHandlers(base *authHandlers, repo PasswordRepo, publicBaseURL string, auditLog *audit.Logger) *passwordHandlers {
	h := &passwordHandlers{
		base:           base,
		repo:           repo,
		rdb:            base.rdb,
		sessions:       base.sessions,
		sender:         base.sender,
		checker:        base.checker,
		audit:          auditLog,
		log:            base.log,
		verifyPassword: base.verifyPassword,
		hashPassword:   base.hashPassword,
		publicBaseURL:  publicBaseURL,
		sendQueue:      make(chan passwordResetJob, passwordResetSendQueue),
	}
	for i := 0; i < passwordResetSendWorkers; i++ {
		go h.resetSendWorker()
	}
	return h
}

// RegisterPasswordRoutes 注册 Task 8 的全部端点。装配方在 NewRouter 之后调用
// 一次；返回装配错误而不是带病运行：
//
//   - publicBaseURL 必须至少包含一个合法的 http(s) origin（可逗号分隔多个，
//     与 RouterConfig.PublicBaseURL 同格式；重置链接取第一个合法项）。
//   - 非 nil 的 EmailSender 必须同时实现 PasswordResetSender；否则重置邮件
//     永远不会发出，而端点仍回 204，属于「静默成功」——装配期直接拒绝。
//   - nil EmailSender 是 M1「未配置 SMTP」的显式状态：启动时告警一次，
//     重置链接不投递但响应保持统一成功态。
func (rt *Router) RegisterPasswordRoutes(publicBaseURL string, auditLog *audit.Logger) error {
	if rt == nil || rt.handlers == nil {
		return errors.New("httpapi: router is not initialized")
	}
	repo, ok := rt.handlers.repo.(PasswordRepo)
	if !ok {
		return errors.New("httpapi: password routes require a PasswordRepo (FindUserByEmail/FindUserByID/UpdatePasswordHash)")
	}
	base, err := passwordResetBaseURL(publicBaseURL)
	if err != nil {
		return err
	}
	if rt.handlers.sender != nil {
		if _, ok := rt.handlers.sender.(PasswordResetSender); !ok {
			return errors.New("httpapi: configured EmailSender must implement PasswordResetSender for password reset links")
		}
	} else {
		rt.handlers.log.Warn("no email sender configured; password reset links will not be delivered (M1 explicit state)")
	}
	h := newPasswordHandlers(rt.handlers, repo, base, auditLog)

	// 公开端点：不挂 CSRF/Origin（无会话可依赖，token/邮箱即凭据）。
	rt.mux.HandleFunc("POST /api/auth/password-reset/request", h.handleResetRequest)
	rt.mux.HandleFunc("POST /api/auth/password-reset/validate", h.handleResetValidate)
	rt.mux.HandleFunc("POST /api/auth/password-reset/confirm", h.handleResetConfirm)
	// 登录态端点：沿用受保护链（SessionAuth → CSRF → Origin）。
	rt.HandleProtected("POST /api/password", http.HandlerFunc(h.handleChangePassword))
	return nil
}

// passwordResetBaseURL 从 PUBLIC_BASE_URL（可逗号分隔多个 origin）取第一个
// 合法项并归一化为 scheme://host。这样链接不会变成 "https://a,https://b/..."。
func passwordResetBaseURL(raw string) (string, error) {
	for _, candidate := range parseAllowedOrigins(raw) {
		if origin, _, ok := normalizeOrigin(candidate); ok {
			return origin, nil
		}
	}
	return "", errors.New("httpapi: PUBLIC_BASE_URL must contain at least one valid http(s) origin")
}

// writeReauthRequired 是 Task 10（换邮箱/删账号）复用的 403 出口。
func writeReauthRequired(w http.ResponseWriter) {
	writeProblem(w, http.StatusForbidden, problemTypeReauthRequired, "Reauthentication required")
}

// writeReauthFailure 把 ReauthGate.Authorize 的错误映射为 HTTP 响应：
// 锁定 → 429 + Retry-After；因子失败 → 403 reauth_required；其余 → 503。
func writeReauthFailure(w http.ResponseWriter, err error) {
	var locked *auth.ReauthLockedError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", retrySeconds(locked.RetryAfter))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests,
			"Too many failed reauthentication attempts")
	case errors.Is(err, auth.ErrReauthRequired):
		writeReauthRequired(w)
	default:
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Reauthentication is temporarily unavailable")
	}
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
		// 先写响应，再非阻塞入队：SMTP 延迟不进入响应时序，且队列满时不
		// 拖慢请求。投递失败只影响用户能否收到邮件，不影响统一成功态。
		w.WriteHeader(http.StatusNoContent)
		h.enqueueResetLink(email, link)
		return
	}
	// 装配期已拒绝空 PUBLIC_BASE_URL；走到这里说明配置在运行期被绕过，
	// 仍保持统一成功态，只记录告警（不含邮箱/链接）。
	h.log.Warn("password reset base URL is empty; reset email not queued")
	w.WriteHeader(http.StatusNoContent)
}

// resetLink 用配置的固定源拼重置链接。publicBaseURL 已在装配期归一化，
// 这里不可能为空（除非被运行期篡改）；空值时返回 ok=false。
func (h *passwordHandlers) resetLink(token string) (string, bool) {
	if h.publicBaseURL == "" {
		return "", false
	}
	return h.publicBaseURL + "/reset-password?token=" + url.QueryEscape(token), true
}

// enqueueResetLink 把投递任务交给有界 worker；队列满时丢弃并计数。
// 非阻塞是刻意设计：绝不因 SMTP 背压改变响应时序。
func (h *passwordHandlers) enqueueResetLink(email, link string) {
	select {
	case h.sendQueue <- passwordResetJob{email: email, link: link}:
	default:
		n := h.sendDropped.Add(1)
		h.log.Error("password reset email queue full; delivery dropped", "dropped_total", n)
	}
}

// resetSendWorker 消费投递队列。错误只记分类计数，不记录原始错误、收件
// 地址或重置链接（三者都可能含敏感材料）。
func (h *passwordHandlers) resetSendWorker() {
	for job := range h.sendQueue {
		sender, ok := h.sender.(PasswordResetSender)
		if !ok {
			// 装配期已校验；nil sender 是 M1 显式状态，不产生日志噪音。
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), passwordResetSendTimeout)
		err := sender.SendPasswordReset(ctx, job.email, job.link)
		cancel()
		if err != nil {
			n := h.sendFailures.Add(1)
			h.log.Error("password reset email delivery failed", "failures_total", n)
		}
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
	ctx := r.Context()
	key := passwordResetKeyPrefix + req.Token

	// 先做「非消费式」token 存在性检查并解析用户，再跑泄露口令筛查：
	// HIBP/上下文词表是有成本、可观测的动作，不能允许未持有效 token 的
	// 匿名请求触发它（也避免对随机 token 做无意义的筛查）。
	uidRaw, err := h.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		h.writeInvalidResetToken(w)
		return
	}
	if err != nil {
		h.log.Error("read password reset token failed", "error", err)
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
	// 每用户上下文（username）参与泄露口令判定：重置路径不能比注册路径更宽。
	if h.checker.CheckWithContext(req.NewPassword, user.Username) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Password is too weak or has been compromised")
		return
	}

	hash, err := h.hashPassword(req.NewPassword)
	if err != nil {
		h.log.Error("hash new password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Password reset failed")
		return
	}

	// 最后才原子消费 token：并发提交同一 token 时只有一个请求能拿到值，
	// 其余得到空（400）；比较值与先前读取一致，防止检查与消费之间被替换。
	consumed, err := consumePasswordResetScript.Run(ctx, h.rdb, []string{key}).Text()
	if errors.Is(err, redis.Nil) {
		h.writeInvalidResetToken(w)
		return
	}
	if err != nil {
		h.log.Error("consume password reset token failed", "error", err)
		h.writeUnavailable(w, "Password reset is temporarily unavailable")
		return
	}
	if strings.TrimSpace(consumed) != strings.TrimSpace(uidRaw) {
		h.writeInvalidResetToken(w)
		return
	}

	// 先吊销再落库：任何 204 都保证旧会话已全部失效；Redis 故障时不改口令。
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
	h.auditEvent(r, audit.ActionAuthPasswordReset, user.ID,
		map[string]string{"action": "password_reset", "field": "password"})
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
	h.auditEvent(r, audit.ActionAuthPasswordChange, user.ID,
		map[string]string{"action": "password_change", "field": "password"})
	w.WriteHeader(http.StatusNoContent)
}

// auditEvent 写一条口令变更审计（F-01-06/F-01-16 的事件）。detail 只含
// 变更字段名，绝不含口令、哈希或 token；审计失败不阻断业务（日志库独立，
// 与 Task 12 的令牌审计同一处理口径）。
func (h *passwordHandlers) auditEvent(r *http.Request, action string, userID int64, detail map[string]string) {
	if h.audit == nil {
		return
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = nil
	}
	event := audit.Event{
		Action:     action,
		Actor:      userID,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: http.StatusNoContent,
		Detail:     raw,
	}
	if err := h.audit.LogEvent(r.Context(), event); err != nil {
		h.log.Warn("password audit write failed", "action", action, "actor", userID, "error", err)
	}
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
