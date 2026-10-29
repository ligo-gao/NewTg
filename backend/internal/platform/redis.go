package platform

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/config"
)

// redisResource 让 *redis.Client 满足 Resource 接口（Ping 语义由 OpenRedis 在建连时验证）。
type redisResource struct{ client *redis.Client }

func (r redisResource) PingContext(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func (r redisResource) Close() error { return r.client.Close() }

// configureRedisTLS 在 rediss:// 时把 caPath 指向的 PEM 追加进系统根证书池。
//
// 跨机部署会用私有 CA 签 Redis 证书，而 go-redis 的 ParseURL 只用进程根证书
// 库校验、没有注入口；运行镜像又是非 root 的 alpine，改不了系统证书库，因此
// 必须从环境变量指一个文件进来。
//
// 三个分支都是配置错误，一律返回错误（fail-closed）：caPath 非空但 DSN 不是
// rediss、文件读不了、PEM 里没有 CA 证书。绝不能带着"空的根池"继续跑——
// 那会让所有 rediss 连接校验失败，错误信息还会把人引向网络问题。
func configureRedisTLS(opt *redis.Options, caPath string) error {
	if caPath == "" {
		return nil
	}
	if opt.TLSConfig == nil {
		return fmt.Errorf("platform: REDIS_TLS_CA is set (%s) but the Redis URL is not rediss://", caPath)
	}
	raw, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("platform: read REDIS_TLS_CA: %w", err)
	}
	hasCA := false
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("platform: parse REDIS_TLS_CA certificate: %w", err)
		}
		if cert.IsCA {
			hasCA = true
		}
	}
	if !hasCA {
		return fmt.Errorf("platform: REDIS_TLS_CA %s contains no CA certificate", caPath)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("platform: load system cert pool: %w", err)
	}
	if pool == nil {
		pool = x509.NewCertPool()
	}
	// 追加而不是替换：同一进程的出站 TLS（上游模型、Webhook）仍需要系统根证书。
	if !pool.AppendCertsFromPEM(raw) {
		return fmt.Errorf("platform: REDIS_TLS_CA %s contains no usable certificate", caPath)
	}
	cfg := opt.TLSConfig.Clone()
	cfg.RootCAs = pool
	opt.TLSConfig = cfg
	return nil
}

// OpenRedis 以 go-redis/v9 构造客户端（Task 4 冻结选型），PoolSize 来自
// RedisPoolSize；REDIS_TLS=on 时 RedisURL() 已切换为 rediss://，此处原样解析。
// 3s 内 Ping 验证，失败时关闭客户端不泄漏。
func OpenRedis(ctx context.Context, cfg *config.Config) (Resource, error) {
	opt, err := redis.ParseURL(cfg.RedisURL())
	if err != nil {
		return nil, fmt.Errorf("platform: parse redis url: %w", err)
	}
	if err := configureRedisTLS(opt, cfg.RedisURLCAPath()); err != nil {
		return nil, err
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
