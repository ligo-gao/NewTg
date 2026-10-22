//go:build integration

package platform

// W17-2 三表迁移的往返/索引/默认值验收（billing-06 §2.4）。
//
// 为什么在临时 schema 里跑全链迁移：schema_integration_test 的其余用例依赖
// 最新形状，直接在测试库上 down 一步会污染同轮测试。这里用 DSN 的
// search_path 把全部表（含 schema_migrations）建到临时 schema，结束时
// DROP SCHEMA CASCADE。
//
// 断言：
//   - 三表存在且列数固定（9/7/11）；
//   - 「按任务/按状态/按时间」三条运行历史索引存在；
//   - 列默认值（status=pending、trigger=schedule、enabled=true、interval=0）
//     与设计一致；
//   - down 后三表消失，再 up 后原样恢复（迁移往返可执行且幂等）。

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/migrations"
)

func workerMigrationDSN(base, schema string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func newWorkerMigrator(t *testing.T, db *sql.DB) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open embedded migrations: %v", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		t.Fatalf("create migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		t.Fatalf("create migrator: %v", err)
	}
	return m
}

func TestWorkerTaskTablesMigrationUpDown(t *testing.T) {
	baseDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	if baseDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Fatalf("ping admin db: %v", err)
	}
	schema := fmt.Sprintf("w17b_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	dsn, err := workerMigrationDSN(baseDSN, schema)
	if err != nil {
		t.Fatalf("scoped dsn: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m := newWorkerMigrator(t, db)
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	wantColumns := map[string]map[string]string{
		"worker_tasks": {
			"interval_seconds": "0",
			"enabled":          "true",
			"shard":            "0",
		},
		"worker_task_locks": {
			"generation": "0",
		},
		"worker_task_runs": {
			"status":  "'pending'::text",
			"trigger": "'schedule'::text",
		},
	}
	wantCounts := map[string]int{"worker_tasks": 9, "worker_task_locks": 7, "worker_task_runs": 11}
	for table, columns := range wantColumns {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&count); err != nil {
			t.Fatalf("count columns %s: %v", table, err)
		}
		if count != wantCounts[table] {
			t.Fatalf("%s 列数 = %d，期望 %d", table, count, wantCounts[table])
		}
		for column, want := range columns {
			var got sql.NullString
			if err := db.QueryRow(`SELECT column_default FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
				table, column).Scan(&got); err != nil {
				t.Fatalf("read default %s.%s: %v", table, column, err)
			}
			if !got.Valid || got.String != want {
				t.Fatalf("%s.%s default = %v，期望 %s", table, column, got, want)
			}
		}
	}

	for _, index := range []string{
		"worker_tasks_due_idx",
		"worker_task_locks_expiry_idx",
		"worker_task_runs_task_started_idx",
		"worker_task_runs_status_started_idx",
		"worker_task_runs_started_idx",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1`, index).Scan(&count); err != nil {
			t.Fatalf("query index %s: %v", index, err)
		}
		if count != 1 {
			t.Fatalf("索引 %s 不存在", index)
		}
	}

	// down：只回退本迁移（全链最新就是它）。
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down one step: %v", err)
	}
	for _, table := range []string{"worker_tasks", "worker_task_locks", "worker_task_runs"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&count); err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("down 后 %s 仍存在", table)
		}
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	for table, want := range wantCounts {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&count); err != nil {
			t.Fatalf("re-count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("往返后 %s 列数 = %d，期望 %d", table, count, want)
		}
	}
}
