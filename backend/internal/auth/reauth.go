// 本文件实现「全量再认证门」（security-01 §2.6/§3.1 的 7.5.1，GC-10）。
//
// 契约：敏感账号属性变更（改邮箱、未来开放的用户名变更、恢复信息变更等）前，
// 必须通过当前密码 + 账号已启用 MFA 时的第二因子（TOTP）。再认证窗口
// REAUTH_WINDOW = 5 分钟，自最近一次成功再认证起算；超窗须重新认证。
// 失败返回 ErrReauthRequired（HTTP 层映射 403 reauth_required），
// 且不得写入任何「已再认证」状态（TC-A-21「不落库」）。
//
// 设计取舍：
//   - 本包不实现 TOTP 算法（Task 7 的 auth.VerifyTOTP）：校验器作为接缝注入，
//     既保持任务边界，也避免再认证路径悄悄绕过 Task 7 的 ±0 步进策略。
//     未注入校验器时，已启用 MFA 的账号一律拒绝（fail-closed）。
//   - 口令材料由 ReauthUserSource 提供；TOTP secret 必须由实现方用
//     CREDENTIAL_ENCRYPTION_KEY 解密后返回明文，本包只消费、不落盘、不记录。
//   - Redis 故障以普通 error 上抛（调用方应映射 503），绝不返回 nil：
//     无法确认再认证状态时不能放行（GC-7 fail-closed）。
package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ReauthWindow 是再认证窗口（security-01 §3.1 冻结：REAUTH_WINDOW=5 分钟）。
// 自最近一次成功再认证起算，窗口内的后续敏感操作可直接通过。
const ReauthWindow = 5 * time.Minute

// 再认证失败锁定（security-01 §2.6「连续失败计入既有认证失败锁定」的再认证
// 专用桶）：连续 5 次失败锁定 15 分钟，与登录失败的阈值/时长同口径。
const (
	// ReauthMaxFailures 是触发锁定的连续失败次数。
	ReauthMaxFailures = 5
	// ReauthLockDuration 是锁定时长。
	ReauthLockDuration = 15 * time.Minute
	// reauthFailureKeyPrefix 是每用户失败计数键；计数含 TTL，成功后清零。
	reauthFailureKeyPrefix = "reauth_fail:"
)

// ErrReauthRequired 表示再认证未通过：因子缺失/错误、窗口已过期、账号没有
// 可用口令，或已启用 MFA 但第二因子缺失。HTTP 层映射 403 reauth_required。
// Redis/存储故障使用普通 error，便于调用方区分 503 与 403（两者都是拒绝）。
var ErrReauthRequired = errors.New("auth: reauthentication required")

// ErrReauthLocked 是锁定哨兵；剩余时间用 ReauthLockedError.RetryAfter 读取。
// HTTP 层映射 429 + Retry-After（与登录锁定同口径）。
var ErrReauthLocked = errors.New("auth: reauthentication temporarily locked")

// ReauthLockedError 携带剩余锁定时间，供 Retry-After 使用。
type ReauthLockedError struct {
	RetryAfter time.Duration
}

// ErrReauthFactorShape 表示提交的因子形态与账号当前 MFA 状态不匹配：
// 已启用 TOTP 却只给密码、未启用却只给 TOTP，或两种因子同时提供。
//
// 与 ErrReauthRequired 的区分是刻意的：形态错误是**调用方对账号 MFA 状态
// 的误判**（请求参数错误，security-03 §2.10 要求 422 字段级错误），不是
// 一次失败的因子猜测，因此不执行解密、也不计入失败锁定——否则不了解账号
// 状态的合法管理员会在改正参数之前被锁 15 分钟。
var ErrReauthFactorShape = errors.New("auth: reauth factor does not match account mfa state")

// ReauthFactorShapeError 携带账号实际要求的因子字段名（password /
// totp_code），HTTP 层据此把 422 定位到正确字段。
type ReauthFactorShapeError struct {
	// Required 是本次请求应当提交的唯一因子字段。
	Required string
}

func (e *ReauthFactorShapeError) Error() string {
	return fmt.Sprintf("auth: reauth factor %q is required by the account mfa state", e.Required)
}

// Unwrap 让 errors.Is(err, ErrReauthFactorShape) 成立。
func (e *ReauthFactorShapeError) Unwrap() error { return ErrReauthFactorShape }

func (e *ReauthLockedError) Error() string {
	return fmt.Sprintf("auth: reauthentication locked for %s", e.RetryAfter)
}

// Unwrap 让 errors.Is(err, ErrReauthLocked) 成立。
func (e *ReauthLockedError) Unwrap() error { return ErrReauthLocked }

// ReauthFactor 是一次再认证提交的因子。Password 是当前密码（未启用 MFA 时
// 的唯一因子）；TOTPCode 是账号已启用 MFA 时必填的第二因子。
//
// 两个字段都为空表示「沿用窗口」：只有最近一次成功再认证仍在 REAUTH_WINDOW
// 内时才放行——对应设计里「通过后 5 分钟内成功」的体验。
type ReauthFactor struct {
	Password string
	TOTPCode string
}

// ReauthMaterial 是再认证所需的用户凭据投影。
//
// TOTPSecret 必须是解密后的 base32 明文；实现方（组合 store 与
// CREDENTIAL_ENCRYPTION_KEY）负责 AES-256-GCM 解密。该值只在本次校验中
// 使用，不得写日志、审计或返回给客户端。
type ReauthMaterial struct {
	PasswordHash string
	TOTPEnabled  bool
	TOTPSecret   string
}

// ReauthUserSource 提供用户再认证材料。用户不存在时返回 (nil, nil)；
// 存储故障返回 error（门会 fail-closed）。
type ReauthUserSource interface {
	ReauthMaterial(ctx context.Context, userID int64) (*ReauthMaterial, error)
}

// ReauthMaterialFunc 把普通函数适配为 ReauthUserSource，便于装配层直接注入
// 一次查询（避免为单一方法定义新类型）。
type ReauthMaterialFunc func(ctx context.Context, userID int64) (*ReauthMaterial, error)

// ReauthMaterial 实现 ReauthUserSource。
func (f ReauthMaterialFunc) ReauthMaterial(ctx context.Context, userID int64) (*ReauthMaterial, error) {
	return f(ctx, userID)
}

// TOTPVerifier 是 Task 7 的 auth.VerifyTOTP 的注入形态。签名冻结为
// (secret, code, now)，使 server NTP 时间成为唯一时间源、测试可注入固定时钟。
type TOTPVerifier func(secret, code string, now time.Time) bool

// ReauthLimiter 是再认证失败计数的接缝。Check 在每次因子校验前调用，返回
// *ReauthLockedError 表示处于锁定窗口；Record 记录一次尝试的结果（ok=true
// 清零连续失败）。默认实现是 RedisReauthLimiter；nil 表示禁用锁定（仅测试）。
type ReauthLimiter interface {
	Check(ctx context.Context, userID int64) error
	Record(ctx context.Context, userID int64, ok bool) error
}

// RedisReauthLimiter 用 Redis 实现「连续 5 次失败锁 15 分钟」。
// 计数按用户，不含 IP：再认证发生在已登录会话内，userID 是更准确的维度。
type RedisReauthLimiter struct {
	rdb *redis.Client
}

// NewRedisReauthLimiter 构造默认限流器。
func NewRedisReauthLimiter(rdb *redis.Client) *RedisReauthLimiter {
	return &RedisReauthLimiter{rdb: rdb}
}

// Check 判断用户是否处于再认证锁定窗口。
func (l *RedisReauthLimiter) Check(ctx context.Context, userID int64) error {
	key := reauthFailureKey(userID)
	n, err := l.rdb.Get(ctx, key).Int()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: reauth limiter check: %w", err)
	}
	if n < ReauthMaxFailures {
		return nil
	}
	ttl, err := l.rdb.PTTL(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("auth: reauth limiter ttl: %w", err)
	}
	switch {
	case ttl == -2:
		// 键在 GET 与 PTTL 之间自然过期：计数已不存在，不能用一个刚消失的
		// 桶把用户再锁 15 分钟（与登录锁定桶对 -2 的口径一致）。
		return nil
	case ttl <= 0 || ttl > ReauthLockDuration:
		// 异常数据来源：旧版本/人工写入的计数器没有 TTL，或 TTL 被外部改坏。
		// 只返回锁定时长而不修复会让该键永不过期——用户被事实永久锁定。
		// 这里用完整窗口重写键（自愈）：保护不中断，且 15 分钟后自然解除。
		if err := l.rdb.Set(ctx, key, strconv.Itoa(n), ReauthLockDuration).Err(); err != nil {
			return fmt.Errorf("auth: reauth limiter heal ttl: %w", err)
		}
		ttl = ReauthLockDuration
	}
	return &ReauthLockedError{RetryAfter: ttl}
}

// Record 记录一次再认证结果。成功清零；失败 INCR 并把 TTL 重置为 15 分钟。
func (l *RedisReauthLimiter) Record(ctx context.Context, userID int64, ok bool) error {
	key := reauthFailureKey(userID)
	if ok {
		if err := l.rdb.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("auth: reauth limiter reset: %w", err)
		}
		return nil
	}
	if _, err := reauthFailureScript.Run(ctx, l.rdb, []string{key},
		strconv.FormatInt(ReauthLockDuration.Milliseconds(), 10)).Result(); err != nil {
		return fmt.Errorf("auth: reauth limiter record: %w", err)
	}
	return nil
}

// reauthFailureScript 原子 INCR + 重置 TTL，避免 INCR 成功而 EXPIRE 失败导致
// 计数器永不过期（与登录失败计数同一实现口径）。
var reauthFailureScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return n
`)

func reauthFailureKey(userID int64) string {
	return reauthFailureKeyPrefix + strconv.FormatInt(userID, 10)
}

// ReauthGate 校验再认证因子并维护用户的再认证窗口。可并发使用。
type ReauthGate struct {
	rdb        *redis.Client
	users      ReauthUserSource
	verifyTOTP TOTPVerifier
	limiter    ReauthLimiter
	// now 是 TOTP 校验的服务端时钟（NTP）；窗口过期由 Redis TTL 独立判定，
	// 两者刻意分离——窗口是权威存储状态，不应随进程时钟漂移。
	now func() time.Time
}

// NewReauthGate 构造再认证门。rdb 与 users 必填；verifyTOTP 为 nil 时，
// 已启用 MFA 的账号无法通过再认证（fail-closed），装配层应在 Task 7 落地后
// 注入 auth.VerifyTOTP。失败锁定默认启用（RedisReauthLimiter）。
func NewReauthGate(rdb *redis.Client, users ReauthUserSource, verifyTOTP TOTPVerifier) *ReauthGate {
	g := &ReauthGate{rdb: rdb, users: users, verifyTOTP: verifyTOTP, now: time.Now}
	if rdb != nil {
		g.limiter = NewRedisReauthLimiter(rdb)
	}
	return g
}

// SetLimiter 替换失败计数实现；nil 表示禁用锁定（仅测试使用）。
// 必须在开始服务前调用。
func (m *ReauthGate) SetLimiter(l ReauthLimiter) {
	m.limiter = l
}

// setClock 替换 TOTP 校验时钟（仅测试使用；生产装配不调用）。
func (m *ReauthGate) setClock(now func() time.Time) {
	if now != nil {
		m.now = now
	}
}

// Authorize 校验 userID 的再认证因子。
//
//   - 提供因子时：必须验证当前密码；账号已启用 MFA 时还必须验证 TOTP。
//     成功后写入/刷新 reauth:{userID} 窗口（5 分钟）。
//   - 未提供因子时：仅当窗口仍有效时返回 nil，否则 ErrReauthRequired。
//
// 返回值约定：认证失败 == ErrReauthRequired；连续失败锁定 ==
// *ReauthLockedError（errors.Is(err, ErrReauthLocked)）；基础设施故障 ==
// 其他 error。因子路径受失败锁定约束；纯窗口检查不产生猜测，不计数。
func (m *ReauthGate) Authorize(ctx context.Context, userID int64, factor ReauthFactor) error {
	if m == nil || m.rdb == nil || m.users == nil {
		return errors.New("auth: reauth gate is not configured")
	}

	if factor.Password == "" && factor.TOTPCode == "" {
		ok, err := m.windowOpen(ctx, userID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrReauthRequired
		}
		return nil
	}

	// 锁定检查在读取用户材料/执行 KDF 之前：锁定期间不再消耗 argon2 与 DB。
	if m.limiter != nil {
		if err := m.limiter.Check(ctx, userID); err != nil {
			return err
		}
	}
	fail := func() error {
		if m.limiter != nil {
			if err := m.limiter.Record(ctx, userID, false); err != nil {
				return err
			}
		}
		return ErrReauthRequired
	}

	material, err := m.users.ReauthMaterial(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: load reauth material: %w", err)
	}
	if material == nil {
		return fail()
	}

	if material.PasswordHash == "" {
		// OAuth-only 用户没有口令，不能「空口令通过」；仍然执行一次等价
		// KDF，避免通过响应时间判断账号是否设置过密码。
		_ = VerifyPassword(dummyReauthHash(), factor.Password)
		return fail()
	}
	if !VerifyPassword(material.PasswordHash, factor.Password) {
		return fail()
	}

	if material.TOTPEnabled {
		if factor.TOTPCode == "" || m.verifyTOTP == nil {
			return fail()
		}
		if !m.verifyTOTP(material.TOTPSecret, factor.TOTPCode, m.now()) {
			return fail()
		}
	}

	// 成功先清零失败计数，再写窗口：若计数清零失败则不放行（fail-closed），
	// 避免「已经锁定但本次成功换来了窗口」的旁路。
	if m.limiter != nil {
		if err := m.limiter.Record(ctx, userID, true); err != nil {
			return err
		}
	}
	// 只有全部因子通过后才记录窗口；失败不会留下任何可被复用的状态。
	if err := m.rdb.Set(ctx, reauthKey(userID), "1", ReauthWindow).Err(); err != nil {
		return fmt.Errorf("auth: record reauth window: %w", err)
	}
	return nil
}

// AuthorizeOnce 校验一次**仅本次请求有效**的步进认证因子，不写再认证窗口。
//
// 与 Authorize 的语义差异来自 security-03 §2.10 的「步进认证只授权本次
// 请求」：
//   - 必须现场提交因子，绝不沿用 5 分钟窗口（空因子直接拒绝）；
//   - 因子形态由账号当前 MFA 状态决定：已启用 TOTP 时只接受 totp_code，
//     未启用时只接受 password；形态不匹配返回 *ReauthFactorShapeError；
//   - 成功后**不写** reauth:<userID>，不延长/提升会话，不产生后续免认证
//     窗口（渠道凭据揭示的冻结口径）。
//
// 失败锁定复用同一 reauth_fail:<userID> 桶（与 Authorize 的其他敏感操作
// 共用阈值与时长），但形态错误不计数：它不是因子猜测，而是调用方对账号
// MFA 状态的误判，计数会让合法管理员在改正参数前被锁 15 分钟。
//
// 返回值约定：因子失败 == ErrReauthRequired；形态不符 ==
// *ReauthFactorShapeError（errors.Is ErrReauthFactorShape）；锁定 ==
// *ReauthLockedError；基础设施故障 == 其他 error（调用方映射 503）。
func (m *ReauthGate) AuthorizeOnce(ctx context.Context, userID int64, factor ReauthFactor) error {
	if m == nil || m.rdb == nil || m.users == nil {
		return errors.New("auth: reauth gate is not configured")
	}

	password := strings.TrimSpace(factor.Password)
	totpCode := strings.TrimSpace(factor.TOTPCode)
	if password == "" && totpCode == "" {
		// 没有「沿用窗口」分支：步进认证不能依赖先前的成功。
		return ErrReauthRequired
	}

	// 锁定检查在读取用户材料/KDF 之前，与 Authorize 同口径。
	if m.limiter != nil {
		if err := m.limiter.Check(ctx, userID); err != nil {
			return err
		}
	}
	fail := func() error {
		if m.limiter != nil {
			if err := m.limiter.Record(ctx, userID, false); err != nil {
				return err
			}
		}
		return ErrReauthRequired
	}

	material, err := m.users.ReauthMaterial(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: load reauth material: %w", err)
	}
	if material == nil {
		return fail()
	}

	if material.TOTPEnabled {
		// 已启用 MFA：唯一合法因子是 totp_code。同时提供密码也是形态错误
		// （设计明确「恰好一个」；密码在这里没有校验意义）。
		if totpCode == "" || password != "" {
			return &ReauthFactorShapeError{Required: "totp_code"}
		}
		if m.verifyTOTP == nil {
			// 未注入校验器的已启用 MFA 账号一律拒绝（fail-closed）。
			return fail()
		}
		if !m.verifyTOTP(material.TOTPSecret, totpCode, m.now()) {
			return fail()
		}
	} else {
		if password == "" || totpCode != "" {
			return &ReauthFactorShapeError{Required: "password"}
		}
		if material.PasswordHash == "" {
			// OAuth-only 账号没有口令：执行等价 KDF 后拒绝，不制造时序差异。
			_ = VerifyPassword(dummyReauthHash(), password)
			return fail()
		}
		if !VerifyPassword(material.PasswordHash, password) {
			return fail()
		}
	}

	// 成功清零失败计数；**不写**窗口键（只授权本次请求）。
	if m.limiter != nil {
		if err := m.limiter.Record(ctx, userID, true); err != nil {
			return err
		}
	}
	return nil
}

// Clear 清除用户的再认证窗口。改密/重置密码等凭证变更后必须调用：
// 旧凭证换来的再认证不应继续授权后续敏感操作。
func (m *ReauthGate) Clear(ctx context.Context, userID int64) error {
	if m == nil || m.rdb == nil {
		return errors.New("auth: reauth gate is not configured")
	}
	return ClearReauthWindow(ctx, m.rdb, userID)
}

// ClearReauthWindow 直接清除 Redis 中的窗口键。HTTP 处理器没有 ReauthGate
// 实例（改密路径也不经过再认证门），因此单独暴露同一口径的清理函数。
func ClearReauthWindow(ctx context.Context, rdb *redis.Client, userID int64) error {
	if rdb == nil {
		return errors.New("auth: redis client is required")
	}
	if err := rdb.Del(ctx, reauthKey(userID)).Err(); err != nil {
		return fmt.Errorf("auth: clear reauth window: %w", err)
	}
	return nil
}

// windowOpen 读取窗口键。Redis 错误原样上抛（fail-closed）。
func (m *ReauthGate) windowOpen(ctx context.Context, userID int64) (bool, error) {
	n, err := m.rdb.Exists(ctx, reauthKey(userID)).Result()
	if err != nil {
		return false, fmt.Errorf("auth: read reauth window: %w", err)
	}
	return n > 0, nil
}

func reauthKey(userID int64) string {
	return "reauth:" + strconv.FormatInt(userID, 10)
}

// dummyReauthHash 为无口令账号的时序对齐哈希。sync.OnceValue 保证只付一次
// KDF 成本；CSPRNG 失败属灾难状态，panic 让请求 500 fail-closed。
var dummyReauthHash = sync.OnceValue(func() string {
	hash, err := HashPassword("newtg-reauth-dummy-password")
	if err != nil {
		panic("auth: generate dummy reauth hash: " + err.Error())
	}
	return hash
})
