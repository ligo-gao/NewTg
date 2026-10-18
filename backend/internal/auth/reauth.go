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
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ReauthWindow 是再认证窗口（security-01 §3.1 冻结：REAUTH_WINDOW=5 分钟）。
// 自最近一次成功再认证起算，窗口内的后续敏感操作可直接通过。
const ReauthWindow = 5 * time.Minute

// ErrReauthRequired 表示再认证未通过：因子缺失/错误、窗口已过期、账号没有
// 可用口令，或已启用 MFA 但第二因子缺失。HTTP 层映射 403 reauth_required。
// Redis/存储故障使用普通 error，便于调用方区分 503 与 403（两者都是拒绝）。
var ErrReauthRequired = errors.New("auth: reauthentication required")

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

// ReauthGate 校验再认证因子并维护用户的再认证窗口。可并发使用。
type ReauthGate struct {
	rdb        *redis.Client
	users      ReauthUserSource
	verifyTOTP TOTPVerifier
	// now 是可替换时钟，仅供测试跨越 5 分钟窗口；生产恒为 time.Now。
	now func() time.Time
}

// NewReauthGate 构造再认证门。rdb 与 users 必填；verifyTOTP 为 nil 时，
// 已启用 MFA 的账号无法通过再认证（fail-closed），装配层应在 Task 7 落地后
// 注入 auth.VerifyTOTP。
func NewReauthGate(rdb *redis.Client, users ReauthUserSource, verifyTOTP TOTPVerifier) *ReauthGate {
	return &ReauthGate{rdb: rdb, users: users, verifyTOTP: verifyTOTP, now: time.Now}
}

// setClock 替换时钟（仅测试使用；生产装配不调用）。
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
// 返回值约定：认证失败 == ErrReauthRequired；基础设施故障 == 其他 error。
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

	material, err := m.users.ReauthMaterial(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: load reauth material: %w", err)
	}
	if material == nil {
		return ErrReauthRequired
	}

	if material.PasswordHash == "" {
		// OAuth-only 用户没有口令，不能「空口令通过」；仍然执行一次等价
		// KDF，避免通过响应时间判断账号是否设置过密码。
		_ = VerifyPassword(dummyReauthHash(), factor.Password)
		return ErrReauthRequired
	}
	if !VerifyPassword(material.PasswordHash, factor.Password) {
		return ErrReauthRequired
	}

	if material.TOTPEnabled {
		if factor.TOTPCode == "" || m.verifyTOTP == nil {
			return ErrReauthRequired
		}
		if !m.verifyTOTP(material.TOTPSecret, factor.TOTPCode, m.now()) {
			return ErrReauthRequired
		}
	}

	// 只有全部因子通过后才记录窗口；失败不会留下任何可被复用的状态。
	if err := m.rdb.Set(ctx, reauthKey(userID), "1", ReauthWindow).Err(); err != nil {
		return fmt.Errorf("auth: record reauth window: %w", err)
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
