package platform

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"newtg/gateway/internal/config"
)

// 本文件覆盖 runWorker 的资源类型/非 nil 守卫（R-41 生产装配保证）。
// 守卫位于 gorm.Open 与任何真实 IO 之前，因此用 sql.Open（不建连）即可
// 走到断言路径，不需要真实的 PG/Redis。

// workerGuardConnectors 注入主库/日志库句柄与任意 Redis Resource。
type workerGuardConnectors struct {
	db, logs *sql.DB
	redis    Resource
}

func (c workerGuardConnectors) openDB(context.Context, *config.Config) (Resource, error) {
	return c.db, nil
}

func (c workerGuardConnectors) openLogsDB(context.Context, *config.Config) (Resource, error) {
	return c.logs, nil
}

func (c workerGuardConnectors) openRedis(*config.Config) (Resource, error) {
	return c.redis, nil
}

func (c workerGuardConnectors) runMigrate(context.Context, *config.Config) error { return nil }

func (c workerGuardConnectors) newAPIHandler(context.Context, *config.Config,
	Resource, Resource, Resource, *slog.Logger) (http.Handler, error) {
	return nil, nil
}

// newWorkerGuardDBs 构造两个未建连的 *sql.DB：sql.Open 只解析 DSN。
func newWorkerGuardDBs(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	const dsn = "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable"
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	logs, err := sql.Open("pgx", dsn)
	if err != nil {
		_ = db.Close()
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() {
		_ = logs.Close()
		_ = db.Close()
	})
	return db, logs
}

// assertClosed 断言 *sql.DB 已被 runWorker 的失败清理路径关闭。
func assertClosed(t *testing.T, db *sql.DB, label string) {
	t.Helper()
	if err := db.Ping(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("%s not closed after worker startup failure (Ping err = %v)", label, err)
	}
}

// TestRunWorkerRejectsNonRedisResource 验证 connectors 返回的 Redis Resource
// 类型不符时启动失败并释放已打开的主库/日志库。
func TestRunWorkerRejectsNonRedisResource(t *testing.T) {
	db, logs := newWorkerGuardDBs(t)
	err := runWorker(context.Background(), gatewayTestConfig(":0"), workerGuardConnectors{
		db: db, logs: logs, redis: &stubResource{name: "redis"},
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("runWorker = %v, want unexpected-type error", err)
	}
	assertClosed(t, db, "main db")
	assertClosed(t, logs, "logs db")
}

// TestRunWorkerRejectsNilRedisClient 验证 redisResource 携带 nil 客户端时
// 启动失败而不是在后续心跳/注册路径里 panic（R-41：引擎与注册表都必须
// 拿到非 nil 客户端）；失败清理不得解引用 nil 客户端。
func TestRunWorkerRejectsNilRedisClient(t *testing.T) {
	db, logs := newWorkerGuardDBs(t)
	err := runWorker(context.Background(), gatewayTestConfig(":0"), workerGuardConnectors{
		db: db, logs: logs, redis: redisResource{client: nil},
	})
	if err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("runWorker = %v, want nil-redis-client error", err)
	}
	assertClosed(t, db, "main db")
	assertClosed(t, logs, "logs db")
}
