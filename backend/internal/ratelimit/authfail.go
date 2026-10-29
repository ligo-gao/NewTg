package ratelimit

// R-03：/v1 预认证失败限流（按客户端 IP 的失败桶 + 10 分钟锁）。
//
// 设计权威：docs/2026-10-28-NEWTG漏洞报告复核与整改方案.md R-03。
//
// 语义（与 B10/B11/B12 的准入桶刻意分开）：
//   - 只有**鉴权失败**才计数（RecordAuthFailure）；成功请求不计数、不清零，
//     防止攻击者用有效令牌把失败计数洗白。
//   - 计数是 60 秒固定窗口；窗口内达到 60 次失败时写入独立锁键，锁 10 分钟。
//     锁与计数分键，避免计数器过期导致提前解锁（反之锁不会阻断计数窗口滚动，
//     因为锁定期内请求在鉴权之前就被拒绝，不会再调用 RecordAuthFailure）。
//   - 本包不决定 Redis 故障时的策略：两个方法都把错误原样上抛，由调用方
//     fail-open（R-03 明确这是唯一允许 fail-open 的限流点）。
//
// 算法复用 fixedWindowScript / retryAfter（与 B10/B11/B12 同一实现），不引入
// 新的存储抽象或脚本。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// R-03 阈值。刻意是编译期常量而不是设置键：这是安全基线（令牌 285 bit，
// 60 次/分钟足以压住撞库），不希望通过配置中心被无意放宽或关闭。
const (
	// AuthFailureLimit 是单个 IP 在 AuthFailureWindow 内允许的失败次数；
	// 第 AuthFailureLimit 次失败即上锁，下一次请求（第 limit+1 次）返回 429。
	AuthFailureLimit = 60
	// AuthFailureWindow 是失败计数窗口（固定 1 分钟，与安全设计一致）。
	AuthFailureWindow = time.Minute
	// AuthFailureLockTTL 是超限后的锁定时长。
	AuthFailureLockTTL = 10 * time.Minute
)

// AuthFailureBlocked 报告 clientIP 当前是否处于超限锁定期，并返回剩余时长。
//
// 只读：不创建、不续期计数键。调用方在**进入鉴权之前**调用它，超限时直接
// 429——因此被锁定的请求不会查库、不会哈希令牌。
//
// Redis 错误原样上抛（fail-open 由调用方决定并打 WARN）。
func (l *Limiter) AuthFailureBlocked(ctx context.Context, clientIP string) (bool, time.Duration, error) {
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		return false, 0, errors.New("ratelimit: empty client ip")
	}
	if l == nil || l.rdb == nil {
		return false, 0, errors.New("ratelimit: nil redis client")
	}
	key := AuthFailureLockKey(clientIP)
	ttl, err := l.rdb.PTTL(ctx, key).Result()
	if err != nil {
		return false, 0, fmt.Errorf("ratelimit: auth failure lock: %w", err)
	}
	switch ttl {
	case -2:
		// 键不存在：未锁定。
		return false, 0, nil
	case -1:
		// 键存在但无过期时间（手工写入/异常数据）：必须修复成有限 TTL，
		// 否则会变成永久封禁（与 fixedWindowScript 的 A11 修复同口径）。
		// 修复是 best-effort：失败也不能把「已锁定」变成放行。
		_ = l.rdb.PExpire(ctx, key, AuthFailureLockTTL).Err()
		return true, AuthFailureLockTTL, nil
	}
	return true, retryAfter(ttl, AuthFailureLockTTL), nil
}

// RecordAuthFailure 记录一次**鉴权失败**，并在窗口内达到阈值时写入 10 分钟锁。
//
// 只应在令牌校验确实失败（不存在/禁用/过期等用户侧事实）后调用；存储故障、
// 网关接线故障不算失败，否则会把基础设施问题记到客户端 IP 头上。
//
// 返回 error 时调用方只能记 WARN 并继续原来的拒绝响应——R-03 要求限流自身
// 异常不得把正常请求打成 500，也不得改变 401 语义。
func (l *Limiter) RecordAuthFailure(ctx context.Context, clientIP string) error {
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		return errors.New("ratelimit: empty client ip")
	}
	if l == nil || l.rdb == nil {
		return errors.New("ratelimit: nil redis client")
	}
	key := AuthFailureKey(clientIP)
	count, _, err := fixedWindow(ctx, l.rdb, key, AuthFailureWindow, 1)
	if err != nil {
		return err
	}
	if count < int64(AuthFailureLimit) {
		return nil
	}
	// 达到/超过阈值：上锁。SET 自带 TTL，避免锁键成为永久桶。
	if err := l.rdb.Set(ctx, AuthFailureLockKey(clientIP), "1", AuthFailureLockTTL).Err(); err != nil {
		return fmt.Errorf("ratelimit: set auth failure lock: %w", err)
	}
	return nil
}
