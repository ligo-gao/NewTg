// Package httpapi 的认证端点：注册 / 登录 / 登出 / 邮箱验证码 / 公开状态。
//
// 安全口径（security-01 §2.2/§2.3、security-07 §2.3）：
//   - 会话 Cookie 与服务端 Redis 会话由 Task 3/4 的 auth.SessionManager 负责；
//   - 登录失败计数为 Redis 双桶（IP+identifier / identifier），未知 identifier
//     只按 IP 计分，避免攻击者用任意用户名恶意锁定他人账号；
//   - 认证面限流器故障 fail-closed（503 + 有界 Retry-After），绝不放行爆破；
//   - 邮箱验证码只存 HMAC 哈希、一次性、10 分钟有效，明文只交给发送器。
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
)

// 认证端点的冻结常量。验证码 TTL/限流阈值来自 modules-01 F-01-02 与
// modules-08 F-08-16（唯一真源）；失败锁定阈值来自 modules-01 F-01-03。
const (
	emailCodeTTL         = 10 * time.Minute
	emailCodeResendAfter = 60 * time.Second
	emailCodeIPLimit     = 10
	emailCodeIPWindow    = time.Hour
	emailCodeMaxAttempts = 3

	pending2FATTL = 5 * time.Minute

	// LoginMaxFailures / LoginLockDuration：连续 5 次失败锁 15 分钟。
	LoginMaxFailures   = 5
	LoginLockDuration  = 15 * time.Minute
	maxAuthBodyBytes   = 1 << 20
	maxIdentifierRunes = 254
	maxAffRunes        = 64
)

// RFC 7807 type URI 沿用全库 https://newtg.dev/problems/<slug> 约定。
const (
	authProblemBadRequest         = "https://newtg.dev/problems/bad_request"
	authProblemConflict           = "https://newtg.dev/problems/conflict"
	authProblemForbidden          = "https://newtg.dev/problems/forbidden"
	authProblemAccountDisabled    = "https://newtg.dev/problems/account_disabled"
	authProblemTooManyRequests    = "https://newtg.dev/problems/too_many_requests"
	authProblemServiceUnavailable = "https://newtg.dev/problems/service_unavailable"
	authProblemEmailSendFailed    = "https://newtg.dev/problems/email_send_failed"
	authProblemInternal           = "https://newtg.dev/problems/internal_error"
)

// canonicalLanguages 是 canonical 7 语（modules-01 F-01-19 / platform-01 users.language）。
var canonicalLanguages = []string{"en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"}

var (
	usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)
	emailPattern    = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	sixDigitPattern = regexp.MustCompile(`^[0-9]{6}$`)
)

// ---------------------------------------------------------------------------
// 存储接缝
// ---------------------------------------------------------------------------

// UserRecord 是认证流程需要的用户行投影。nil/空的 PasswordHash 表示
// OAuth 即时注册用户没有口令（platform-01 §1 / R-01-11）。
type UserRecord struct {
	ID                 int64
	Username           string
	Email              string
	PasswordHash       string
	Role               string
	Status             string
	EmailVerified      bool
	TOTPEnabled        bool
	MustChangePassword bool
}

// CreateUserInput 是注册路径的写侧入参。SignupBonus > 0 时由存储层在同一
// 事务内完成「原子 UPDATE balance + wallet_transactions（type=bonus,
// reference_type=signup_grant）」，满足 GC-5 的「每笔余额变动恰一行流水」。
type CreateUserInput struct {
	Username      string
	Email         string
	PasswordHash  string
	Aff           string
	EmailVerified bool
	SignupBonus   int64
}

// CreatedUser 是 INSERT ... RETURNING 的结果：角色/状态取自信赖的 DB 行，
// 绝不取请求体（ruling 3）。
type CreatedUser struct {
	ID     int64
	Role   string
	Status string
}

// ErrDuplicateUser 表示 username 或 email 撞唯一约束。上层必须把它映射成
// 与预检查完全相同的 409 文案，避免字段级枚举信道。
var ErrDuplicateUser = errors.New("httpapi: username or email already exists")

// AuthRepo 是认证端点对 users 表的最小依赖面。单测注入内存假实现；
// 生产实现是 SQLAuthRepo。
type AuthRepo interface {
	FindUserByIdentifier(ctx context.Context, identifier string) (*UserRecord, error)
	UsernameOrEmailTaken(ctx context.Context, username, email string) (bool, error)
	CreateUser(ctx context.Context, in CreateUserInput) (CreatedUser, error)
}

// ConfigReader 是 configcenter.Center.Get 的读取面（便于单测注入）。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// EmailCodeSender 负责把明文验证码交给 SMTP/通知渠道。实现失败时端点
// 返回 5xx 并作废已生成的码；nil 表示未配置邮件（码仍会生成，但不下发）。
type EmailCodeSender interface {
	SendEmailCode(ctx context.Context, email, purpose, code string) error
}

// ---------------------------------------------------------------------------
// SQL 实现
// ---------------------------------------------------------------------------

// SQLAuthRepo 是 AuthRepo 的 PostgreSQL 实现（database/sql + pgx stdlib）。
// 它同时实现 rbac.PermissionLoader：Load 按 UserID 读取 users.admin_permissions
// 的权威值（NULL → nil = FULL）。
type SQLAuthRepo struct {
	db *sql.DB
}

// NewSQLAuthRepo 构造主库仓储。
func NewSQLAuthRepo(db *sql.DB) *SQLAuthRepo { return &SQLAuthRepo{db: db} }

// FindUserByIdentifier 按用户名或邮箱查找用户。邮箱比较大小写不敏感；
// username 精确匹配优先于同形邮箱（避免 identifier 歧义时随机选行）。
func (r *SQLAuthRepo) FindUserByIdentifier(ctx context.Context, identifier string) (*UserRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, username, COALESCE(email, ''), COALESCE(password_hash, ''),
		       role, status, email_verified_at IS NOT NULL, totp_enabled, must_change_password
		FROM users
		WHERE username = $1 OR (email IS NOT NULL AND lower(email) = lower($1))
		ORDER BY (username = $1) DESC
		LIMIT 1`, identifier)
	var u UserRecord
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash,
		&u.Role, &u.Status, &u.EmailVerified, &u.TOTPEnabled, &u.MustChangePassword)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: find user by identifier: %w", err)
	}
	return &u, nil
}

// UsernameOrEmailTaken 是注册前的友好预检查；真正的并发兜底是唯一约束。
func (r *SQLAuthRepo) UsernameOrEmailTaken(ctx context.Context, username, email string) (bool, error) {
	var taken bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM users
			WHERE username = $1 OR (email IS NOT NULL AND lower(email) = lower($2))
		)`, username, email).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("httpapi: check username/email taken: %w", err)
	}
	return taken, nil
}

// CreateUser 在一个事务内完成注册（可选注册赠送）。任何一步失败都整体回滚：
// 不允许出现「用户已建、赠送未发」或「余额已加、流水缺失」的半状态（GC-5）。
func (r *SQLAuthRepo) CreateUser(ctx context.Context, in CreateUserInput) (CreatedUser, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedUser{}, fmt.Errorf("httpapi: begin create user: %w", err)
	}
	// 主错误优先：成功路径显式 Commit；失败路径 Rollback 的错误无需覆盖主错误。
	defer func() { _ = tx.Rollback() }()

	var out CreatedUser
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (username, email, email_verified_at, password_hash, aff)
		VALUES ($1, $2, CASE WHEN $3 THEN now() ELSE NULL END, $4, NULLIF($5, ''))
		RETURNING id, role, status`,
		in.Username, in.Email, in.EmailVerified, in.PasswordHash, in.Aff,
	).Scan(&out.ID, &out.Role, &out.Status)
	if isUniqueViolation(err) {
		return CreatedUser{}, ErrDuplicateUser
	}
	if err != nil {
		return CreatedUser{}, fmt.Errorf("httpapi: insert user: %w", err)
	}

	if in.SignupBonus > 0 {
		var balance int64
		err = tx.QueryRowContext(ctx, `
			UPDATE users SET balance = balance + $1, updated_at = now()
			WHERE id = $2
			RETURNING balance`, in.SignupBonus, out.ID).Scan(&balance)
		if err != nil {
			return CreatedUser{}, fmt.Errorf("httpapi: grant signup bonus: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO wallet_transactions
				(user_id, type, amount, balance_after, status, reference_type, reference_id)
			VALUES ($1, 'bonus', $2, $3, 'confirmed', 'signup_grant', $4)`,
			out.ID, in.SignupBonus, balance, strconv.FormatInt(out.ID, 10))
		if err != nil {
			return CreatedUser{}, fmt.Errorf("httpapi: insert signup bonus transaction: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return CreatedUser{}, fmt.Errorf("httpapi: commit create user: %w", err)
	}
	return out, nil
}

// Load 实现 rbac.PermissionLoader。语义必须与数据库一致：
//
//	(nil, nil)      ← admin_permissions IS NULL（FULL）
//	([]string{}, nil) ← 显式空数组（没有任何权限）
//	error           ← 读取/解析失败，调用方必须 fail-closed
func (r *SQLAuthRepo) Load(ctx context.Context, userID int64) ([]string, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT admin_permissions FROM users WHERE id = $1`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		// 用户已被删除：权限无法确认，必须拒绝而不是退回 FULL。
		return nil, fmt.Errorf("httpapi: load permissions: user %d not found", userID)
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: load permissions for user %d: %w", userID, err)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var perms []string
	if err := json.Unmarshal(raw, &perms); err != nil {
		return nil, fmt.Errorf("httpapi: corrupt admin_permissions for user %d: %w", userID, err)
	}
	return perms, nil
}

// SQLSessionRecorder 把会话审计副本写入 login_sessions。只允许收到
// SHA-256(session id)（I11：明文 session id 绝不落第二个副本）。
type SQLSessionRecorder struct {
	db *sql.DB
}

// NewSQLSessionRecorder 构造 login_sessions 写入器。
func NewSQLSessionRecorder(db *sql.DB) *SQLSessionRecorder { return &SQLSessionRecorder{db: db} }

// RecordSession 幂等写入会话哈希。审计副本失败不影响登录（由 SessionManager 异步调用）。
func (r *SQLSessionRecorder) RecordSession(ctx context.Context, userID int64, sidHash string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO login_sessions (session_id_hash, user_id)
		VALUES ($1, $2)
		ON CONFLICT (session_id_hash) DO NOTHING`, sidHash, userID)
	if err != nil {
		return fmt.Errorf("httpapi: record login session: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------------------------------------------------------------------------
// 登录失败锁定（Redis 双桶）
// ---------------------------------------------------------------------------

// ErrLoginLocked 是锁定哨兵；具体剩余时间用 LockedError.RetryAfter 读取。
var ErrLoginLocked = errors.New("httpapi: login temporarily locked")

// LockedError 携带剩余锁定时间，供 Retry-After 使用。
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("httpapi: login locked for %s", e.RetryAfter)
}

// Unwrap 让 errors.Is(err, ErrLoginLocked) 成立。
func (e *LockedError) Unwrap() error { return ErrLoginLocked }

// LoginLimiter 实现 modules-01 F-01-03 的失败锁定：
//
//   - identifier 非空（已知用户或按用户名探测）：双桶 = IP+identifier 与 identifier；
//   - identifier 为空（未知用户）：只按 IP 计分，防止攻击者用他人用户名恶意锁定。
//
// 计数是连续失败：成功登录清零。Redis 错误原样上抛，调用方必须 fail-closed
// （security-07 §2.3 对认证面冻结为 503 + Retry-After）。
type LoginLimiter struct {
	rdb *redis.Client
}

// NewLoginLimiter 构造限流器。
func NewLoginLimiter(rdb *redis.Client) *LoginLimiter { return &LoginLimiter{rdb: rdb} }

const (
	loginIPKeyPrefix         = "login:fail:ip:"
	loginIPIdentifierPrefix  = "login:fail:ipid:"
	loginIdentifierKeyPrefix = "login:fail:id:"
)

// Check 判断 ip/identifier 是否处于锁定期。identifier 为空时只查 IP 桶。
func (l *LoginLimiter) Check(ctx context.Context, ip, identifier string) error {
	keys := l.buckets(ip, identifier)
	values, err := l.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return fmt.Errorf("httpapi: login limiter check: %w", err)
	}
	var retry time.Duration
	for i, raw := range values {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < LoginMaxFailures {
			continue
		}
		ttl, err := l.rdb.PTTL(ctx, keys[i]).Result()
		if err != nil {
			return fmt.Errorf("httpapi: login limiter ttl: %w", err)
		}
		if ttl <= 0 || ttl > LoginLockDuration {
			// 无 TTL（异常数据）按完整锁定窗口处理，避免永久锁定也避免提前放行。
			ttl = LoginLockDuration
		}
		if ttl > retry {
			retry = ttl
		}
	}
	if retry > 0 {
		return &LockedError{RetryAfter: retry}
	}
	return nil
}

// Record 记录一次登录结果。ok=true 清零两个桶（连续失败语义）；ok=false
// 递增桶并把 TTL 重置为 15 分钟——锁定从第 5 次失败起算。
func (l *LoginLimiter) Record(ctx context.Context, ip, identifier string, ok bool) error {
	keys := l.buckets(ip, identifier)
	if ok {
		if err := l.rdb.Del(ctx, keys...).Err(); err != nil {
			return fmt.Errorf("httpapi: login limiter reset: %w", err)
		}
		return nil
	}
	_, err := loginFailureScript.Run(ctx, l.rdb, keys,
		strconv.FormatInt(LoginLockDuration.Milliseconds(), 10)).Result()
	if err != nil {
		return fmt.Errorf("httpapi: login limiter record: %w", err)
	}
	return nil
}

// buckets 返回本次应检查/计数的 Redis 键。未知 identifier（空串）只返回 IP 桶。
func (l *LoginLimiter) buckets(ip, identifier string) []string {
	ip = strings.TrimSpace(ip)
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if identifier == "" {
		return []string{loginIPKeyPrefix + sha256Hex(ip)}
	}
	return []string{
		loginIPIdentifierPrefix + sha256Hex(ip+"\x00"+identifier),
		loginIdentifierKeyPrefix + loginIdentifierHash(identifier),
	}
}

// loginIdentifierHash 对 identifier 做小写归一后 SHA-256，避免把用户名/邮箱
// 原文写进 Redis 键（PII 最小化），同时保证大小写变体共享同一锁定桶。
func loginIdentifierHash(identifier string) string {
	return sha256Hex(strings.ToLower(strings.TrimSpace(identifier)))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// loginFailureScript 对每个桶 INCR 并重置 15 分钟 TTL。多键在单个 Lua 脚本
// 内完成，跨副本计数一致（security-07 S5/TC-G-14）。
var loginFailureScript = redis.NewScript(`
local ttl = tonumber(ARGV[1])
for i = 1, #KEYS do
  redis.call('INCR', KEYS[i])
  redis.call('PEXPIRE', KEYS[i], ttl)
end
return 1
`)

// ---------------------------------------------------------------------------
// 邮箱验证码
// ---------------------------------------------------------------------------

// emailCodeStore 用 Redis 保存一次性验证码。只存
// HMAC-SHA256(SESSION_SECRET, purpose|email|code)，Redis 泄露时无法反推出
// 可提交的 6 位码。
type emailCodeStore struct {
	rdb    *redis.Client
	secret []byte
}

func newEmailCodeStore(rdb *redis.Client, secret []byte) *emailCodeStore {
	if len(secret) == 0 {
		// 没有 SESSION_SECRET 时退化为进程内随机密钥：验证码仍不可离线反推，
		// 但跨副本/重启无法互认。生产装配必须提供 SESSION_SECRET。
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			panic("httpapi: crypto/rand failed: " + err.Error())
		}
		secret = buf
	}
	return &emailCodeStore{rdb: rdb, secret: secret}
}

func (s *emailCodeStore) codeKey(email, purpose string) string {
	return "email_code:" + purpose + ":" + email
}

func (s *emailCodeStore) attemptsKey(email, purpose string) string {
	return s.codeKey(email, purpose) + ":attempts"
}

func (s *emailCodeStore) hash(email, purpose, code string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(purpose + "\x00" + strings.ToLower(email) + "\x00" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// issue 生成 6 位数字码并写入哈希，返回明文供发送器使用（绝不落日志）。
func (s *emailCodeStore) issue(ctx context.Context, email, purpose string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("httpapi: generate email code: %w", err)
	}
	code := fmt.Sprintf("%06d", n.Int64())
	key := s.codeKey(email, purpose)
	if err := s.rdb.Set(ctx, key, s.hash(email, purpose, code), emailCodeTTL).Err(); err != nil {
		return "", fmt.Errorf("httpapi: store email code: %w", err)
	}
	if err := s.rdb.Del(ctx, s.attemptsKey(email, purpose)).Err(); err != nil {
		return "", fmt.Errorf("httpapi: reset email code attempts: %w", err)
	}
	return code, nil
}

// consume 原子校验并删除验证码（一次性）。错误尝试累计 3 次后整码作废
// （modules-01 TC-F-01-01-04）。
func (s *emailCodeStore) consume(ctx context.Context, email, purpose, code string) (bool, error) {
	ok, err := consumeEmailCodeScript.Run(ctx, s.rdb,
		[]string{s.codeKey(email, purpose), s.attemptsKey(email, purpose)},
		s.hash(email, purpose, code),
		strconv.Itoa(int(emailCodeTTL.Seconds())),
		strconv.Itoa(emailCodeMaxAttempts),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("httpapi: consume email code: %w", err)
	}
	return ok == 1, nil
}

// discard 作废验证码（发送失败时调用）。
func (s *emailCodeStore) discard(ctx context.Context, email, purpose string) error {
	return s.rdb.Del(ctx, s.codeKey(email, purpose), s.attemptsKey(email, purpose)).Err()
}

var consumeEmailCodeScript = redis.NewScript(`
local stored = redis.call('GET', KEYS[1])
if not stored then
  return 0
end
if stored == ARGV[1] then
  redis.call('DEL', KEYS[1], KEYS[2])
  return 1
end
local n = redis.call('INCR', KEYS[2])
if n == 1 then
  redis.call('EXPIRE', KEYS[2], ARGV[2])
end
if n >= tonumber(ARGV[3]) then
  redis.call('DEL', KEYS[1], KEYS[2])
end
return 0
`)

// emailIPLimitScript 以原子方式做「INCR + 首次设置过期」，避免 INCR 成功而
// EXPIRE 失败导致计数器永久不过期。
var emailIPLimitScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return n
`)

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type authHandlers struct {
	repo           AuthRepo
	sessions       *auth.SessionManager
	rdb            *redis.Client
	limiter        *LoginLimiter
	codes          *emailCodeStore
	config         ConfigReader
	sender         EmailCodeSender
	checker        auth.CompromisedPasswordChecker
	log            *slog.Logger
	verifyPassword func(hash, plain string) bool
	hashPassword   func(plain string) (string, error)
}

func newAuthHandlers(repo AuthRepo, sessions *auth.SessionManager, rdb *redis.Client,
	config ConfigReader, sessionSecret []byte, sender EmailCodeSender,
	checker auth.CompromisedPasswordChecker, log *slog.Logger) *authHandlers {
	if log == nil {
		log = slog.Default()
	}
	return &authHandlers{
		repo:           repo,
		sessions:       sessions,
		rdb:            rdb,
		limiter:        NewLoginLimiter(rdb),
		codes:          newEmailCodeStore(rdb, sessionSecret),
		config:         config,
		sender:         sender,
		checker:        checker,
		log:            log,
		verifyPassword: auth.VerifyPassword,
		hashPassword:   auth.HashPassword,
	}
}

// statusResponse 是 GET /api/status 的公开投影（modules-08 §1.4.1）。
// 绝不包含任何密钥类字段（turnstile.secret_key / auth.login_password_encryption.*）。
type statusResponse struct {
	Auth                           statusAuth `json:"auth"`
	PasswordLoginEncryptionEnabled bool       `json:"password_login_encryption_enabled"`
	Languages                      []string   `json:"languages"`
	DefaultLanguage                string     `json:"default_language"`
}

type statusAuth struct {
	SignupEnabled             bool            `json:"signup_enabled"`
	PasswordLoginEnabled      bool            `json:"password_login_enabled"`
	EmailVerificationRequired bool            `json:"email_verification_required"`
	Turnstile                 statusTurnstile `json:"turnstile"`
}

type statusTurnstile struct {
	Enabled bool   `json:"enabled"`
	SiteKey string `json:"site_key"`
}

func (h *authHandlers) handleStatus(w http.ResponseWriter, _ *http.Request) {
	langs := make([]string, len(canonicalLanguages))
	copy(langs, canonicalLanguages)
	// 站点开关变更要求即时反映到登录/注册页，禁止中间层缓存公开状态。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, statusResponse{
		Auth: statusAuth{
			SignupEnabled:             h.boolConfig("auth.signup_enabled", true),
			PasswordLoginEnabled:      h.boolConfig("auth.password_login_enabled", true),
			EmailVerificationRequired: h.boolConfig("auth.email_verification_required", false),
			Turnstile: statusTurnstile{
				Enabled: h.boolConfig("auth.turnstile.enabled", false),
				SiteKey: h.stringConfig("auth.turnstile.site_key", ""),
			},
		},
		PasswordLoginEncryptionEnabled: h.boolConfig("auth.login_password_encryption.enabled", false),
		Languages:                      langs,
		DefaultLanguage:                "en",
	})
}

type emailCodeRequest struct {
	Email   string `json:"email"`
	Purpose string `json:"purpose"`
}

func (h *authHandlers) handleEmailCode(w http.ResponseWriter, r *http.Request) {
	var req emailCodeRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address")
		return
	}
	if !validEmailCodePurpose(req.Purpose) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid verification purpose")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)

	// 同邮箱 1 次/60s（modules-08 F-08-16）。先占用冷却键：即使后续 IP 桶
	// 拒绝或 SMTP 失败，也保持 60s 冷却，防止重试风暴。
	cooldownKey := "email_code:cooldown:" + email
	acquired, err := h.rdb.SetNX(ctx, cooldownKey, "1", emailCodeResendAfter).Result()
	if err != nil {
		h.writeServiceUnavailable(w, "Email verification is temporarily unavailable")
		return
	}
	if !acquired {
		retry := h.ttlOr(ctx, cooldownKey, emailCodeResendAfter)
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Please wait before requesting another code")
		return
	}

	// 同 IP 10 次/小时。
	ipKey := "email_code:ip:" + sha256Hex(ip)
	n, err := emailIPLimitScript.Run(ctx, h.rdb, []string{ipKey},
		strconv.Itoa(int(emailCodeIPWindow.Seconds()))).Int64()
	if err != nil {
		h.writeServiceUnavailable(w, "Email verification is temporarily unavailable")
		return
	}
	if n > emailCodeIPLimit {
		retry := h.ttlOr(ctx, ipKey, emailCodeIPWindow)
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many verification code requests")
		return
	}

	code, err := h.codes.issue(ctx, email, req.Purpose)
	if err != nil {
		h.log.Error("issue email code failed", "error", err)
		h.writeServiceUnavailable(w, "Email verification is temporarily unavailable")
		return
	}
	if h.sender != nil {
		if err := h.sender.SendEmailCode(ctx, email, req.Purpose, code); err != nil {
			// 明文码只交给发送器；失败即作废，避免留下一个用户永远收不到的有效码。
			_ = h.codes.discard(ctx, email, req.Purpose)
			h.log.Error("send email code failed", "purpose", req.Purpose, "error", err)
			writeProblem(w, http.StatusBadGateway, authProblemEmailSendFailed, "Failed to send verification email")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type registerRequest struct {
	Username        string `json:"username"`
	Email           string `json:"email"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirmPassword"`
	EmailCode       string `json:"email_code"`
	AgreeTerms      bool   `json:"agree_terms"`
	Aff             string `json:"aff"`
}

func (h *authHandlers) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !h.boolConfig("auth.signup_enabled", true) {
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Registration is disabled")
		return
	}
	var req registerRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}

	username := strings.TrimSpace(req.Username)
	switch {
	case !usernamePattern.MatchString(username):
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid username")
		return
	case !auth.ValidatePasswordLength(req.Password):
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Password must be between 8 and 128 characters")
		return
	case req.Password != req.ConfirmPassword:
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Passwords do not match")
		return
	case !req.AgreeTerms:
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Terms must be accepted")
		return
	}
	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address")
		return
	}
	aff := strings.TrimSpace(req.Aff)
	if utf8.RuneCountInString(aff) > maxAffRunes {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid affiliate code")
		return
	}
	// 泄露口令筛查（security-01 §2.3）：长度合格但已泄露/上下文可猜的口令
	// 同样必须拒绝。用户名作为每用户上下文传入。
	if h.checker.CheckWithContext(req.Password, username) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Password is too weak or has been compromised")
		return
	}

	emailVerified := false
	if h.boolConfig("auth.email_verification_required", false) {
		if !sixDigitPattern.MatchString(req.EmailCode) {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
			return
		}
		ok, err := h.codes.consume(r.Context(), email, "register", req.EmailCode)
		if err != nil {
			h.writeServiceUnavailable(w, "Registration is temporarily unavailable")
			return
		}
		if !ok {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
			return
		}
		emailVerified = true
	}

	// 预检查给出友好 409；并发竞态由 CreateUser 的唯一约束兜底，两者响应必须同形。
	taken, err := h.repo.UsernameOrEmailTaken(r.Context(), username, email)
	if err != nil {
		h.log.Error("check duplicate user failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}
	if taken {
		h.writeDuplicateUser(w)
		return
	}

	passwordHash, err := h.hashPassword(req.Password)
	if err != nil {
		h.log.Error("hash password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}

	created, err := h.repo.CreateUser(r.Context(), CreateUserInput{
		Username:      username,
		Email:         email,
		PasswordHash:  passwordHash,
		Aff:           aff,
		EmailVerified: emailVerified,
		SignupBonus:   h.signupBonus(emailVerified),
	})
	if errors.Is(err, ErrDuplicateUser) {
		h.writeDuplicateUser(w)
		return
	}
	if err != nil {
		h.log.Error("create user failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}
	if created.Status != "" && created.Status != "enabled" {
		// DB 默认应为 enabled；非 enabled 说明站点策略/数据异常，拒绝建会话。
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Account is not available")
		return
	}

	user := &UserRecord{
		ID:            created.ID,
		Username:      username,
		Email:         email,
		Role:          created.Role,
		Status:        created.Status,
		EmailVerified: emailVerified,
	}
	if err := h.startSession(r.Context(), w, r, user); err != nil {
		h.log.Error("create session failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}
	writeJSON(w, http.StatusCreated, authSessionResponse{User: userView(user)})
}

// signupBonus 读取站点注册赠送配置。默认 0（不赠送）；赠送额为正数且
// （未要求已验证邮箱 或 邮箱已验证）时返回金额。
func (h *authHandlers) signupBonus(emailVerified bool) int64 {
	bonus := h.int64Config("billing.signup_grant_credits", 0)
	if bonus <= 0 {
		return 0
	}
	if h.boolConfig("billing.signup_grant_requires_verified_email", false) && !emailVerified {
		return 0
	}
	return bonus
}

type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

func (h *authHandlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.boolConfig("auth.password_login_enabled", true) {
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Password login is disabled")
		return
	}
	var req loginRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" || utf8.RuneCountInString(identifier) > maxIdentifierRunes {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid identifier")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)

	// 锁定预检查：已知 identifier 的双桶在此拦截，避免锁定期间仍消耗 DB/argon2。
	if err := h.limiter.Check(ctx, ip, identifier); err != nil {
		h.writeLimiterFailure(w, err)
		return
	}

	user, err := h.repo.FindUserByIdentifier(ctx, identifier)
	if err != nil {
		h.log.Error("find user failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Login failed")
		return
	}
	if user == nil {
		// 未知 identifier 只按 IP 计分：否则攻击者可用他人用户名把账号锁死。
		if err := h.limiter.Check(ctx, ip, ""); err != nil {
			h.writeLimiterFailure(w, err)
			return
		}
		// dummy verify：与真实用户同等的 argon2 成本，消除「查无此人」的时序特征。
		_ = h.verifyPassword(dummyPasswordHash(), req.Password)
		if err := h.limiter.Record(ctx, ip, "", false); err != nil {
			h.writeLimiterFailure(w, err)
			return
		}
		writeUnauthorized(w) // 与错误口令逐字相同
		return
	}

	// OAuth 即时注册用户没有口令：仍执行 dummy verify 保持时序，再按失败计数。
	if user.PasswordHash == "" {
		_ = h.verifyPassword(dummyPasswordHash(), req.Password)
		if err := h.limiter.Record(ctx, ip, identifier, false); err != nil {
			h.writeLimiterFailure(w, err)
			return
		}
		writeUnauthorized(w)
		return
	}
	if !h.verifyPassword(user.PasswordHash, req.Password) {
		if err := h.limiter.Record(ctx, ip, identifier, false); err != nil {
			h.writeLimiterFailure(w, err)
			return
		}
		writeUnauthorized(w)
		return
	}

	// 口令正确后才检查封禁：错误口令不得成为账号状态探测信道。
	if user.Status != "enabled" {
		writeProblem(w, http.StatusUnauthorized, authProblemAccountDisabled, "This account has been disabled")
		return
	}

	// 连续失败计数清零。Reset 失败时保持保守：下一次失败可能提前锁定，
	// 但会话创建同样依赖 Redis，会 fail-closed。
	_ = h.limiter.Record(ctx, ip, identifier, true)

	if user.TOTPEnabled {
		ticket, err := h.createPending2FA(ctx, user.ID)
		if err != nil {
			h.log.Error("create pending 2fa ticket failed", "error", err)
			h.writeServiceUnavailable(w, "Login is temporarily unavailable")
			return
		}
		writeJSON(w, http.StatusOK, pending2FAResponse{Status: "pending_2fa", Ticket: ticket})
		return
	}

	if err := h.startSession(ctx, w, r, user); err != nil {
		h.log.Error("create session failed", "error", err)
		h.writeServiceUnavailable(w, "Login is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, authSessionResponse{User: userView(user)})
}

func (h *authHandlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	// 登出是 public-safe 的：无会话/重复登出都必须 204；也不做 CSRF 校验
	// （security-01 §2.2：登出不构成状态提升，且清 Cookie 必须在会话失效后仍可用）。
	if sid, ok := sessionID(r); ok {
		if err := h.sessions.Revoke(r.Context(), sid); err != nil {
			h.log.Error("revoke session failed", "error", err)
			h.writeServiceUnavailable(w, "Logout is temporarily unavailable")
			return
		}
	}
	clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 会话与响应辅助
// ---------------------------------------------------------------------------

type authUserView struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	Email              string `json:"email,omitempty"`
	Role               string `json:"role"`
	EmailVerified      bool   `json:"email_verified"`
	MustChangePassword bool   `json:"must_change_password"`
}

type authSessionResponse struct {
	User authUserView `json:"user"`
}

type pending2FAResponse struct {
	Status string `json:"status"`
	Ticket string `json:"ticket"`
}

func userView(u *UserRecord) authUserView {
	return authUserView{
		ID:                 u.ID,
		Username:           u.Username,
		Email:              u.Email,
		Role:               u.Role,
		EmailVerified:      u.EmailVerified,
		MustChangePassword: u.MustChangePassword,
	}
}

// startSession 创建 Redis 会话并签发 CSRF 令牌，然后写两个 Cookie（security-01 §2.2）：
// 会话 HttpOnly（JS 不可读），CSRF 非 HttpOnly（前端读出后写 X-CSRF-Token）。
func (h *authHandlers) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, user *UserRecord) error {
	sid, err := h.sessions.Create(ctx, user.ID, auth.SessionOpts{
		UA:   r.UserAgent(),
		IP:   clientIP(r),
		Role: user.Role, // 角色只能来自 DB 行（ruling 3），绝不读请求体
	})
	if err != nil {
		return err
	}
	token := auth.NewCSRFToken()
	if err := h.sessions.IssueCSRF(ctx, sid, token); err != nil {
		// 半建状态必须清理，避免留下没有 CSRF 的会话。
		_ = h.sessions.Revoke(ctx, sid)
		return err
	}
	setSessionCookies(w, sid, token)
	return nil
}

func setSessionCookies(w http.ResponseWriter, sid, csrfToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		HttpOnly: false, // 前端必须能读取后写入 X-CSRF-Token
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(1, 0),
			HttpOnly: name == auth.SessionCookieName,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// createPending2FA 写 pending_2fa:{ticket}（5 分钟），不建立正式会话。
func (h *authHandlers) createPending2FA(ctx context.Context, userID int64) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("httpapi: generate 2fa ticket: %w", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)
	if err := h.rdb.Set(ctx, "pending_2fa:"+ticket,
		strconv.FormatInt(userID, 10), pending2FATTL).Err(); err != nil {
		return "", fmt.Errorf("httpapi: store 2fa ticket: %w", err)
	}
	return ticket, nil
}

// dummyPasswordHash 是未知用户/无口令用户登录时用于时序对齐的哈希。
// sync.OnceValue 保证只付一次 KDF 成本；CSPRNG 失败属灾难状态，panic 让
// 请求 500 fail-closed，而不是退化成零成本的快速失败。
var dummyPasswordHash = sync.OnceValue(func() string {
	hash, err := auth.HashPassword("newtg-dummy-password-value")
	if err != nil {
		panic("httpapi: generate dummy password hash: " + err.Error())
	}
	return hash
})

func (h *authHandlers) writeDuplicateUser(w http.ResponseWriter) {
	// 重名与重邮箱必须同一个响应：任何字段级差异都会变成注册枚举接口。
	writeProblem(w, http.StatusConflict, authProblemConflict, "Username or email is already in use")
}

func (h *authHandlers) writeLimiterFailure(w http.ResponseWriter, err error) {
	var locked *LockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", retrySeconds(locked.RetryAfter))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many failed login attempts")
		return
	}
	// security-07 §2.3：认证面 Redis 故障必须 fail-closed，拒绝有界（Retry-After）。
	h.log.Error("login limiter unavailable", "error", err)
	h.writeServiceUnavailable(w, "Login is temporarily unavailable")
}

func (h *authHandlers) writeServiceUnavailable(w http.ResponseWriter, title string) {
	w.Header().Set("Retry-After", "5")
	writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable, title)
}

// ttlOr 读取键的剩余 TTL；异常值回落到完整窗口（有界重试提示）。
func (h *authHandlers) ttlOr(ctx context.Context, key string, fallback time.Duration) time.Duration {
	ttl, err := h.rdb.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 || ttl > fallback {
		return fallback
	}
	return ttl
}

// Retry-After 取整秒（向上取整，避免 0）。
func retrySeconds(d time.Duration) string {
	seconds := int64((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

// ---------------------------------------------------------------------------
// 输入/配置辅助
// ---------------------------------------------------------------------------

func readAuthJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBodyBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxAuthBodyBytes {
		return errors.New("request body too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(raw, dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientIP 只取连接地址：X-Forwarded-For 在未配置可信代理时可由客户端伪造，
// 用它做登录限流等于把攻击者可控值放进风控键（security-07）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validEmail(email string) bool {
	return utf8.RuneCountInString(email) <= 254 && emailPattern.MatchString(email)
}

func validEmailCodePurpose(purpose string) bool {
	switch purpose {
	case "register", "reset", "verify", "bind":
		return true
	default:
		return false
	}
}

func (h *authHandlers) readConfig(key string) (json.RawMessage, bool) {
	if h.config == nil {
		return nil, false
	}
	return h.config.Get(key)
}

func (h *authHandlers) boolConfig(key string, def bool) bool {
	raw, ok := h.readConfig(key)
	if !ok {
		return def
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

func (h *authHandlers) int64Config(key string, def int64) int64 {
	raw, ok := h.readConfig(key)
	if !ok {
		return def
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

func (h *authHandlers) stringConfig(key, def string) string {
	raw, ok := h.readConfig(key)
	if !ok {
		return def
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}
