package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/config"
)

// redisResource 让 *redis.Client 满足 Resource 接口（Ping 语义由 OpenRedis 在建连时验证）。
type redisResource struct{ client *redis.Client }

func (r redisResource) PingContext(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func (r redisResource) Close() error { return r.client.Close() }

// OpenRedis 以 go-redis/v9 构造客户端（Task 4 冻结选型），PoolSize 来自
// RedisPoolSize；REDIS_TLS=on 时 RedisURL() 已切换为 rediss://，此处原样解析。
// 3s 内 Ping 验证，失败时关闭客户端不泄漏。
func OpenRedis(ctx context.Context, cfg *config.Config) (Resource, error) {
	opt, err := redis.ParseURL(cfg.RedisURL())
	if err != nil {
		return nil, fmt.Errorf("platform: parse redis url: %w", err)
	}
	opt.PoolSize = cfg.RedisPoolSize
	client := redis.NewClient(opt)
	// W10-METRICS：命令耗时是旁路观察，Hook 内部不得改变返回值/错误语义。
	client.AddHook(redisMetricsHook{})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		// 正在返回 Ping 的主错误；关闭客户端的失败是次要信息。
		_ = client.Close()
		return nil, fmt.Errorf("platform: ping redis: %w", err)
	}
	return redisResource{client}, nil
}
