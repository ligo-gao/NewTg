//go:build integration

package plugin

// 注册表热刷新与逐副本运行时状态的集成验收（临时隔离 PG + Redis）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_m6d_rt?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='...' NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	  go test -tags=integration ./internal/plugin/ -run TestPluginRuntime -p 1 -v
//
// 覆盖：两个 Manager 共用真实 Redis + 真实 SQLVersionLoader；A 广播后 B
// 从 DB 重建快照（而非本地缓存）；聚合包含两个副本并按 instance_id 排序；
// 停机删除状态 key。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

func openRuntimeIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openRuntimeIntegrationRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping test redis: %v", err)
	}
	return rdb
}

func runtimeIntegrationSource(key, version string) string {
	return fixture{Key: key, Version: version}.Source()
}

func TestPluginRuntimeHotReloadAndAggregateAcrossReplicas(t *testing.T) {
	db := openRuntimeIntegrationDB(t)
	rdb := openRuntimeIntegrationRedis(t)
	ctx := context.Background()

	key := fmt.Sprintf("it-rt-%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugin_versions WHERE "key" = $1`, key)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugins WHERE "key" = $1`, key)
		_, _ = rdb.Del(context.Background(),
			RuntimeStatusKey("it-replica-a"), RuntimeStatusKey("it-replica-b")).Result()
	})
	for _, query := range []string{
		`DELETE FROM task_plugin_versions WHERE "key" = $1`,
		`DELETE FROM task_plugins WHERE "key" = $1`,
	} {
		if _, err := db.ExecContext(ctx, query, key); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}

	sourceV1 := runtimeIntegrationSource(key, "1.0.0")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO task_plugins ("key", name, source) VALUES ($1, 'Runtime IT', 'upload')`, key); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO task_plugin_versions
		("key", version, source_text, source_hash, enabled, active, uploaded_by)
		VALUES ($1, '1.0.0', $2, $3, true, true, 1)`, key, sourceV1, sourceHash(sourceV1)); err != nil {
		t.Fatalf("insert v1: %v", err)
	}

	managerA := MustNewManager(nil, NewSQLVersionLoader(db), Options{MasterEnabled: true})
	managerB := MustNewManager(nil, NewSQLVersionLoader(db), Options{MasterEnabled: true})
	// interval 取长周期：对端必须由 pub/sub（而不是 ticker）触发 Reload。
	// 这同时证明 30s 周期是可注入的（测试不需要等待真实 30s）。
	coordinatorA, err := NewRuntimeCoordinator(RuntimeCoordinatorConfig{
		Manager: managerA, Redis: rdb, InstanceID: "it-replica-a", Role: "gateway",
		Version: "it", Interval: time.Hour, Logger: runtimeTestLogger(),
	})
	if err != nil {
		t.Fatalf("coordinator A: %v", err)
	}
	coordinatorB, err := NewRuntimeCoordinator(RuntimeCoordinatorConfig{
		Manager: managerB, Redis: rdb, InstanceID: "it-replica-b", Role: "worker",
		Version: "it", Interval: time.Hour, Logger: runtimeTestLogger(),
	})
	if err != nil {
		t.Fatalf("coordinator B: %v", err)
	}
	coordinatorA.Start(ctx)
	coordinatorB.Start(ctx)
	t.Cleanup(func() { coordinatorA.Stop(); coordinatorB.Stop() })
	// 确认订阅已建立后再依赖广播（避免 SUBSCRIBE 确认竞态）。
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	if err := coordinatorB.waitSubscribed(waitCtx); err != nil {
		t.Fatalf("replica B subscribe: %v", err)
	}

	eventually(t, 5*time.Second, "both replicas load v1", func() bool {
		return managerA.Snapshot().Lookup(key).Version == "1.0.0" &&
			managerB.Snapshot().Lookup(key).Version == "1.0.0"
	})
	generationB := managerB.Snapshot().Generation()

	// 本副本 A 已 Reload 并广播；B 收到后必须重读 DB 拿到 v2。
	sourceV2 := runtimeIntegrationSource(key, "2.0.0")
	if _, err := db.ExecContext(ctx,
		`UPDATE task_plugin_versions SET active = false WHERE "key" = $1 AND version = '1.0.0'`, key); err != nil {
		t.Fatalf("deactivate v1: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO task_plugin_versions
		("key", version, source_text, source_hash, enabled, active, uploaded_by)
		VALUES ($1, '2.0.0', $2, $3, true, true, 1)`, key, sourceV2, sourceHash(sourceV2)); err != nil {
		t.Fatalf("insert v2: %v", err)
	}
	if err := managerA.Reload(ctx); err != nil {
		t.Fatalf("replica A reload: %v", err)
	}
	if err := coordinatorA.PublishPluginChanged(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}

	eventually(t, 5*time.Second, "replica B reloads v2 via pub/sub", func() bool {
		return managerB.Snapshot().Lookup(key).Version == "2.0.0"
	})
	if managerB.Snapshot().Generation() <= generationB {
		t.Fatalf("B 的 generation 应递增：before=%d after=%d",
			generationB, managerB.Snapshot().Generation())
	}

	// 聚合：两个未过期副本 + 升序 + 本地标记只落在请求副本。
	eventually(t, 5*time.Second, "runtime status keys", func() bool {
		return mustRuntimeKey(t, rdb, "it-replica-a") && mustRuntimeKey(t, rdb, "it-replica-b")
	})
	aggregate := coordinatorB.Aggregate(ctx)
	if aggregate.AggregateUnavailable {
		t.Fatal("Redis 可用时 aggregate_unavailable 必须为 false")
	}
	if len(aggregate.Instances) < 2 {
		t.Fatalf("聚合应含两个副本：%+v", aggregate.Instances)
	}
	var order []string
	for _, instance := range aggregate.Instances {
		order = append(order, instance.InstanceID)
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] > order[i] {
			t.Fatalf("聚合未按 instance_id 升序：%v", order)
		}
	}
	localCount := 0
	for _, instance := range aggregate.Instances {
		if instance.InstanceID == "it-replica-b" {
			if !instance.Local {
				t.Fatalf("本地副本必须标记 local：%+v", instance)
			}
			localCount++
		}
	}
	if localCount != 1 {
		t.Fatalf("聚合中的本地副本应恰好一条：%+v", aggregate.Instances)
	}

	// 停机必须 DEL 状态 key（而不是等 TTL）。
	coordinatorA.Stop()
	if mustRuntimeKey(t, rdb, "it-replica-a") {
		t.Fatal("Stop 后状态 key 必须删除")
	}
}

func mustRuntimeKey(t *testing.T, rdb *redis.Client, id string) bool {
	t.Helper()
	exists, err := rdb.Exists(context.Background(), RuntimeStatusKey(id)).Result()
	if err != nil {
		t.Fatalf("exists %s: %v", id, err)
	}
	return exists > 0
}
