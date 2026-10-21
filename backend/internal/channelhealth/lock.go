package channelhealth

// W7/M4-b：健康体检单轮的分布式锁（R-M4-13 / modules-06 F-06-12）。
//
// 语义：多副本部署时同一轮体检只允许一个执行者。锁用 Redis `SET NX PX`
// 抢占，compare-and-delete 释放；拿不到锁的副本**跳过本轮**（不是排队等待），
// 避免并发探测把上游流量翻倍、也避免两轮同时写 last_test_*。
//
// TTL 由调用方按「本轮最坏时长」计算（见 worker.roundTTL）：TTL 短于单轮
// 时长会让第二个副本在第一个还没跑完时就抢到锁，产生重复出站请求。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// ChannelHealthCheckLockKey 是体检锁键（modules-06 F-06-12 / 调研 §2 #21）。
// 固定键名而不是按渠道分片：一轮体检是一个整体，只有整轮互斥才能保证
// last_test_* 的写入者唯一。
const ChannelHealthCheckLockKey = "channel_health_check_lock"

// healthLockReleaseTimeout 是释放锁的独立预算：停机时调用方的 ctx 可能
// 已取消，仍要尽量主动释放（TTL 只是兜底）。
const healthLockReleaseTimeout = 2 * time.Second

// RoundLocker 是单轮租约锁的接缝（生产 RedisLocker；测试可注入 fake 验证
// 「锁竞争只跑一轮」与「锁错误只告警」）。
type RoundLocker interface {
	// TryLock 尝试抢占 key；acquired=false 表示已被其它副本持有。
	// release 必须幂等且在锁已被 TTL 回收/他人持有时不误删（compare-and-delete）。
	TryLock(ctx context.Context, key string, ttl time.Duration) (release func(), acquired bool, err error)
}

// RedisLocker 是 RoundLocker 的 Redis 实现。
type RedisLocker struct {
	client *redis.Client
	log    *slog.Logger
}

// NewRedisLocker 构造锁实现。client 为 nil 时 TryLock 返回错误（装配层
// 负责把该错误当作「本轮跳过」而不是启动失败）。
func NewRedisLocker(client *redis.Client) *RedisLocker {
	return &RedisLocker{client: client, log: slog.Default()}
}

// WithLogger 注入结构化日志（nil 保持 slog.Default）。
func (l *RedisLocker) WithLogger(log *slog.Logger) *RedisLocker {
	if log != nil {
		l.log = log
	}
	return l
}

// TryLock 抢占本轮租约。
func (l *RedisLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	if l == nil || l.client == nil {
		return nil, false, errors.New("channelhealth: redis locker is not configured")
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	token, err := newLockToken()
	if err != nil {
		return nil, false, fmt.Errorf("channelhealth: generate lock token: %w", err)
	}
	acquired, err := l.client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("channelhealth: acquire lock %s: %w", key, err)
	}
	if !acquired {
		return nil, false, nil
	}
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), healthLockReleaseTimeout)
		defer cancel()
		// compare-and-delete：TTL 恰在释放前过期、另一副本已抢到锁时，
		// 直接 DEL 会删掉别人的租约。
		if err := releaseLockScript.Run(releaseCtx, l.client, []string{key}, token).Err(); err != nil {
			l.log.Warn("channelhealth: release health check lock failed (TTL will reclaim it)",
				"key", key, "error", err)
		}
	}
	return release, true, nil
}

var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

// newLockToken 生成跨副本唯一的持有者令牌（16 字节随机）。
func newLockToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
