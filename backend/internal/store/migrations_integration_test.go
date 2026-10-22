//go:build integration

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
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

	// 只前滚到本用例要验证的两个迁移（family 090000 + MJ 索引 091000），
	// 而不是全部迁移：后续迁移（如 W10-N9 的终态保留索引）排在后面，会改变
	// down 步数的语义。固定目标让用例只对这两步的 up/down 负责。
	//
	// 目标必须包含 091000：本用例断言它创建的
	// `tasks_mj_user_upstream_idx`/`tasks_mj_upstream_idx` 在 up 后存在、
	// down 后消失。只前滚到 family 迁移（090000）时这两个索引尚未创建，
	// 断言会在正确的迁移链上稳定失败（2026-10-22 W8/MJ-c 集成门禁发现）。
	if err := m.Migrate(20261023091000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261023091000: %v", err)
	}

	// up 后：family='mj' 可写入，未知值仍被 CHECK 拒绝，领取索引存在。
	execTaskInsert(t, db, "mig_mj_before_down", "mj")
	if err := execTaskInsertErr(db, "mig_bogus", "bogus"); err == nil {
		t.Fatal("unknown family must be rejected by the extended CHECK")
	}
	if !indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("tasks_family_pending_idx missing after up")
	}
	// W8/MJ-a2 的归属索引在同一链上（family 迁移之后的一步）。
	if !indexExists(t, db, "tasks_mj_user_upstream_idx") || !indexExists(t, db, "tasks_mj_upstream_idx") {
		t.Fatal("MJ upstream indexes missing after up")
	}

	// down 两步：先回退 W8/MJ-a2 的索引迁移，再回退 family 迁移。family 的
	// down 必须先删 MJ 行（否则 CHECK 恢复失败），再恢复单值 CHECK。
	// 步数写成 2 而不是「down 到 family 版本」：测试冻结的是当前迁移链的
	// 直接前驱关系；以后在 family 之后再加迁移时，这里必须同步增加步数
	//（新迁移不会改变 family down 的语义，只是多了要回退的一层）。
	if err := m.Steps(-2); err != nil {
		t.Fatalf("migrate down 2: %v", err)
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
	if indexExists(t, db, "tasks_mj_user_upstream_idx") || indexExists(t, db, "tasks_mj_upstream_idx") {
		t.Fatal("down must drop MJ upstream indexes")
	}
	// 插件行在 down 后仍可写（CHECK 没有误伤合法族）。
	execTaskInsert(t, db, "mig_plugin_after_down", "plugin")

	// up 两步：迁移可反转（down 后再次前滚仍接受 mj，索引恢复）。
	if err := m.Steps(2); err != nil {
		t.Fatalf("migrate up 2: %v", err)
	}
	execTaskInsert(t, db, "mig_mj_after_up", "mj")
	if !indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("tasks_family_pending_idx missing after re-up")
	}
	if !indexExists(t, db, "tasks_mj_user_upstream_idx") || !indexExists(t, db, "tasks_mj_upstream_idx") {
		t.Fatal("MJ upstream indexes missing after re-up")
	}
}

// TestTasksTerminalRetentionIndexMigrationUpDown 验证 W10-N9 的保留索引迁移
// 可反转：up 建索引（含终态/待补偿谓词），down 删除，再次 up 恢复。
//
// 固定目标版本 20261023120000（当前 tip），与上面的 MJ 用例同一套临时
// schema 手法；断言的 finally 状态是「索引存在」，不依赖其它迁移的副作用。
func TestTasksTerminalRetentionIndexMigrationUpDown(t *testing.T) {
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
	schema := fmt.Sprintf("w10n9_mig_%d", time.Now().UnixNano())
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

	if err := m.Migrate(20261023120000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261023120000: %v", err)
	}
	if !indexExists(t, db, "tasks_terminal_retention_idx") {
		t.Fatal("tasks_terminal_retention_idx missing after up")
	}
	var def string
	if err := db.QueryRow(`SELECT pg_get_indexdef('tasks_terminal_retention_idx'::regclass)`).Scan(&def); err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	for _, want := range []string{"success", "failure", "billing_pending"} {
		if !strings.Contains(def, want) {
			t.Fatalf("index definition missing %q: %s", want, def)
		}
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	if indexExists(t, db, "tasks_terminal_retention_idx") {
		t.Fatal("down must drop tasks_terminal_retention_idx")
	}
	// down 不改变数据面：终态任务仍可写入。
	if err := execTaskInsertErr(db, "mig_terminal_after_down", "plugin"); err != nil {
		t.Fatalf("insert after down: %v", err)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1: %v", err)
	}
	if !indexExists(t, db, "tasks_terminal_retention_idx") {
		t.Fatal("tasks_terminal_retention_idx missing after re-up")
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
