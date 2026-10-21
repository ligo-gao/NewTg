package ratelimit

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// fixedWindowScript 原子完成「自增 + 首次设置过期 + 读取剩余 TTL」。
//
// 从 httpapi/rate_limit.go（基线 rateLimit:v2 实现）原样迁入，R-M5-01 冻结
// 的唯一算法：
//   - 单脚本避免 INCR 成功而 EXPIRE 失败导致计数永不过期；
//   - PTTL 与 INCR 在同一原子单元内读取，Retry-After 不会与窗口重置竞态；
//   - 键存在但无 TTL（异常数据/手工写入）时补设 TTL，避免永久桶（A11）。
//
// ARGV[1]=窗口毫秒数，ARGV[2]=本次消耗名额（cost）。cost=1 时 `n == cost`
// 就是「本窗口第一次自增」，与基线的 `n == 1` 等价。
var fixedWindowScript = redis.NewScript(`
local n = redis.call('INCRBY', KEYS[1], ARGV[2])
if n == tonumber(ARGV[2]) then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

// retryAfter 把脚本返回的 PTTL 归一为「窗口剩余时间」：
// min(PTTL, window)，再向上取整到整秒（最小 1s，与 httpapi.retrySeconds 同口径）。
//
// 用真实 PTTL 而不是 window 常量，是因为窗口起点是桶首次自增时刻：
// 窗口中途到达的请求应只等剩余时间（A2/A3）。
func retryAfter(ttl, window time.Duration) time.Duration {
	retry := window
	if ttl > 0 && ttl < window {
		retry = ttl
	}
	seconds := (retry + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	return seconds * time.Second
}
