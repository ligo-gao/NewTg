//go:build integration

package platform

// W10-METRICS 集成验收：真实 PostgreSQL + Redis 下的依赖/迁移/连接池指标。
//
// 运行（测试库按任务约定自建 newtg_dom_w10m / newtg_dom_w10m_logs）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10m?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10m_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration -p 1 ./internal/platform/ -run TestIntegrationMetricsProbe -v

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/metrics"
)

func TestIntegrationMetricsProbe(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if dsn == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}

	mainDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	// Close 必须走 t.Cleanup 而不是 defer：恢复 schema_migrations 的 cleanup
	// 注册得比本行晚，LIFO 下会先执行恢复；若用 defer Close，连接池会在
	// 恢复之前关闭。
	t.Cleanup(func() { _ = mainDB.Close() })
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(redisMetricsHook{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 迁移版本表由 migrate 创建；集成库是自建的空库，这里写入一行固定版本，
	// 让探针的读取路径（SELECT version, dirty）可被真实验证。
	if _, err := mainDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint NOT NULL PRIMARY KEY,
			dirty boolean NOT NULL
		)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	// 复核发现的基础设施缺陷：原实现 DELETE+INSERT fixture 且不还原，同一
	// 隔离库的第二次运行会读到伪造版本 20261021000001（/readyz 报
	// migration=fail），集成门禁不可重跑。这里先快照真实迁移行，cleanup
	// 时原样恢复（含 dirty 位），使本用例对同一库可重复执行。
	originalMigrations := snapshotSchemaMigrations(t, ctx, mainDB)
	t.Cleanup(func() {
		restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRestore()
		if _, err := mainDB.ExecContext(restoreCtx, `DELETE FROM schema_migrations`); err != nil {
			t.Logf("restore schema_migrations delete: %v", err)
			return
		}
		for _, row := range originalMigrations {
			if _, err := mainDB.ExecContext(restoreCtx,
				`INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`,
				row.version, row.dirty); err != nil {
				t.Logf("restore schema_migrations row %d: %v", row.version, err)
			}
		}
	})

	if _, err := mainDB.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	if _, err := mainDB.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, dirty) VALUES ($1, false)`,
		int64(20261021000001)); err != nil {
		t.Fatalf("seed schema_migrations: %v", err)
	}

	go startMetricsProbe(ctx, mainDB, logsDB, rdb, testMetricsLogger())

	// Redis 命令耗时由 Hook 产生：一次 PING 应当出现 cmd="ping"。
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}

	waitForGauge(t, "newtg_dependency_up", map[string]string{"dependency": "postgres"}, 1)
	waitForGauge(t, "newtg_dependency_up", map[string]string{"dependency": "redis"}, 1)
	waitForGauge(t, "newtg_redis_up", nil, 1)
	waitForGauge(t, "newtg_migration_version",
		map[string]string{"version": "20261021000001", "dirty": "false"}, 1)
	waitForGauge(t, "newtg_db_pool_connections",
		map[string]string{"db": "main", "state": "open"}, 1) // 至少 1（探针持有连接）
	waitForGauge(t, "newtg_db_pool_connections",
		map[string]string{"db": "logs", "state": "open"}, 0) // 允许 0，但 family 必存在

	if findFamily(t, "newtg_redis_command_duration_seconds") == nil {
		t.Fatal("newtg_redis_command_duration_seconds missing after PING")
	}
}

// schemaMigrationRow 是 schema_migrations 的一行快照（版本 + dirty）。
type schemaMigrationRow struct {
	version int64
	dirty   bool
}

// snapshotSchemaMigrations 读取真实迁移状态，供用例结束原样恢复。
func snapshotSchemaMigrations(t *testing.T, ctx context.Context, db *sql.DB) []schemaMigrationRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT version, dirty FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("snapshot schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []schemaMigrationRow
	for rows.Next() {
		var row schemaMigrationRow
		if err := rows.Scan(&row.version, &row.dirty); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot schema_migrations rows: %v", err)
	}
	return out
}

func waitForGauge(t *testing.T, name string, labels map[string]string, wantAtLeast float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := gaugeValue(t, name, labels); ok && v >= wantAtLeast {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gauge %s%v did not reach >= %v", name, labels, wantAtLeast)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func gaugeValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	fam := findFamily(t, name)
	if fam == nil {
		return 0, false
	}
	for _, m := range fam.Metric {
		match := true
		for wantName, wantValue := range labels {
			found := false
			for _, pair := range m.Label {
				if pair.GetName() == wantName {
					found = pair.GetValue() == wantValue
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match && m.GetGauge() != nil {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func findFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() == name {
			return fam
		}
	}
	return nil
}
