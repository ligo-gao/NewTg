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

// connectors 抽象 DB/Redis 建连与启动期迁移步骤；生产路径由 prodConnectors 从
// cfg 真实建连/迁移，测试注入 stubConnectors（见 server_test.go）。
//
// runMigrate 作为接口方法而非直接调用包级 RunMigrate，是为了让 runGateway 的
// 「DB_MIGRATE_ON_START 是否触发迁移」可在不触真 PG 的前提下单测（M0 评审 Task 6）：
// 生产实现只做一次转发，行为与直接调用完全一致。
type connectors interface {
	openDB(ctx context.Context, cfg *config.Config) (Resource, error)
	openRedis(cfg *config.Config) (Resource, error)
	runMigrate(ctx context.Context, cfg *config.Config) error
}

// prodConnectors 生产实现：OpenDB / OpenRedis 从 cfg 真实建连并验证连通性。
type prodConnectors struct{}

func (prodConnectors) openDB(ctx context.Context, cfg *config.Config) (Resource, error) {
	return OpenDB(ctx, cfg)
}

func (prodConnectors) openRedis(cfg *config.Config) (Resource, error) {
	return OpenRedis(context.Background(), cfg)
}

// runMigrate 生产实现直接复用 RunMigrate（含 pg_advisory_lock 串行化）。
func (prodConnectors) runMigrate(ctx context.Context, cfg *config.Config) error {
	return RunMigrate(ctx, cfg)
}

// RunMigrate 由 migrate.go 实现（含 pg_advisory_lock 串行化）。
