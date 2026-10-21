package platform

// W7/M5-b 生产装配守卫（M5-b 复核 F1）：
// handler 把 nil limiter 解释为「跳过限流」，因此生产装配必须把「拿不到
// limiter」变成启动错误，而不是静默关掉 B11/B10/B12 三档桶。

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestNewRelayRateLimiterRequiresRedis(t *testing.T) {
	limiter, err := newRelayRateLimiter(nil)
	if err == nil {
		t.Fatal("newRelayRateLimiter(nil) error = nil，期望装配错误（不得静默返回 nil limiter）")
	}
	if limiter != nil {
		t.Fatalf("newRelayRateLimiter(nil) limiter = %v，期望 nil", limiter)
	}

	// 构造本身不建立连接：地址不可达也应当返回可用的 limiter，由请求期
	// 的 Redis 错误走中继 fail-closed（500 rate_limit_check_failed）。
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	limiter, err = newRelayRateLimiter(rdb)
	if err != nil {
		t.Fatalf("newRelayRateLimiter(rdb) error = %v，期望 nil", err)
	}
	if limiter == nil {
		t.Fatal("newRelayRateLimiter(rdb) = nil，生产装配必须注入非 nil limiter")
	}
}
