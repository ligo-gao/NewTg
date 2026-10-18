// Package platform 是单二进制 newtg 的三种运行角色入口（M0 运行时装配：
// logger / PG / Redis / metrics / 优雅停机 / 迁移执行器）。
package platform

import (
	"context"

	"newtg/gateway/internal/config"
)

// Resource 是平台外部资源（PG 连接池 / Redis 客户端）的最小抽象。
// RunGateway / RunWorker 只依赖本接口，测试注入 stub 即可避免触真 PG/Redis（GC-8）。
type Resource interface {
	PingContext(ctx context.Context) error
	Close() error
}

// connectors 抽象 DB/Redis 建连步骤；生产路径由 prodConnectors 从 cfg 真实建连，
// 测试注入 stubConnectors（见 server_test.go）。
type connectors interface {
	openDB(ctx context.Context, cfg *config.Config) (Resource, error)
	openRedis(cfg *config.Config) (Resource, error)
}

// prodConnectors 生产实现：OpenDB / OpenRedis 从 cfg 真实建连并验证连通性。
type prodConnectors struct{}

func (prodConnectors) openDB(ctx context.Context, cfg *config.Config) (Resource, error) {
	return OpenDB(ctx, cfg)
}

func (prodConnectors) openRedis(cfg *config.Config) (Resource, error) {
	return OpenRedis(context.Background(), cfg)
}

// RunMigrate 由 migrate.go 实现（含 pg_advisory_lock 串行化）。
