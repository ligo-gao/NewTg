package httpapi

// R-03：/v1 预认证失败限流的接缝与客户端 IP 解析。
//
// 设计权威：docs/2026-10-28-NEWTG漏洞报告复核与整改方案.md R-03。
//
// 存储实现复用 internal/ratelimit（同一 Redis 客户端与固定窗口脚本），
// 这里只定义中间件需要的窄接口，便于单测注入而无需真实 Redis。

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"newtg/gateway/internal/ratelimit"
)

// relayAuthFailureCheckTimeout 是预认证失败桶查询的等待上限。
//
// Redis 故障时 go-redis 默认会重试并等待数秒；fail-open 若以「慢」为代价，
// 就等于把限流故障扩散成全局延迟。250ms 远大于正常 PTTL 往返，同时保证
// 故障期每个请求很快回到正常鉴权流程。
const relayAuthFailureCheckTimeout = 250 * time.Millisecond

// RelayAuthFailureLimiter 是 R-03 预认证失败限流接缝。
//
// 两个方法都必须在实现层把 Redis 错误原样上抛；fail-open 的决策与 WARN
// 日志只在 relayAuthWithGuard 里做一次（全仓唯一允许 fail-open 的限流点）。
type RelayAuthFailureLimiter interface {
	// AuthFailureBlocked 只读地报告 clientIP 是否处于超限锁定期，
	// 并返回剩余时长（用于 Retry-After）。
	AuthFailureBlocked(ctx context.Context, clientIP string) (bool, time.Duration, error)
	// RecordAuthFailure 记录一次确定的鉴权失败；达到阈值时上锁。
	RecordAuthFailure(ctx context.Context, clientIP string) error
}

// authFailureLimiterOf 复用既有的 RateLimiter 装配点启用 R-03。
//
// 生产的 *ratelimit.Limiter 同时实现 Allow 与失败桶方法；只实现 Allow 的
// 轻量测试 fake 会得到 nil，即跳过 R-03。这样无需在 assembly 里新增字段，
// 也避免为所有既有 fake 补齐两个方法。
func authFailureLimiterOf(limiter RelayRateLimiter) RelayAuthFailureLimiter {
	guard, ok := limiter.(RelayAuthFailureLimiter)
	if !ok {
		return nil
	}
	return guard
}

// relayAuthClientIP 用 clientip 的唯一取值路径解析客户端地址（TRUSTED_PROXIES
// 生效），并仅在结果确实是 IP 时返回。
//
// 不自己读 X-Forwarded-For：信任边界统一由 internal/clientip 决定。解析不出
// IP（Unix socket/异常 RemoteAddr/畸形 XFF）时跳过失败桶——宁可不限流，也
// 不能把不同来源塞进同一个键或误伤。
func relayAuthClientIP(r *http.Request, trustedProxies []netip.Prefix) (string, bool) {
	addr, err := netip.ParseAddr(resolveClientIP(r, trustedProxies))
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}

// 编译期确认生产限流器满足 R-03 接缝（type assertion 的前提）。
var _ RelayAuthFailureLimiter = (*ratelimit.Limiter)(nil)
