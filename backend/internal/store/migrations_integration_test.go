//go:build integration

package store

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

	"newtg/gateway/migrations"
)

// 本文件验证 W8/MJ-A0 迁移 20261023090000_tasks_mj_family 的 up/down 往返。
//
// 为什么不直接在当前测试库上 down 一步：schema_integration_test 的其余用例
// 依赖 tasks 的**最新**形状，回退会污染同一轮测试。这里在测试库里建一个
// 临时 schema，并通过 DSN 的 search_path 让 golang-migrate 把全部表
// （含 schema_migrations）建在该 schema 内；结束后 DROP SCHEMA CASCADE。
//
// 这个用例专门捕获「只改 up、不改 down」的回归：
//   - down 若为空：m.Steps(-1) 成功但 CHECK 仍是 {plugin,mj}，
//     随后「down 后插入 mj 必须失败」的断言会红；
//   - down 若恢复 CHECK 但漏掉 DELETE mj 行：ADD CONSTRAINT 会因存量行
//     违约而失败，m.Steps(-1) 直接红。
func TestTasksMJFamilyMigrationUpDown(t *testing.T) {
	baseDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	if baseDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin db: %v", err)
	}
	// 标识符由时间戳生成，只含数字，无注入面。
	schema := fmt.Sprintf("mja0_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	dsn, err := dsnWithSearchPath(baseDSN, schema)
	if err != nil {
		t.Fatalf("build scoped dsn: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}

	m := newTestMigrator(t, db)
	t.Cleanup(func() { _, _ = m.Close() })

	// 只前滚到本用例要验证的版本，而不是全部迁移：后续迁移（如 W10-N9 的
	// 终态保留索引）会排在它之后，`Steps(-1)` 就不再指向 tasks_mj_family，
	// 让断言误判。固定目标是让用例只对这个迁移的 up/down 负责。
	if err := m.Migrate(20261023090000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261023090000: %v", err)
	}

	// up 后：family='mj' 可写入，未知值仍被 CHECK 拒绝，领取索引存在。
	execTaskInsert(t, db, "mig_mj_before_down", "mj")
	if err := execTaskInsertErr(db, "mig_bogus", "bogus"); err == nil {
		t.Fatal("unknown family must be rejected by the extended CHECK")
	}
	if !indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("tasks_family_pending_idx missing after up")
	}

	// down 一步：必须先删 MJ 行（否则 CHECK 恢复失败），再恢复单值 CHECK。
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	var mjRows int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE family = 'mj'`).Scan(&mjRows); err != nil {
		t.Fatalf("count mj rows: %v", err)
	}
	if mjRows != 0 {
		t.Fatalf("down must remove mj rows before restoring the single-value CHECK, left %d", mjRows)
	}
	if err := execTaskInsertErr(db, "mig_mj_after_down", "mj"); err == nil {
		t.Fatal("down must restore family CHECK to {plugin}")
	}
	if indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("down must drop tasks_family_pending_idx")
	}
	// 插件行在 down 后仍可写（CHECK 没有误伤合法族）。
	execTaskInsert(t, db, "mig_plugin_after_down", "plugin")

	// up 一步：迁移可反转（down 后再次前滚仍接受 mj）。
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1: %v", err)
	}
	execTaskInsert(t, db, "mig_mj_after_up", "mj")
	if !indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("tasks_family_pending_idx missing after re-up")
	}
}

// dsnWithSearchPath 在 DSN 上追加 search_path 运行时参数。
//
// 保留原 query（sslmode 等）并覆盖 search_path：pgx 把它作为会话参数下发，
// 因此无 schema 限定的 DDL 与 CURRENT_SCHEMA() 都落在临时 schema 内。
func dsnWithSearchPath(base, schema string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func newTestMigrator(t *testing.T, db *sql.DB) *migrate.Migrate {
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

func execTaskInsert(t *testing.T, db *sql.DB, publicID, family string) {
	t.Helper()
	if err := execTaskInsertErr(db, publicID, family); err != nil {
		t.Fatalf("insert task (family=%s): %v", family, err)
	}
}

func execTaskInsertErr(db *sql.DB, publicID, family string) error {
	_, err := db.Exec(`INSERT INTO tasks
		(public_task_id, family, action, plugin_key, fetch_mode, state, user_id)
		VALUES ($1, $2, 'generate', '', 'poll', 'submitted', 1)`, publicID, family)
	return err
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("query index %s: %v", name, err)
	}
	return n > 0
}
