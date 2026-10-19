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
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
	"newtg/gateway/internal/store"
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

	// 注册赠送反滥用（modules-08 F-08-16 / security-07 R-07-10）：
	// 同 IP 24h 内至多 3 个账号实际领赠；超出时注册照常成功但不发放。
	// 设备维度（签名 Cookie nkt_did）尚未落地，留 seam 见 reserveSignupGrantSlot。
	signupGrantIPLimit  = 3
	signupGrantIPWindow = 24 * time.Hour
)

// RFC 7807 type URI 沿用全库 https://newtg.dev/problems/<slug> 约定。
const (
	authProblemBadRequest          = "https://newtg.dev/problems/bad_request"
	authProblemConflict            = "https://newtg.dev/problems/conflict"
	authProblemForbidden           = "https://newtg.dev/problems/forbidden"
	authProblemAccountDisabled     = "https://newtg.dev/problems/account_disabled"
	authProblemTooManyRequests     = "https://newtg.dev/problems/too_many_requests"
	authProblemServiceUnavailable  = "https://newtg.dev/problems/service_unavailable"
	authProblemEmailSendFailed     = "https://newtg.dev/problems/email_send_failed"
	authProblemEmailDomainDenied   = "https://newtg.dev/problems/email_domain_not_allowed"
	authProblemTempPasswordExpired = "https://newtg.dev/problems/temp_password_expired"
	authProblemInternal            = "https://newtg.dev/problems/internal_error"
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
//	(nil, nil)      ← role=admin 且 admin_permissions IS NULL（FULL）
//	([]string{}, nil) ← 显式空数组，或 role=user（无论列里残留什么）
//	error           ← 读取/解析失败，调用方必须 fail-closed
//
// role 与权限列在同一行读取：降权后旧会话里的 Role=admin 仍是登录时的快照，
// 若只按 admin_permissions 判定，NULL 会被映射成 FULL，被降权的管理员可以
// 靠旧会话继续拥有全部权限（F-07-06 / IS-B-5）。users.role 是权限真源的一部分。
func (r *SQLAuthRepo) Load(ctx context.Context, userID int64) ([]string, error) {
	var (
		raw  []byte
		role string
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT admin_permissions, role FROM users WHERE id = $1`, userID).Scan(&raw, &role)
	if errors.Is(err, sql.ErrNoRows) {
		// 用户已被删除：权限无法确认，必须拒绝而不是退回 FULL。
		return nil, fmt.Errorf("httpapi: load permissions: user %d not found", userID)
	}
	if err != nil {
		return nil, fmt.Errorf("httpapi: load permissions for user %d: %w", userID, err)
	}
	if role != rbac.RoleAdmin {
		// 非 admin 行永远不是 FULL：显式空集让残留会话无法借 NULL 取得权限。
		return []string{}, nil
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
		switch ttl {
		case -2:
			// 键在 MGET 与 PTTL 之间过期/被删除：计数已不存在，视为未锁定
			// （不能用一个刚消失的桶把人锁 15 分钟）。
			continue
		case -1:
			// 键存在但无 TTL（异常数据）：按完整锁定窗口兜底，避免永久锁定。
			ttl = LoginLockDuration
		default:
			if ttl <= 0 || ttl > LoginLockDuration {
				ttl = LoginLockDuration
			}
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
	// audit 为 nil 时不写审计（未装配日志库的测试/开发环境）。
	audit *audit.Logger
	// passwordEncryption 是登录前端加密（F-01-25）的解密接缝；nil 表示
	// 开关关闭且不接受加密载荷（装配方未提供时为 M1 的明文登录状态）。
	passwordEncryption *PasswordEncryption
	// turnstile 是按接入点区分的 handler 内人机校验；nil 表示未配置。
	// 它必须在速率桶之后调用（限流外层 → Turnstile 内层，modules-01 §3.15）。
	turnstile *turnstileGates
	// signupGrantAlert 是赠送被反滥用上限抑制时的告警钩子；nil 时仅落 Warn 日志。
	signupGrantAlert func(ctx context.Context, userID int64, ip string)
	// reserveSignupGrant 是 Redis 槽位预留的可替换接缝（测试注入故障）。
	reserveSignupGrant func(ctx context.Context, ip string) (bool, error)
	// initialized 查询安装向导是否已完成。GET /api/status 用它暴露
	// system_initialized（前端初始化闸门）；nil 表示未接线的单元装配
	// （生产装配由 BuildGatewayRouter 传入 SetupStore.IsInitialized）。
	initialized func(ctx context.Context) (bool, error)
	// geoIP 是本地 GeoIP 库接缝；nil 表示库未配置，/api/geoip 返回 unknown。
	geoIP GeoIPLookup
	// usableGroups 是 GET /api/status 的 usable_groups 数据源（Task 13）。
	// nil 时该字段恒为空数组（未接线的单元装配）。
	usableGroups UsableGroupLookup
}

func newAuthHandlers(repo AuthRepo, sessions *auth.SessionManager, rdb *redis.Client,
	config ConfigReader, sessionSecret []byte, sender EmailCodeSender,
	checker auth.CompromisedPasswordChecker, log *slog.Logger) *authHandlers {
	if log == nil {
		log = slog.Default()
	}
	h := &authHandlers{
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
	h.reserveSignupGrant = h.reserveSignupGrantSlot
	return h
}

// statusResponse 是 GET /api/status 的公开投影（modules-08 §1.4.1）。
// 绝不包含任何密钥类字段（turnstile.secret_key / auth.login_password_encryption.*）。
//
// site/nav/billing/pay 由注册表中 public:true 的键自动投影（settings.PublicSnapshot），
// 新增公开键只需改注册表，不会出现「注册了但没下发」的第二份清单。
type statusResponse struct {
	Auth                           statusAuth     `json:"auth"`
	Site                           map[string]any `json:"site"`
	Nav                            map[string]any `json:"nav"`
	Billing                        map[string]any `json:"billing"`
	Pay                            map[string]any `json:"pay"`
	PasswordLoginEncryptionEnabled bool           `json:"password_login_encryption_enabled"`
	SystemInitialized              bool           `json:"system_initialized"`
	Languages                      []string       `json:"languages"`
	DefaultLanguage                string         `json:"default_language"`
	// 派生 / 常量字段（modules-08 §1.4.1 末行）：不写入 system_settings。
	// usd_exchange_rate 的常量真源是 billing-02 §4.1（7.1 = 71/10）。
	USDExchangeRate     float64 `json:"usd_exchange_rate"`
	CreditDisplaySymbol string  `json:"credit_display_symbol"`
	// UsableGroups 只在请求携带有效会话时非空（Modules-02 §2.14 / Task 13
	// 冻结形状）。匿名请求恒为空数组，不读用户分组、也不返回 401。
	UsableGroups []usableGroupView `json:"usable_groups"`
}

// usableGroupView 是 GET /api/status 的 usable_groups 元素（Task 13 冻结）。
type usableGroupView struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Ratio       int32    `json:"ratio"`
	Models      []string `json:"models"`
}

// UsableGroupLookup 是 usable_groups 的查询接缝（production: *store.UserGroupsRepo；
// 单测可注入内存实现）。
type UsableGroupLookup interface {
	UsableGroupsForUser(ctx context.Context, userID int64) ([]store.UsableGroup, error)
}

// statusUsableGroups 解析会话并返回该用户的可用分组。
//
// 公开面语义（R-PUB-05 同口径）：无会话/会话失效/Redis 故障/查询失败一律
// 降级为空数组，绝不 401、绝不 5xx——登录页的 status 探测不能因为个性化
// 增量数据不可用而失效。这里不 Touch 会话：/api/status 会被匿名流量高频
// 访问，替用户续期等于让第三方流量决定会话寿命。
func (h *authHandlers) statusUsableGroups(r *http.Request) []usableGroupView {
	empty := []usableGroupView{}
	if h.sessions == nil || h.usableGroups == nil {
		return empty
	}
	sid, ok := sessionID(r)
	if !ok {
		return empty
	}
	sess, err := h.sessions.Get(r.Context(), sid)
	if err != nil || sess == nil || sess.UserID == 0 {
		return empty
	}
	groups, err := h.usableGroups.UsableGroupsForUser(r.Context(), sess.UserID)
	if err != nil {
		// 不返回错误：个性化分组清单是公开状态的增量，查询失败只影响该字段。
		h.log.Warn("status: load usable groups failed", "user_id", sess.UserID, "error", err)
		return empty
	}
	views := make([]usableGroupView, 0, len(groups))
	for _, group := range groups {
		models := group.Models
		if models == nil {
			// JSON null 与空数组在模型选择器上语义不同：null 像「未知」，
			// [] 才是「该分组当前无可用模型」。
			models = []string{}
		}
		views = append(views, usableGroupView{
			Name:        group.Name,
			Description: group.Description,
			Ratio:       group.Ratio,
			Models:      models,
		})
	}
	return views
}

type statusAuth struct {
	SignupEnabled             bool            `json:"signup_enabled"`
	PasswordLoginEnabled      bool            `json:"password_login_enabled"`
	EmailVerificationRequired bool            `json:"email_verification_required"`
	Turnstile                 statusTurnstile `json:"turnstile"`
	Passkey                   statusPasskey   `json:"passkey"`
}

type statusTurnstile struct {
	Enabled bool   `json:"enabled"`
	SiteKey string `json:"site_key"`
}

type statusPasskey struct {
	Enabled bool `json:"enabled"`
}

func (h *authHandlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	// B8 公开只读面 60 次/分钟/IP+path（security-07 §2.1，冻结值）。
	// 公开面按 §2.3 fail-open：Redis 故障时放行并告警，绝不因限流器故障
	// 让登录/注册页的 status 探测整体不可用。
	ip := clientIP(r)
	over, retry, err := h.allowRequestRate(r.Context(),
		PublicReadRateLimitKey(ip, r.URL.Path), PublicReadRateLimit, PublicReadRateWindow)
	if err != nil {
		h.log.Warn("status request rate limiter unavailable; failing open", "error", err)
	} else if over {
		writeRateLimited(w, retry, "Too many requests")
		return
	}

	langs := make([]string, len(canonicalLanguages))
	copy(langs, canonicalLanguages)
	// 初始化状态是前端 /setup 全局守卫的唯一依据，查询失败必须 503 而不是
	// 猜 false（猜 false 会把已初始化站点重新导向安装向导，猜 true 会把新站
	// 锁死在登录页）。nil seam 只出现在未接线的单元装配，按未初始化处理。
	initialized := false
	if h.initialized != nil {
		value, err := h.initialized(r.Context())
		if err != nil {
			h.log.Error("status: read initialization state failed", "error", err)
			h.writeServiceUnavailable(w, "Status is temporarily unavailable")
			return
		}
		initialized = value
	}
	// 关闭 Turnstile 时 site_key 必须为空串（契约 schemas/public.yaml）：
	// 残留的 site_key 会让前端误渲染一个人机校验组件并提交无效令牌。
	turnstileEnabled := h.boolConfig("auth.turnstile.enabled", false)
	siteKey := ""
	if turnstileEnabled {
		siteKey = h.stringConfig("auth.turnstile.site_key", "")
	}
	// 公开键全集来自注册表（§1.4.1 单一真源）：site/nav/billing/pay 按域投影，
	// 缺值自动回落注册表默认值。auth 区保留既有窄结构以维持向后兼容。
	public := settings.PublicSnapshot(h.config)
	site := settings.DomainValues(public, "site")
	// R-PUB-07：自定义首页 HTML 只在开关打开时下发。此前无条件下发会让
	// home_custom_enabled=false 的站点仍把整段 HTML（可能很大且含运营文案）
	// 发给所有访客；开关关闭时前端也不应收到这段「已失效」的内容。
	// 用投影值判断（而不是再次读配置），保证与下发值来自同一快照。
	if enabled, _ := site["home_custom_enabled"].(bool); !enabled {
		delete(site, "home_custom_html")
	}
	// 站点开关变更要求即时反映到登录/注册页，禁止中间层缓存公开状态。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, statusResponse{
		Auth: statusAuth{
			SignupEnabled:             h.boolConfig("auth.signup_enabled", true),
			PasswordLoginEnabled:      h.boolConfig("auth.password_login_enabled", true),
			EmailVerificationRequired: h.boolConfig("auth.email_verification_required", false),
			Turnstile: statusTurnstile{
				Enabled: turnstileEnabled,
				SiteKey: siteKey,
			},
			Passkey: statusPasskey{Enabled: publicBool(public, "auth.passkey.enabled")},
		},
		Site:                           site,
		Nav:                            settings.DomainValues(public, "nav"),
		Billing:                        settings.DomainValues(public, "billing"),
		Pay:                            settings.DomainValues(public, "pay"),
		PasswordLoginEncryptionEnabled: h.boolConfig("auth.login_password_encryption.enabled", false),
		SystemInitialized:              initialized,
		Languages:                      langs,
		DefaultLanguage:                "en",
		USDExchangeRate:                usdExchangeRate,
		CreditDisplaySymbol:            publicString(public, "billing.currency_symbol", "¥"),
		UsableGroups:                   h.statusUsableGroups(r),
	})
}

// usdExchangeRate 是模块 09 双币种展示的汇率常量（billing-02 §4.1 的唯一真源，
// 默认 71/10 = 7.1）。它不是设置键，不落 system_settings。
const usdExchangeRate = 7.1

// publicString 读取注册表投影中的字符串值；缺失时回落 fallback。
func publicString(values map[string]any, key, fallback string) string {
	if value, ok := values[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

// publicBool 读取注册表投影中的布尔值；缺失/类型不符时按 false
// （注册表已保证类型，这只防御装配期的零值 reader）。
func publicBool(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

type emailCodeRequest struct {
	Email          string `json:"email"`
	Purpose        string `json:"purpose"`
	TurnstileToken string `json:"turnstile_token"`
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

	// F-01-02 权限：register/reset 面向 anonymous；verify/bind 必须是已登录用户。
	// 本端点挂在公开路由上（不经过 SessionAuth），因此在这里显式校验会话。
	if req.Purpose == "verify" || req.Purpose == "bind" {
		sid, ok := sessionID(r)
		if !ok {
			writeUnauthorized(w)
			return
		}
		if _, err := h.sessions.Get(ctx, sid); err != nil {
			// 含 Redis 故障：无法确认身份时 fail-closed。
			writeUnauthorized(w)
			return
		}
	}

	// 同邮箱 1 次/60s（modules-08 F-08-16）。冷却键对邮箱做哈希，避免把 PII
	// 写进 Redis 键；先占用冷却键：即使后续 IP 桶拒绝或 SMTP 失败，也保持
	// 60s 冷却，防止重试风暴。
	cooldownKey := "email_code:cooldown:" + sha256Hex(email)
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

	// 限流（同邮箱冷却 + 同 IP 桶）之后才做人机校验：Turnstile 始终是限流
	// 的内层，服务不可达时也不能绕过本地速率桶（modules-01 §3.15 I-10）。
	if !h.checkTurnstile(w, r, auth.TurnstileEndpointVerification, req.TurnstileToken) {
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
	TurnstileToken  string `json:"turnstile_token"`
}

func (h *authHandlers) handleRegister(w http.ResponseWriter, r *http.Request) {
	// B3 注册请求速率桶 3 次/分钟/IP（security-07 §2.1，modules-08 F-08-16）。
	// 放在关闭注册开关与 body 解析之前：畸形请求、关闭态探测同样计数，既不
	// 付出解析/KDF 成本，也让失败审计被桶约束（未认证流量不得放大审计写入）。
	// 认证面 Redis 故障 fail-closed（§2.3：503 + 有界 Retry-After）。
	ip := clientIP(r)
	over, retry, err := h.allowRequestRate(r.Context(),
		RegisterRateLimitKey(ip), RegisterRateLimit, RegisterRateWindow)
	if err != nil {
		h.log.Error("registration request rate limiter unavailable", "error", err)
		h.auditRegister(r, 0, "failed", "limiter_unavailable")
		h.writeServiceUnavailable(w, "Registration is temporarily unavailable")
		return
	}
	if over {
		// 429 洪水每 IP 每窗口只留一条审计（SETNX 标记）；标记失败即跳过。
		h.auditRegisterRateLimited(r, ip)
		writeRateLimited(w, retry, "Too many registration requests")
		return
	}
	if !h.boolConfig("auth.signup_enabled", true) {
		h.auditRegister(r, 0, "failed", "signup_disabled")
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Registration is disabled")
		return
	}

	var req registerRequest
	if err := readAuthJSON(r, &req); err != nil {
		h.auditRegister(r, 0, "failed", "invalid_request")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}

	// 顺序冻结：注册速率桶（B3）→ 匿名请求体限制（readAuthJSON）→ Turnstile
	// → 业务校验（modules-01 §3.15）。人机校验绝不先于限流执行。
	if !h.checkTurnstile(w, r, auth.TurnstileEndpointRegister, req.TurnstileToken) {
		h.auditRegister(r, 0, "failed", "turnstile_failed")
		return
	}

	username := strings.TrimSpace(req.Username)
	switch {
	case !usernamePattern.MatchString(username):
		h.auditRegister(r, 0, "failed", "invalid_username")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid username")
		return
	case !auth.ValidatePasswordLength(req.Password):
		h.auditRegister(r, 0, "failed", "invalid_password")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Password must be between 8 and 128 characters")
		return
	case req.Password != req.ConfirmPassword:
		h.auditRegister(r, 0, "failed", "password_mismatch")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Passwords do not match")
		return
	case !req.AgreeTerms:
		h.auditRegister(r, 0, "failed", "terms_not_accepted")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Terms must be accepted")
		return
	}
	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		h.auditRegister(r, 0, "failed", "invalid_email")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address")
		return
	}
	// F-01-01 邮箱域名白名单（默认关闭）：开启时不在清单内的域名 422；
	// 清单为空/畸形按 fail-closed 拒绝（未启用时完全放行）。
	if !h.emailDomainAllowed(email) {
		h.auditRegister(r, 0, "failed", "email_domain_denied")
		writeProblem(w, http.StatusUnprocessableEntity, authProblemEmailDomainDenied, "Email domain is not allowed")
		return
	}
	aff := strings.TrimSpace(req.Aff)
	if utf8.RuneCountInString(aff) > maxAffRunes {
		h.auditRegister(r, 0, "failed", "invalid_aff")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid affiliate code")
		return
	}
	// 泄露口令筛查（security-01 §2.3）：长度合格但已泄露/上下文可猜的口令
	// 同样必须拒绝。用户名作为每用户上下文传入。
	if h.checker.CheckWithContext(req.Password, username) {
		h.auditRegister(r, 0, "failed", "weak_password")
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Password is too weak or has been compromised")
		return
	}

	emailVerified := false
	if h.boolConfig("auth.email_verification_required", false) {
		if !sixDigitPattern.MatchString(req.EmailCode) {
			h.auditRegister(r, 0, "failed", "invalid_email_code")
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
			return
		}
		ok, err := h.codes.consume(r.Context(), email, "register", req.EmailCode)
		if err != nil {
			h.auditRegister(r, 0, "failed", "service_unavailable")
			h.writeServiceUnavailable(w, "Registration is temporarily unavailable")
			return
		}
		if !ok {
			h.auditRegister(r, 0, "failed", "invalid_email_code")
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
			return
		}
		emailVerified = true
	}

	// 预检查给出友好 409；并发竞态由 CreateUser 的唯一约束兜底，两者响应必须同形。
	taken, err := h.repo.UsernameOrEmailTaken(r.Context(), username, email)
	if err != nil {
		h.log.Error("check duplicate user failed", "error", err)
		h.auditRegister(r, 0, "failed", "internal_error")
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}
	if taken {
		h.auditRegister(r, 0, "failed", "email_taken")
		h.writeDuplicateUser(w)
		return
	}

	passwordHash, err := h.hashPassword(req.Password)
	if err != nil {
		h.log.Error("hash password failed", "error", err)
		h.auditRegister(r, 0, "failed", "internal_error")
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}

	// 注册赠送反滥用（R-07-10）：先原子预留 IP 槽位；超限/计数不可用时
	// 注册照常成功但 bonus=0。预留失败（CreateUser 返回重复等）要归还槽位，
	// 避免攻击者用重复注册消耗他人的领赠额度。
	bonus := h.signupBonus(emailVerified)
	grantReserved := false
	grantSuppressed := false
	if bonus > 0 {
		allowed, err := h.reserveSignupGrant(r.Context(), ip)
		switch {
		case err != nil:
			// 反滥用计数不可用时跳过发放，但绝不阻塞注册（可用性优先于赠送）。
			h.log.Warn("signup grant limiter unavailable; skipping signup bonus", "error", err)
			bonus = 0
		case !allowed:
			bonus = 0
			grantSuppressed = true
		default:
			grantReserved = true
		}
	}

	created, err := h.repo.CreateUser(r.Context(), CreateUserInput{
		Username:      username,
		Email:         email,
		PasswordHash:  passwordHash,
		Aff:           aff,
		EmailVerified: emailVerified,
		SignupBonus:   bonus,
	})
	if err != nil && grantReserved {
		// 用户行没有创建成功 → 赠送不会发生 → 归还预占的 IP 槽位。
		h.releaseSignupGrantSlot(context.WithoutCancel(r.Context()), ip)
	}
	if errors.Is(err, ErrDuplicateUser) {
		h.auditRegister(r, 0, "failed", "email_taken")
		h.writeDuplicateUser(w)
		return
	}
	if err != nil {
		h.log.Error("create user failed", "error", err)
		h.auditRegister(r, 0, "failed", "internal_error")
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Registration failed")
		return
	}
	if created.Status != "" && created.Status != "enabled" {
		// DB 默认应为 enabled；非 enabled 说明站点策略/数据异常，拒绝建会话。
		h.auditRegister(r, created.ID, "failed", "account_unavailable")
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Account is not available")
		return
	}
	if grantSuppressed {
		h.alertSignupGrantSuppressed(r.Context(), created.ID, ip)
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
		h.log.Error("create session after registration failed", "user_id", created.ID, "error", err)
		// 账号已提交而会话失败：不能返回 500（重试会因重复注册 409）。
		// 映射为可重试的 503，并明确提示「账号可能已创建，请尝试登录」。
		h.auditRegister(r, created.ID, "failed", "session_unavailable")
		w.Header().Set("Retry-After", "5")
		writeAuthProblemDetail(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Registration succeeded but session creation failed",
			"The account may have been created. Please try logging in.")
		return
	}
	h.auditRegister(r, created.ID, "success", "")
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

// emailDomainAllowed 实施 F-01-01 的邮箱域名白名单。规则支持精确域名
// （example.com）与子域通配（*.example.com，不匹配裸域）。未启用时一律放行；
// 启用但清单为空/无法解析时 fail-closed（拒绝），避免「配置写坏=全放行」。
func (h *authHandlers) emailDomainAllowed(email string) bool {
	if !h.boolConfig("auth.email_domain_restrict_enabled", false) {
		return true
	}
	entries, ok := h.stringListConfig("auth.email_domain_allowlist")
	if !ok {
		return false
	}
	_, domain, found := strings.Cut(email, "@")
	if !found || domain == "" {
		return false
	}
	domain = strings.ToLower(domain)
	for _, raw := range entries {
		rule := strings.ToLower(strings.TrimSpace(raw))
		if rule == "" {
			continue
		}
		if suffix, wildcard := strings.CutPrefix(rule, "*."); wildcard {
			// "*.example.com" 只匹配子域，不匹配 example.com 本身。
			if suffix != "" && strings.HasSuffix(domain, "."+suffix) {
				return true
			}
			continue
		}
		if domain == rule {
			return true
		}
	}
	return false
}

// stringListConfig 读取 jsonb 字符串数组配置。非数组/含非字符串元素视为不可用。
func (h *authHandlers) stringListConfig(key string) ([]string, bool) {
	raw, ok := h.readConfig(key)
	if !ok {
		return nil, false
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// signupGrantSlotKey 是「同 IP 24h 内实际领赠名额」的 Redis 键。邮箱路径的
// 注册与首次邮箱验证补发共用同一键/脚本，保证两条发放路径共享同一反滥用上限。
func signupGrantSlotKey(ip string) string {
	return "signup_grant:ip:" + sha256Hex(ip)
}

// reserveSignupGrantForIP 原子预留一个领赠名额，返回 false 表示已达上限。
func reserveSignupGrantForIP(ctx context.Context, rdb *redis.Client, ip string) (bool, error) {
	if rdb == nil {
		return false, errors.New("httpapi: redis is required for signup grant slots")
	}
	n, err := reserveSignupGrantScript.Run(ctx, rdb, []string{signupGrantSlotKey(ip)},
		strconv.Itoa(signupGrantIPLimit),
		strconv.Itoa(int(signupGrantIPWindow.Seconds()))).Int64()
	if err != nil {
		return false, fmt.Errorf("httpapi: reserve signup grant slot: %w", err)
	}
	return n == 1, nil
}

// releaseSignupGrantForIP 归还预占名额（最终没有写出发放流水时）。
// 用 Lua 把计数下限钉在 0：键已过期/TTL 被 Redis 重启清掉时，裸 DECR 会
// 创建 -1 且没有 TTL 的永久负计数器，反而让后续发放永远通过。
func releaseSignupGrantForIP(ctx context.Context, rdb *redis.Client, ip string) error {
	if rdb == nil {
		return errors.New("httpapi: redis is required for signup grant slots")
	}
	return releaseSignupGrantScript.Run(ctx, rdb, []string{signupGrantSlotKey(ip)}).Err()
}

// releaseSignupGrantSlotDetached 在剥离取消信号的短超时上下文里归还名额：
// 验证事务已成功/失败后的清理不能被客户端断连取消，否则名额要等到 24h
// TTL 才释放（与注册路径的 context.WithoutCancel 口径一致）。
func releaseSignupGrantSlotDetached(ctx context.Context, rdb *redis.Client, ip string) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return releaseSignupGrantForIP(releaseCtx, rdb, ip)
}

// releaseSignupGrantScript：仅当键存在且值 > 0 时 DECR；返回原值（测试可读）。
var releaseSignupGrantScript = redis.NewScript(`
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
if n > 0 then
  redis.call('DECR', KEYS[1])
end
return n
`)

// reserveSignupGrantSlot 原子预留一个「同 IP 24h 内第 N 个实际领赠」名额。
// 返回 false 表示已达上限（调用方仍应完成注册，只是不发放）。
//
// 设备维度（同设备 24h ≤2，签名 Cookie nkt_did）尚未落地：待设备标识任务
// 提供可信 device id 后，在同一脚本里增加第二个桶即可；接口保持不变。
func (h *authHandlers) reserveSignupGrantSlot(ctx context.Context, ip string) (bool, error) {
	return reserveSignupGrantForIP(ctx, h.rdb, ip)
}

// releaseSignupGrantSlot 归还预占名额（用户行最终未创建时）。
func (h *authHandlers) releaseSignupGrantSlot(ctx context.Context, ip string) {
	if err := releaseSignupGrantForIP(ctx, h.rdb, ip); err != nil {
		h.log.Warn("release signup grant slot failed", "error", err)
	}
}

// alertSignupGrantSuppressed 在赠送因反滥用上限被抑制时触发告警钩子。
// 钩子为空时落 Warn 日志（审计/告警侧仍能发现异常注册模式）。
func (h *authHandlers) alertSignupGrantSuppressed(ctx context.Context, userID int64, ip string) {
	if h.signupGrantAlert != nil {
		h.signupGrantAlert(ctx, userID, ip)
		return
	}
	h.log.Warn("signup grant suppressed by anti-abuse cap", "user_id", userID)
}

// reserveSignupGrantScript：GET→比较→INCR 在同一 Lua 事务内完成，避免并发
// 注册同时看到 count<3 而超额发放。返回 1=预留成功，0=已达上限。
var reserveSignupGrantScript = redis.NewScript(`
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
if n >= tonumber(ARGV[1]) then
  return 0
end
n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return 1
`)

type loginRequest struct {
	Identifier        string `json:"identifier"`
	Password          string `json:"password"`
	PasswordEncrypted string `json:"password_encrypted"`
	EncryptionKeyID   string `json:"encryption_key_id"`
	TurnstileToken    string `json:"turnstile_token"`
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

	// B1 登录请求速率桶 5 次/分钟/IP+identifier（security-07 §2.1，modules-08
	// F-08-16）。它在失败锁定 B2 之前计数，覆盖「低频但持续」的凭据填充：
	// B2 只在连续 5 次失败后锁 15 分钟，B1 则限制任意 1 分钟内的请求总量。
	// 认证面 Redis 故障 fail-closed（§2.3：503 + 有界 Retry-After）。
	over, retry, err := h.allowRequestRate(ctx,
		LoginRateLimitKey(ip, identifier), LoginRateLimit, LoginRateWindow)
	if err != nil {
		h.log.Error("login request rate limiter unavailable", "error", err)
		h.auditLoginLimited(r, ip, identifier, nil, "limiter_unavailable")
		h.writeServiceUnavailable(w, "Login is temporarily unavailable")
		return
	}
	if over {
		h.auditLoginLimited(r, ip, identifier, nil, "rate_limited")
		writeRateLimited(w, retry, "Too many login requests")
		return
	}

	// 登录速率桶（B1）之后才做人机校验：Turnstile 必须始终是限流的内层，
	// 否则无效令牌会绕过本地桶直接消耗 Cloudflare 校验配额（modules-01 §3.15）。
	if !h.checkTurnstile(w, r, auth.TurnstileEndpointLogin, req.TurnstileToken) {
		return
	}

	// 登录前端加密（F-01-25）：解密置于速率桶/人机校验之后，避免未限流的
	// RSA 解封装消耗。解密失败/防降级拒绝不得提前返回——那会把「解密失败」
	// 与「口令错误」在限流计次与 argon2 时序上区分开（反 oracle）。置空
	// 口令继续正常失败路径：空串永不匹配任何 PHC 哈希。
	password := req.Password
	if h.passwordEncryption != nil {
		decoded, err := h.passwordEncryption.DecodeLoginPassword(
			r.Context(), req.Password, req.PasswordEncrypted, req.EncryptionKeyID)
		if err != nil {
			password = ""
		} else {
			password = decoded
		}
	}

	// 锁定预检查：已知 identifier 的双桶在此拦截，避免锁定期间仍消耗 DB/argon2。
	if err := h.limiter.Check(ctx, ip, identifier); err != nil {
		h.auditLoginLimited(r, ip, identifier, nil, limiterReason(err))
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
			h.auditLoginLimited(r, ip, identifier, nil, limiterReason(err))
			h.writeLimiterFailure(w, err)
			return
		}
		// dummy verify：与真实用户同等的 argon2 成本，消除「查无此人」的时序特征。
		_ = h.verifyPassword(dummyPasswordHash(), password)
		if err := h.limiter.Record(ctx, ip, "", false); err != nil {
			h.auditLoginLimited(r, ip, identifier, nil, "limiter_unavailable")
			h.writeLimiterFailure(w, err)
			return
		}
		h.auditLogin(r, nil, "failed", "invalid_credentials")
		writeUnauthorized(w) // 与错误口令逐字相同
		return
	}

	// OAuth 即时注册用户没有口令：仍执行 dummy verify 保持时序，再按失败计数。
	if user.PasswordHash == "" {
		_ = h.verifyPassword(dummyPasswordHash(), password)
		if err := h.limiter.Record(ctx, ip, identifier, false); err != nil {
			h.auditLoginLimited(r, ip, identifier, user, "limiter_unavailable")
			h.writeLimiterFailure(w, err)
			return
		}
		h.auditLogin(r, user, "failed", "invalid_credentials")
		writeUnauthorized(w)
		return
	}
	if !h.verifyPassword(user.PasswordHash, password) {
		if err := h.limiter.Record(ctx, ip, identifier, false); err != nil {
			h.auditLoginLimited(r, ip, identifier, user, "limiter_unavailable")
			h.writeLimiterFailure(w, err)
			return
		}
		h.auditLogin(r, user, "failed", "invalid_credentials")
		writeUnauthorized(w)
		return
	}

	// 口令正确后才检查封禁：错误口令不得成为账号状态探测信道。
	if user.Status != "enabled" {
		h.auditLogin(r, user, "failed", "account_disabled")
		writeProblem(w, http.StatusUnauthorized, authProblemAccountDisabled, "This account has been disabled")
		return
	}

	// 管理员下发的临时口令带 24h 有效期（F-07-12/TEMP_PWD_TTL）：有效标记由
	// POST /api/users/{id}/reset-password 写入，用户自选新口令成功后删除。
	// 标记缺失（过期/被清理/Redis 换库）即视为过期——绝不能因为读不到截止
	// 时间就无限期放行临时口令。Redis 故障 fail-closed（503）。
	if user.MustChangePassword {
		exists, err := h.rdb.Exists(ctx, tempPasswordKeyPrefix+strconv.FormatInt(user.ID, 10)).Result()
		if err != nil {
			h.log.Error("check temp password ttl failed", "error", err)
			h.auditLogin(r, user, "failed", "session_unavailable")
			h.writeServiceUnavailable(w, "Login is temporarily unavailable")
			return
		}
		if exists == 0 {
			h.auditLogin(r, user, "failed", "temp_password_expired")
			writeProblem(w, http.StatusUnauthorized, authProblemTempPasswordExpired,
				"The temporary password has expired; ask an administrator to reset it")
			return
		}
	}

	// 强制改密状态（modules-01 F-01-03 / F-07-12）：按用户写入 Redis 受限标记，
	// SessionAuth 据此只放行登出/自读/改密/2FA 端点。写入失败 fail-closed——否则
	// 临时口令会换来一个不受限的完整会话。
	if user.MustChangePassword {
		if err := h.sessions.MarkPasswordChangeRequired(ctx, user.ID); err != nil {
			h.log.Error("mark password change required failed", "user_id", user.ID, "error", err)
			h.auditLogin(r, user, "failed", "session_unavailable")
			h.writeServiceUnavailable(w, "Login is temporarily unavailable")
			return
		}
	} else {
		// 自愈：历史改密若在删除标记时遇到 Redis 故障，残留标记会让用户被
		// 永久卡在受限态。DB 真源已声明 flag=false，下次成功登录时清除。
		if err := h.sessions.ClearPasswordChangeRequired(ctx, user.ID); err != nil {
			h.log.Warn("clear stale password change restriction failed", "user_id", user.ID, "error", err)
		}
	}

	// 连续失败计数清零。Reset 失败时保持保守：下一次失败可能提前锁定，
	// 但会话创建同样依赖 Redis，会 fail-closed。
	_ = h.limiter.Record(ctx, ip, identifier, true)

	if user.TOTPEnabled {
		ticket, err := h.createPending2FA(ctx, user.ID)
		if err != nil {
			h.log.Error("create pending 2fa ticket failed", "error", err)
			h.auditLogin(r, user, "failed", "session_unavailable")
			h.writeServiceUnavailable(w, "Login is temporarily unavailable")
			return
		}
		h.auditLogin(r, user, "success", "mfa_pending")
		writeJSON(w, http.StatusOK, pending2FAResponse{Status: "pending_2fa", Ticket: ticket})
		return
	}

	if err := h.startSession(ctx, w, r, user); err != nil {
		h.log.Error("create session failed", "error", err)
		h.auditLogin(r, user, "failed", "session_unavailable")
		h.writeServiceUnavailable(w, "Login is temporarily unavailable")
		return
	}
	h.auditLogin(r, user, "success", "")
	writeJSON(w, http.StatusOK, authSessionResponse{User: userView(user)})
}

// clearSiteDataHeader 是 security-09 §2.5 / TC-I-06 冻结的三项，顺序与内容
// 不得增删。刻意不含 executionContexts：它会让浏览器立即终止页面脚本，
// 登出后前端连「已退出」的确认界面都无法渲染。
const clearSiteDataHeader = `"cache", "storage", "cookies"`

// setClearSiteData 要求浏览器清除本源的缓存、存储与 Cookie（登出/删号）。
// 与 Set-Cookie 过期一起下发：前者清理前端存储，后者清理 HTTP Cookie。
func setClearSiteData(w http.ResponseWriter) {
	w.Header().Set("Clear-Site-Data", clearSiteDataHeader)
}

func (h *authHandlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	// 登出是 public-safe 的：无会话/重复登出都必须 204；也不做 CSRF 校验
	// （security-01 §2.2：登出不构成状态提升，且清 Cookie 必须在会话失效后仍可用）。
	if sid, ok := sessionID(r); ok {
		// 身份必须在吊销前读取：Revoke 之后会话记录已消失，审计将拿不到 actor。
		sess, getErr := h.sessions.Get(r.Context(), sid)
		if err := h.sessions.Revoke(r.Context(), sid); err != nil {
			h.log.Error("revoke session failed", "error", err)
			h.writeServiceUnavailable(w, "Logout is temporarily unavailable")
			return
		}
		if getErr == nil && sess != nil {
			h.auditLogout(r, sess.UserID)
		}
	}
	setClearSiteData(w)
	clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// auditLogout 写 auth.logout 审计。detail 只有结果；IP/UA 走专用列。
func (h *authHandlers) auditLogout(r *http.Request, userID int64) {
	if h.audit == nil {
		return
	}
	event := audit.Event{
		Action:    audit.ActionAuthLogout,
		Actor:     userID,
		Result:    "success",
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Method:    r.Method,
		Route:     "/api/logout",
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Error("write logout audit failed", "error", err)
	}
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

// writeAuthProblemDetail 输出带 detail 的 RFC 7807 Problem（writeProblem 无 detail 参数）。
func writeAuthProblemDetail(w http.ResponseWriter, status int, problemType, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(dto.Problem{
		Type:   problemType,
		Title:  title,
		Status: status,
		Detail: &detail,
	})
}

// limiterReason 把限流器错误归类为审计原因（不记录任何凭据材料）。
func limiterReason(err error) string {
	var locked *LockedError
	if errors.As(err, &locked) {
		return "locked"
	}
	return "limiter_unavailable"
}

// auditLogin 写入 auth.login 审计（success/failed）。审计是旁路：写失败只落
// 日志，绝不影响登录结果。detail 只含结果/原因，IP/UA 走专用列；绝不含口令、
// 验证码或 identifier 原文（避免把枚举输入落成 PII）。
func (h *authHandlers) auditLogin(r *http.Request, user *UserRecord, result, reason string) {
	if h.audit == nil {
		return
	}
	var actor int64
	var username string
	if user != nil {
		actor = user.ID
		username = user.Username
	}
	detail, err := json.Marshal(map[string]string{"result": result, "reason": reason})
	if err != nil {
		h.log.Error("marshal login audit detail failed", "error", err)
		return
	}
	event := audit.Event{
		Action:    audit.ActionAuthLogin,
		Actor:     actor,
		Username:  username,
		Result:    result,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Method:    r.Method,
		Route:     "/api/auth/login",
		Detail:    detail,
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Error("write login audit failed", "error", err)
	}
}

// loginAuditLimitedKeyPrefix 限制登录「限流拒绝」审计的写入频率：/api/auth/login
// 是未认证端点，429/锁定可被无限重放；每 IP+identifier 每窗口至多一条。
const loginAuditLimitedKeyPrefix = "login:audit:limited:"

// auditLoginLimited 是登录限流拒绝审计的有界版本（rate_limited / locked /
// limiter_unavailable）。SETNX 失败或窗口内已记过 → 跳过：审计可少记，
// 绝不能成为未认证流量的日志放大面。user 可空（未解析到用户时 actor=0）。
func (h *authHandlers) auditLoginLimited(r *http.Request, ip, identifier string, user *UserRecord, reason string) {
	if h.audit == nil || h.rdb == nil {
		return
	}
	key := loginAuditLimitedKeyPrefix +
		sha256Hex(ip+"\x00"+strings.ToLower(strings.TrimSpace(identifier)))
	accepted, err := h.rdb.SetNX(r.Context(), key, "1", LoginRateWindow).Result()
	if err != nil || !accepted {
		return
	}
	h.auditLogin(r, user, "failed", reason)
}

// auditRegister 写 auth.register 审计（成功/失败）。成功时 Actor 是新用户 id；
// 失败时 actor=0（writer 回退 user#0）。detail 只含结果与原因，绝不含口令、
// 验证码、Turnstile token 或邮箱原文（PII 最小化）。
func (h *authHandlers) auditRegister(r *http.Request, actor int64, result, reason string) {
	if h.audit == nil {
		return
	}
	detail, err := json.Marshal(map[string]string{"result": result, "reason": reason})
	if err != nil {
		h.log.Error("marshal register audit detail failed", "error", err)
		return
	}
	event := audit.Event{
		Action:    audit.ActionAuthRegister,
		Actor:     actor,
		Result:    result,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Method:    r.Method,
		Route:     "/api/auth/register",
		Detail:    detail,
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Error("write register audit failed", "error", err)
	}
}

// signupAuditRateLimitedKeyPrefix 限制「注册限流拒绝」审计的写入频率：
// /api/auth/register 是未认证端点，429 可被无限重放；每 IP 每窗口至多一条。
const signupAuditRateLimitedKeyPrefix = "signup:audit:rate_limited:"

// auditRegisterRateLimited 在 SETNX 标记成功时写一次 rate_limited 审计。
// 标记写入失败（Redis 故障）或窗口内已记过 → 跳过：审计可少记，不能放大。
func (h *authHandlers) auditRegisterRateLimited(r *http.Request, ip string) {
	if h.audit == nil || h.rdb == nil {
		return
	}
	key := signupAuditRateLimitedKeyPrefix + sha256Hex(ip)
	accepted, err := h.rdb.SetNX(r.Context(), key, "1", RegisterRateWindow).Result()
	if err != nil || !accepted {
		return
	}
	h.auditRegister(r, 0, "failed", "rate_limited")
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
