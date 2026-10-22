//go:build integration

package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 本文件是 W10-N11 的真 PG（日志库）验收（R-W10-09 A）。
//
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10nn_logs?sslmode=disable' \
//	NEWTG_TEST_RETENTION_DSN='postgres://newtg_retention:newtg@localhost:55432/newtg_dom_w10nn_logs?sslmode=disable' \
//	  go test -tags=integration ./internal/audit/ -run AuditRetention -count=1
//
// 缺 NEWTG_TEST_RETENTION_DSN 时用同一个 DSN 跑逻辑用例，并 t.Log 登记
// 「retention 身份未被覆盖」；权限拒绝用例在可建受限角色时独立验证。

func openAuditDB(t *testing.T, env string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s 未设置，跳过审计保留集成测试", env)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", env, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping %s: %v", env, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openAuditPair(t *testing.T) (admin, retention *sql.DB) {
	t.Helper()
	admin = openAuditDB(t, "NEWTG_TEST_LOGS_DSN")
	if os.Getenv("NEWTG_TEST_RETENTION_DSN") == "" {
		t.Log("NEWTG_TEST_RETENTION_DSN 未设置：用 admin DSN 执行审计保留逻辑测试；" +
			"retention 角色身份未覆盖（已登记）")
		return admin, admin
	}
	return admin, openAuditDB(t, "NEWTG_TEST_RETENTION_DSN")
}

type auditFixture struct {
	id       int64
	username string
}

// insertAuditRow 写入一行审计 fixture，created_at 可回填（表无触发器/无 updated_at）。
func insertAuditRow(t *testing.T, db *sql.DB, username, ip string, createdAt time.Time) auditFixture {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO audit_logs (user_id, username, category, ip, result, detail, created_at)
		VALUES (7, $1, 'operation', $2, 'success', '{"fixture":true}'::jsonb, $3)
		RETURNING id`, username, nullable(ip), createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert audit fixture: %v", err)
	}
	return auditFixture{id: id, username: username}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func auditRowExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM audit_logs WHERE id = $1)`, id).
		Scan(&exists); err != nil {
		t.Fatalf("check audit row %d: %v", id, err)
	}
	return exists
}

func auditIP(t *testing.T, db *sql.DB, id int64) (sql.NullString, string) {
	t.Helper()
	var ip sql.NullString
	var username string
	if err := db.QueryRow(`SELECT ip, username FROM audit_logs WHERE id = $1`, id).
		Scan(&ip, &username); err != nil {
		t.Fatalf("read audit row %d: %v", id, err)
	}
	return ip, username
}

func cleanupAuditPrefix(t *testing.T, db *sql.DB, prefix string) {
	t.Helper()
	clear := func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE username LIKE $1`, prefix+"%") }
	clear()
	t.Cleanup(clear)
}

func fixturePrefix(t *testing.T, tag string) string {
	t.Helper()
	return fmt.Sprintf("w10nn-%s-%d", tag, time.Now().UnixNano())
}

func TestAuditRetentionDeletesOnlyRowsOlderThanTwelveMonths(t *testing.T) {
	admin, retention := openAuditPair(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, -DefaultRetentionMonths, 0)
	prefix := fixturePrefix(t, "del")
	cleanupAuditPrefix(t, admin, prefix)

	old := insertAuditRow(t, admin, prefix+"-old", "", cutoff.Add(-time.Hour))
	fresh := insertAuditRow(t, admin, prefix+"-fresh", "", cutoff.Add(time.Hour))
	// 恰好等于 cutoff 的行按 `< cutoff` 保留（边界留给下一轮/下一个瞬间）。
	boundary := insertAuditRow(t, admin, prefix+"-boundary", "", cutoff)

	n, err := DeleteAuditBefore(ctx, retention, cutoff, 1000)
	if err != nil {
		t.Fatalf("delete before cutoff: %v", err)
	}
	if n < 1 {
		t.Fatalf("deleted = %d, want >= 1", n)
	}
	if auditRowExists(t, admin, old.id) {
		t.Fatal("row older than 12 months survived")
	}
	if !auditRowExists(t, admin, fresh.id) {
		t.Fatal("row inside the 12-month window was deleted")
	}
	if !auditRowExists(t, admin, boundary.id) {
		t.Fatal("row exactly at cutoff must be retained (created_at < cutoff only)")
	}
}

func TestAuditRetentionErasesIPAfterNinetyDaysKeepingRowAndUsername(t *testing.T) {
	admin, retention := openAuditPair(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Duration(DefaultIPEraseDays) * 24 * time.Hour)
	prefix := fixturePrefix(t, "ip")
	cleanupAuditPrefix(t, admin, prefix)

	old := insertAuditRow(t, admin, prefix+"-old", "203.0.113.7", cutoff.Add(-time.Hour))
	oldNull := insertAuditRow(t, admin, prefix+"-oldnull", "", cutoff.Add(-time.Hour))
	fresh := insertAuditRow(t, admin, prefix+"-fresh", "198.51.100.9", cutoff.Add(time.Hour))

	n, err := EraseAuditIPBefore(ctx, retention, cutoff, 1000)
	if err != nil {
		t.Fatalf("erase ip before cutoff: %v", err)
	}
	if n < 1 {
		t.Fatalf("erased = %d, want >= 1", n)
	}

	ip, username := auditIP(t, admin, old.id)
	if ip.Valid {
		t.Fatalf("old ip = %q, want NULL", ip.String)
	}
	if username != prefix+"-old" {
		t.Fatalf("username snapshot changed to %q (R-W10-09 A keeps it)", username)
	}
	if !auditRowExists(t, admin, old.id) {
		t.Fatal("ip erasure must not delete the audit row")
	}
	if !auditRowExists(t, admin, oldNull.id) {
		t.Fatal("row with NULL ip must survive")
	}
	if ip, _ := auditIP(t, admin, fresh.id); !ip.Valid || ip.String != "198.51.100.9" {
		t.Fatalf("recent ip = %v, want kept", ip)
	}
}

func TestAuditRetentionBatchLimitAndRepeatNoop(t *testing.T) {
	admin, retention := openAuditPair(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, -DefaultRetentionMonths, 0)
	prefix := fixturePrefix(t, "batch")
	cleanupAuditPrefix(t, admin, prefix)

	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, insertAuditRow(t, admin, fmt.Sprintf("%s-%d", prefix, i), "",
			cutoff.Add(-time.Duration(i+1)*time.Hour)).id)
	}

	n, err := DeleteAuditBefore(ctx, retention, cutoff, 2)
	if err != nil || n != 2 {
		t.Fatalf("first batch = (%d, %v), want (2, nil)", n, err)
	}
	n, err = DeleteAuditBefore(ctx, retention, cutoff, 2)
	if err != nil || n != 1 {
		t.Fatalf("second batch = (%d, %v), want (1, nil)", n, err)
	}
	n, err = DeleteAuditBefore(ctx, retention, cutoff, 2)
	if err != nil || n != 0 {
		t.Fatalf("repeat = (%d, %v), want (0, nil) (idempotent no-op)", n, err)
	}
	for _, id := range ids {
		if auditRowExists(t, admin, id) {
			t.Fatalf("row %d survived deletion", id)
		}
	}
}

// TestAuditRetentionAppRoleDenied 证明「用 app 风格 DSN → 权限拒绝」而不是
// 静默成功：app 角色只有 SELECT/INSERT，没有 DELETE 与列级 UPDATE(ip)。
func TestAuditRetentionAppRoleDenied(t *testing.T) {
	admin := openAuditDB(t, "NEWTG_TEST_LOGS_DSN")
	ctx := context.Background()
	role := "w10nn_audit_app"
	if _, err := admin.Exec(fmt.Sprintf(
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='%s') THEN
		   CREATE ROLE %s LOGIN PASSWORD 'w10nn'; END IF; END $$`, role, role)); err != nil {
		t.Skipf("cannot create role (need superuser): %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf(`DROP OWNED BY %s`, role))
		_, _ = admin.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	})
	dbName := os.Getenv("NEWTG_TEST_DB_NAME")
	if dbName == "" {
		dbName = "newtg_dom_w10nn_logs"
	}
	for _, stmt := range []string{
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, dbName, role),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, role),
		fmt.Sprintf(`GRANT SELECT, INSERT ON audit_logs TO %s`, role),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Skipf("grant failed: %v", err)
		}
	}
	appDSN := os.Getenv("NEWTG_TEST_APP_DSN")
	if appDSN == "" {
		appDSN = strings.Replace(os.Getenv("NEWTG_TEST_LOGS_DSN"),
			"newtg:newtg@", role+":w10nn@", 1)
	}
	appDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		t.Skipf("open app dsn: %v", err)
	}
	defer func() { _ = appDB.Close() }()
	if err := appDB.Ping(); err != nil {
		t.Skipf("ping app dsn: %v", err)
	}

	if _, err := DeleteAuditBefore(ctx, appDB, time.Now().AddDate(0, -12, 0), 10); err == nil {
		t.Fatal("app role deleted audit rows, want permission denied")
	}
	if _, err := EraseAuditIPBefore(ctx, appDB, time.Now().Add(-90*24*time.Hour), 10); err == nil {
		t.Fatal("app role erased audit ip, want permission denied")
	}
}

func TestAuditRetentionSelfAuditIsBounded(t *testing.T) {
	admin, retention := openAuditPair(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	prefix := fixturePrefix(t, "self")
	cleanupAuditPrefix(t, admin, prefix)
	// 两条过期行：删除分支会被调用多批（batchSize 设小以触发多批）。
	insertAuditRow(t, admin, prefix+"-1", "", now.AddDate(-2, 0, 0))
	insertAuditRow(t, admin, prefix+"-2", "", now.AddDate(-2, 0, 0))

	auditLogger := New(admin)
	// 清理上一轮失败留下的自审计行（否则计数断言会漂移）。
	if _, err := admin.Exec(`DELETE FROM audit_logs WHERE detail->>'event' = 'audit_retention'`); err != nil {
		t.Fatalf("clear prior self-audit rows: %v", err)
	}
	var auditCalls int
	w := &retentionWorker{
		db:              retention,
		log:             testRetentionLogger(),
		now:             func() time.Time { return now },
		interval:        RetentionInterval,
		retentionMonths: DefaultRetentionMonths,
		ipEraseDays:     0,
		batchSize:       1, // 强制多批删除，验证自审计仍然只有一条
		maxBatches:      10,
		audit: func(ctx context.Context, detail json.RawMessage) error {
			auditCalls++
			return auditLogger.Log(ctx, ActionAuditRetention, 0, detail)
		},
	}
	w.deleteFn = func(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
		return DeleteAuditBefore(ctx, w.db, cutoff, limit)
	}
	w.runOnce(ctx)

	if auditCalls != 1 {
		t.Fatalf("self-audit calls = %d, want exactly 1 per pass", auditCalls)
	}
	var rows int
	if err := admin.QueryRow(
		`SELECT count(*) FROM audit_logs WHERE category = 'operation' AND detail->>'event' = 'audit_retention'`).
		Scan(&rows); err != nil {
		t.Fatalf("count self-audit: %v", err)
	}
	if rows != 1 {
		t.Fatalf("self-audit rows = %d, want 1", rows)
	}
	// 自记录行没有 IP、created_at=now：本轮规则不会删它/擦它（自引用有界）。
	var hasIP bool
	if err := admin.QueryRow(
		`SELECT ip IS NOT NULL FROM audit_logs WHERE detail->>'event' = 'audit_retention'`).
		Scan(&hasIP); err != nil {
		t.Fatalf("read self-audit ip: %v", err)
	}
	if hasIP {
		t.Fatal("self-audit row must not carry an IP")
	}
}

// TestRetentionMigrationIsRepeatable 验证部署 SQL 可重复执行（幂等授权）。
func TestRetentionMigrationIsRepeatable(t *testing.T) {
	db := openAuditDB(t, "NEWTG_TEST_LOGS_DSN")
	sqlBytes, err := os.ReadFile("../../migrations-logs/20261022000000_retention_role.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(string(sqlBytes)); err != nil {
			t.Fatalf("apply retention migration (run %d): %v", i+1, err)
		}
	}
}
