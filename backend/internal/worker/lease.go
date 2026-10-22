package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis 租约键前缀。
//
// 与 DB 的 worker_task_locks 是「同一语义的两层」：Redis 提供跨副本的即时
// 互斥（SET NX PX，崩溃后 TTL 自动释放），DB 行是权威的持有者/代际记录
// （billing-06 §2.3.1 与 §2.3.3）。Redis 故障时调度器退回纯 DB CAS，不会
// 出现「Redis 挂了所有任务都停摆」。
const leaseKeyPrefix = "newtg:worker:lease:"

// LeaseKey 返回某任务类型的 Redis 租约键（导出供测试/运维观测）。
func LeaseKey(taskType string) string { return leaseKeyPrefix + taskType }

// Lease 是跨副本互斥接缝。
type Lease interface {
	// Acquire 尝试占用；false 表示其他持有者仍有效（不应继续执行）。
	Acquire(ctx context.Context, taskType, holder string, ttl time.Duration) (bool, error)
	// Renew 仅在自己的 token 仍命中时续期；false 表示租约已被他人接管，
	// 调用方必须取消执行上下文（§2.3.1 心跳失败语义）。
	Renew(ctx context.Context, taskType, holder string, ttl time.Duration) (bool, error)
	// Release 仅删除自己的 token（比较后删除，避免误删新持有者的锁）。
	Release(ctx context.Context, taskType, holder string) error
	// Active 表示该键当前存在（管理员触发时的「正在运行」判定）。
	Active(ctx context.Context, taskType string) (bool, error)
}

// RedisLease 是 Lease 的 Redis 实现。
type RedisLease struct {
	client *redis.Client
}

// NewRedisLease 构造 Redis 租约。client 为 nil 时返回一个「永远可获取」的
// 空实现：单测/无 Redis 部署退回纯 DB CAS（DB 仍是权威）。
func NewRedisLease(client *redis.Client) Lease {
	if client == nil {
		return nopLease{}
	}
	return &RedisLease{client: client}
}

func (l *RedisLease) Acquire(ctx context.Context, taskType, holder string, ttl time.Duration) (bool, error) {
	ok, err := l.client.SetNX(ctx, LeaseKey(taskType), holder, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("worker: redis lease acquire %q: %w", taskType, err)
	}
	return ok, nil
}

// renewScript 是「比较 + 续期」的原子操作。不能用 GET 后再 PEXPIRE：
// 两者之间另一副本可能已经抢到（TTL 恰好过期），GET 读到的仍是自己的旧值，
// 会把别人的新租约续成自己的有效期。
var renewScript = redis.NewScript(`
if redis.call('get', KEYS[1]) == ARGV[1] then
  return redis.call('pexpire', KEYS[1], ARGV[2])
end
return 0
`)

func (l *RedisLease) Renew(ctx context.Context, taskType, holder string, ttl time.Duration) (bool, error) {
	res, err := renewScript.Run(ctx, l.client, []string{LeaseKey(taskType)},
		holder, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("worker: redis lease renew %q: %w", taskType, err)
	}
	return res == 1, nil
}

// releaseScript 是比较后删除；直接 DEL 会误删他人刚抢到的租约。
var releaseScript = redis.NewScript(`
if redis.call('get', KEYS[1]) == ARGV[1] then
  return redis.call('del', KEYS[1])
end
return 0
`)

func (l *RedisLease) Release(ctx context.Context, taskType, holder string) error {
	_, err := releaseScript.Run(ctx, l.client, []string{LeaseKey(taskType)}, holder).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("worker: redis lease release %q: %w", taskType, err)
	}
	return nil
}

func (l *RedisLease) Active(ctx context.Context, taskType string) (bool, error) {
	n, err := l.client.Exists(ctx, LeaseKey(taskType)).Result()
	if err != nil {
		return false, fmt.Errorf("worker: redis lease active %q: %w", taskType, err)
	}
	return n > 0, nil
}

// nopLease 让「没有 Redis」成为显式的降级路径：Acquire 恒真，真正的互斥
// 由 DB 的 generation CAS 承担（§2.3.1 是权威，Redis 只是纵深）。
type nopLease struct{}

func (nopLease) Acquire(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (nopLease) Renew(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (nopLease) Release(context.Context, string, string) error { return nil }
func (nopLease) Active(context.Context, string) (bool, error)  { return false, nil }
