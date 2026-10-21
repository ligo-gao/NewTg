// Package ratelimit 实现跨请求速率桶的原子固定窗口计数。
//
// 设计权威：docs/security-07-限流与滥用防护-设计.md §2.2.2/§2.2.3
// （键形状、单桶原子、跨副本一致性）与 docs/security-00-安全设计与ASVS对齐.md
// §4.1 F-1（RateLimiter 冻结接口：Allow(bucket, key, cost) -> Decision）。
//
// 算法（控制端裁决 R-M5-01）：唯一实现 = Redis 固定窗口 Lua。固定窗口在窗口
// 交界处允许约 2× 突刺；对「每分钟准入 cap」这是可信的设计权衡，且复用基线
// 已在生产验证的原子脚本（INCR+PEXPIRE+PTTL），M5 不引入令牌桶。
//
// fail 策略不在这里决定：Allow 只把 Redis 错误原样上抛，由每个调用面按
// security-07 §2.3 的逐面裁决选择 fail-open / fail-closed（中继面 R-M5-10
// fail-closed → 500，认证面 503，公开只读面 fail-open）。把策略写进共享
// helper 会让同一个底层脚本无法服务不同路由的不同 fail 语义。
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// FailMode 是桶在计数后端不可用时的期望策略（F-1 的 fail_mode）。
// 限流器本身不执行该策略，只把它回填到 Decision，供调用方决策。
type FailMode int

const (
	// FailClosed 表示后端故障时拒绝（中继面 B10–B17、认证面 B1/B3/B4/B5）。
	FailClosed FailMode = iota
	// FailOpen 表示后端故障时放行（公开只读面 B8/B9、支付 webhook）。
	FailOpen
)

// 桶名只用于日志/观测，不参与键构造，也不是 Redis 键的一部分。
// 取值与 security-07 §2.2.2 的桶语义一一对应，M5-b 的日志可直接复用。
const (
	BucketUserModel  = "user_model"  // B10：rateLimit:<userID>
	BucketToken      = "token"       // B11：rateLimit:token:<tokenID>
	BucketUserGlobal = "user_global" // B12：rateLimit:v2:user:GR:<userID>
)

// Bucket 描述一次准入检查的阈值、窗口与失败策略（F-1 的语言无关形态的 Go 版）。
type Bucket struct {
	Name     string
	Limit    int
	Window   time.Duration
	FailMode FailMode
}

// Decision 是一次准入检查的结果。
//
// RetryAfter 始终回填当前窗口剩余时间（已向上取整到整秒，最小 1s）；
// 只有 Allowed=false 时才应写入 Retry-After 头，限制由调用方做。
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	FailMode   FailMode
}

// Limiter 是 Redis 固定窗口限流器。零值不可用，必须先经 New/NewWithAddr 构造。
type Limiter struct {
	rdb *redis.Client
	own bool // 是否由本实例创建并负责 Close 的客户端
}

// New 用调用方提供的 Redis 客户端构造 Limiter。客户端生命周期仍归调用方，
// Close 不会关闭它——这样生产侧可以复用装配好的连接池，测试侧可以把两个
// Limiter 指向同一个 miniredis 来模拟跨副本共享计数（A7）。
func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb}
}

// NewWithAddr 用地址构造 Limiter 并接管客户端生命周期（Close 会关闭它）。
// 主要给测试/独立工具注入 miniredis 地址使用；生产装配应复用既有客户端。
func NewWithAddr(addr string) (*Limiter, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("ratelimit: empty redis address")
	}
	return &Limiter{rdb: redis.NewClient(&redis.Options{Addr: addr}), own: true}, nil
}

// Close 释放本实例拥有的 Redis 客户端；注入的客户端不受影响。
func (l *Limiter) Close() error {
	if l == nil || !l.own || l.rdb == nil {
		return nil
	}
	return l.rdb.Close()
}

// Allow 消耗固定窗口中的一个名额并返回准入决定。
//
// 语义：
//   - limit<=0 表示「不限」（modules-08 F-08-16；R-M5-06 的分组覆盖值 0
//     同样是「该组不限」）：不建键、不访问 Redis，直接放行。若把 0 当阈值
//     传给脚本，第一个请求就会因 n>0 被拒——「不限」会变成「全拒」。
//   - Allowed = n <= limit，即第 limit 次通过、第 limit+1 次拒绝；被拒请求
//     仍已自增（计数口径含失败请求，relay-01 §4）。
//   - Redis 错误原样上抛（不吞、不降级为内存计数）。
//
// cost 是本次消耗的名额数；M5 只使用 cost=1，但接口按 F-1 保留该形参，
// 以免未来成功数桶/加权桶再复制一份脚本。cost<=0 不消耗名额，直接放行。
func (l *Limiter) Allow(ctx context.Context, bucket Bucket, key string, cost int) (Decision, error) {
	window := bucket.Window
	if window <= 0 {
		// 窗口是契约常量（security-07：固定 1 分钟，无窗口时长设置键）。
		// 零值说明调用方漏填；回落到 1 分钟，避免 PEXPIRE 0 把键立刻删掉
		// 从而让桶「永不计数」。
		window = time.Minute
	}
	decision := Decision{Allowed: true, FailMode: bucket.FailMode}
	if bucket.Limit <= 0 || cost <= 0 {
		return decision, nil
	}
	if key == "" {
		// 键是服务端鉴权上下文构造的（F-1 约束），空键说明接线错误；
		// 返回 error 让调用方按自己的 fail 策略处理，而不是静默共享一个空桶。
		return Decision{}, errors.New("ratelimit: empty bucket key")
	}
	if l == nil || l.rdb == nil {
		return Decision{}, errors.New("ratelimit: nil redis client")
	}

	count, ttl, err := fixedWindow(ctx, l.rdb, key, window, cost)
	if err != nil {
		return Decision{}, err
	}
	decision.Allowed = count <= int64(bucket.Limit)
	decision.RetryAfter = retryAfter(ttl, window)
	return decision, nil
}

func fixedWindow(ctx context.Context, rdb *redis.Client, key string, window time.Duration, cost int) (int64, time.Duration, error) {
	values, err := fixedWindowScript.Run(ctx, rdb, []string{key},
		window.Milliseconds(), cost).Int64Slice()
	if err != nil {
		return 0, 0, fmt.Errorf("ratelimit: fixed window: %w", err)
	}
	if len(values) != 2 {
		return 0, 0, fmt.Errorf("ratelimit: fixed window: unexpected reply length %d", len(values))
	}
	return values[0], time.Duration(values[1]) * time.Millisecond, nil
}
