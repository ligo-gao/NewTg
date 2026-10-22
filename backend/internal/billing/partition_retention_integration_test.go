//go:build integration

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// 本文件是 W10-N10 的真 PG（日志库）验收（R-W10-03/04）。
//
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10nn_logs?sslmode=disable' \
//	NEWTG_TEST_RETENTION_DSN='postgres://newtg_retention:newtg@localhost:55432/newtg_dom_w10nn_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration ./internal/billing/ -run PartitionRetention -count=1
//
// 【两个 DSN】保留删除的授权主体是 newtg_retention（R-W10-01 A）。测试环境
// 若没有该角色/DSN，则用同一个 admin DSN 跑逻辑用例（分区属主即当前用户），
// 并在日志中明确登记「身份分离未被覆盖」。权限拒绝用例在能建受限角色时
// 独立创建 app 风格角色，证明「用 app DSN → 拒绝」而不是静默降级。

func openPartitionLogsDB(t *testing.T, env string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s 未设置，跳过分区保留集成测试", env)
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

// openPartitionPair 返回 admin（建表/造数）与 retention（执行删除）两个连接。
//
// 缺 NEWTG_TEST_RETENTION_DSN 时两者相同，并 t.Log 登记覆盖缺口：逻辑
// （只删过期分区、边界、上限）仍被覆盖，身份分离由「权限拒绝」用例补。
func openPartitionPair(t *testing.T) (admin, retention *sql.DB, sameIdentity bool) {
	t.Helper()
	admin = openPartitionLogsDB(t, "NEWTG_TEST_LOGS_DSN")
	if os.Getenv("NEWTG_TEST_RETENTION_DSN") == "" {
		t.Log("NEWTG_TEST_RETENTION_DSN 未设置：用 app/admin DSN 执行删除逻辑测试；" +
			"retention 角色身份未覆盖（已登记）")
		return admin, admin, true
	}
	retention = openPartitionLogsDB(t, "NEWTG_TEST_RETENTION_DSN")
	return admin, retention, false
}

func currentUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	var user string
	if err := db.QueryRow(`SELECT current_user`).Scan(&user); err != nil {
		t.Fatalf("select current_user: %v", err)
	}
	return user
}

// createUsagePartition 建一个月分区，并把属主转给 owner（空表示保持当前用户）。
func createUsagePartition(t *testing.T, db *sql.DB, name, from, to, owner string) {
	t.Helper()
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_logs FOR VALUES FROM ('%s') TO ('%s')`,
		name, from, to)
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("create partition %s: %v", name, err)
	}
	if owner != "" {
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s OWNER TO %s`, name, owner)); err != nil {
			t.Fatalf("alter owner of %s to %s: %v", name, owner, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
	})
}

func partitionExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("check partition %s: %v", name, err)
	}
	return exists
}

func TestPartitionRetentionDropsOnlyExpiredOwnedPartitions(t *testing.T) {
	admin, retention, _ := openPartitionPair(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	retentionDays := time.Duration(DefaultPartitionRetentionDays) * 24 * time.Hour
	owner := ""
	if currentUser(t, admin) != currentUser(t, retention) {
		owner = currentUser(t, retention)
	}

	// 固定使用迁移窗口之外的月份，避免与 12 个月预建分区冲突。
	expiredOld := "usage_logs_202401"  // To 2024-02-01，远早于 cutoff
	expiredMid := "usage_logs_202402"  // To 2024-03-01
	retainedNew := "usage_logs_202609" // To 2026-10-01 > cutoff(2026-07-24)
	createUsagePartition(t, admin, expiredOld, "2024-01-01", "2024-02-01", owner)
	createUsagePartition(t, admin, expiredMid, "2024-02-01", "2024-03-01", owner)
	createUsagePartition(t, admin, retainedNew, "2026-09-01", "2026-10-01", owner)

	// 过期分区里各写一行：DROP 后父表 SELECT 应为空且不报 no partition。
	rowTimes := []time.Time{
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC),
	}
	for i, created := range rowTimes {
		rid := fmt.Sprintf("00000000-0000-4000-8000-%012d", 900+i)
		if _, err := admin.Exec(
			`INSERT INTO usage_logs (request_id, user_id, model, status, created_at)
			 VALUES ($1::uuid, 42, 'w10nn-test', 1, $2)`, rid, created); err != nil {
			t.Fatalf("insert row into %s: %v", []string{expiredOld, expiredMid}[i], err)
		}
	}

	dropped, err := DropExpiredUsagePartitions(ctx, retention, now, retentionDays, 10)
	if err != nil {
		t.Fatalf("drop expired: %v", err)
	}
	if len(dropped) != 2 || dropped[0] != expiredOld || dropped[1] != expiredMid {
		t.Fatalf("dropped = %v, want [%s %s] (oldest first)", dropped, expiredOld, expiredMid)
	}
	if partitionExists(t, admin, expiredOld) || partitionExists(t, admin, expiredMid) {
		t.Fatal("expired partitions still exist after drop")
	}
	if !partitionExists(t, admin, retainedNew) {
		t.Fatal("partition above cutoff must be retained")
	}
	var rows int
	if err := admin.QueryRow(
		`SELECT count(*) FROM usage_logs WHERE model = 'w10nn-test'`).Scan(&rows); err != nil {
		t.Fatalf("select parent after drop: %v", err)
	}
	if rows != 0 {
		t.Fatalf("rows in dropped partitions = %d, want 0", rows)
	}
}

func TestPartitionRetentionBatchLimitLeavesBacklog(t *testing.T) {
	admin, retention, _ := openPartitionPair(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	owner := ""
	if currentUser(t, admin) != currentUser(t, retention) {
		owner = currentUser(t, retention)
	}
	months := []struct {
		name, from, to string
	}{
		{"usage_logs_202301", "2023-01-01", "2023-02-01"},
		{"usage_logs_202302", "2023-02-01", "2023-03-01"},
		{"usage_logs_202303", "2023-03-01", "2023-04-01"},
	}
	for _, m := range months {
		createUsagePartition(t, admin, m.name, m.from, m.to, owner)
	}

	// 单轮上限 2：只删最旧两个，积压留给下一轮（不无限循环）。
	dropped, err := DropExpiredUsagePartitions(ctx, retention, now,
		time.Duration(DefaultPartitionRetentionDays)*24*time.Hour, 2)
	if err != nil {
		t.Fatalf("drop batch: %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("batch dropped = %v, want 2", dropped)
	}
	if !partitionExists(t, admin, "usage_logs_202303") {
		t.Fatal("backlog partition must survive this round")
	}
	// 下一轮继续。
	dropped, err = DropExpiredUsagePartitions(ctx, retention, now,
		time.Duration(DefaultPartitionRetentionDays)*24*time.Hour, 2)
	if err != nil {
		t.Fatalf("drop backlog: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "usage_logs_202303" {
		t.Fatalf("backlog dropped = %v, want [usage_logs_202303]", dropped)
	}
	// 再执行 = no-op（幂等，无候选）。
	dropped, err = DropExpiredUsagePartitions(ctx, retention, now,
		time.Duration(DefaultPartitionRetentionDays)*24*time.Hour, 2)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("repeat drop = (%v, %v), want (nil, nil)", dropped, err)
	}
}

func TestPartitionRetentionRejectsAppIdentity(t *testing.T) {
	admin := openPartitionLogsDB(t, "NEWTG_TEST_LOGS_DSN")
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	retentionDays := time.Duration(DefaultPartitionRetentionDays) * 24 * time.Hour

	// 造一个过期分区，属主故意留成 admin（模拟「retention 未获属主」或
	// 「误用 app DSN」）。受限角色连分区所有权都没有。
	createUsagePartition(t, admin, "usage_logs_202304", "2023-04-01", "2023-05-01", "")

	// 建一个 app 风格的最小权限角色（只有 CONNECT/USAGE/SELECT），复现误用
	// app DSN 的场景。需要超级用户；建不出来时跳过并登记。
	role := "w10nn_app_role"
	if _, err := admin.Exec(fmt.Sprintf(
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='%s') THEN
		   CREATE ROLE %s LOGIN PASSWORD 'w10nn'; END IF; END $$`, role, role)); err != nil {
		t.Skipf("cannot create limited role (need superuser): %v", err)
	}
	t.Cleanup(func() {
		// DROP OWNED 先收回该角色持有的授权，否则 DROP ROLE 会因依赖失败。
		_, _ = admin.Exec(fmt.Sprintf(`DROP OWNED BY %s`, role))
		_, _ = admin.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	})
	dbName := os.Getenv("NEWTG_TEST_DB_NAME")
	if dbName == "" {
		dbName = "newtg_dom_w10nn_logs"
	}
	if _, err := admin.Exec(fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, dbName, role)); err != nil {
		t.Skipf("cannot grant connect: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, role)); err != nil {
		t.Skipf("cannot grant usage: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`GRANT SELECT ON usage_logs TO %s`, role)); err != nil {
		t.Skipf("cannot grant select: %v", err)
	}
	appDSN := os.Getenv("NEWTG_TEST_APP_DSN")
	if appDSN == "" {
		appDSN = strings.Replace(os.Getenv("NEWTG_TEST_LOGS_DSN"),
			"newtg:newtg@", role+":w10nn@", 1)
	}
	appDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		t.Skipf("open app-style dsn: %v", err)
	}
	defer func() { _ = appDB.Close() }()
	if err := appDB.Ping(); err != nil {
		t.Skipf("ping app-style dsn: %v", err)
	}

	_, err = DropExpiredUsagePartitions(ctx, appDB, now, retentionDays, 2)
	var permErr *PartitionRetentionUnavailableError
	if !errors.As(err, &permErr) {
		t.Fatalf("app identity drop error = %v, want *PartitionRetentionUnavailableError", err)
	}
	if !partitionExists(t, admin, "usage_logs_202304") {
		t.Fatal("partition must survive a rejected drop")
	}
}

func TestPartitionDropLoopRunsOnceWithRedisLock(t *testing.T) {
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过分区删除循环集成测试")
	}
	db := openPartitionLogsDB(t, "NEWTG_TEST_LOGS_DSN")
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping test redis: %v", err)
	}
	defer func() { _ = client.Close() }()

	calls := make(chan time.Time, 8)
	dropFn := func(context.Context, time.Time, time.Duration, int) ([]string, error) {
		select {
		case calls <- time.Now():
		default:
		}
		return nil, nil
	}
	start := func() (context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- RunPartitionDropLoop(ctx, PartitionDropDeps{
				DB:            db,
				Redis:         client,
				RetentionDays: DefaultPartitionRetentionDays,
				Interval:      20 * time.Millisecond,
				Drop:          dropFn,
			})
		}()
		return cancel, done
	}
	waitStop := func(t *testing.T, cancel context.CancelFunc, done chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("loop = %v, want nil", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("loop did not stop on cancel")
		}
	}

	// 另一个副本持有锁：本循环必须跳过（不并发 DROP）。
	if ok, err := client.SetNX(context.Background(), partitionDropLockKey,
		"other-replica", time.Minute).Result(); err != nil || !ok {
		t.Fatalf("pre-acquire lock = (%t, %v)", ok, err)
	}
	cancel, done := start()
	select {
	case <-calls:
		waitStop(t, cancel, done)
		t.Fatal("drop ran while another replica held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	waitStop(t, cancel, done)
	_ = client.Del(context.Background(), partitionDropLockKey)

	// 锁释放后：启动即执行一轮（无论日更间隔），并在 cancel 后退出。
	cancel, done = start()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		waitStop(t, cancel, done)
		t.Fatal("startup pass did not run after lock release")
	}
	waitStop(t, cancel, done)
}

// TestEnsureUsagePartitionsLookahead 验证 R-W10-04：默认 3 个月（当月 + 下 3）
// 与显式 0（只保证当月）的分区数，并验证新分区属主被转给 newtg_retention。
func TestEnsureUsagePartitionsLookahead(t *testing.T) {
	admin := openPartitionLogsDB(t, "NEWTG_TEST_LOGS_DSN")
	ctx := context.Background()

	// 用远期月份避免与迁移预建窗口冲突。
	now := time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC)
	names := []string{
		"usage_logs_203001", "usage_logs_203002",
		"usage_logs_203003", "usage_logs_203004",
	}
	for _, name := range names {
		_, _ = admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
		t.Cleanup(func() { _, _ = admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)) })
	}

	if err := EnsureUsagePartitionsWithLookahead(ctx, admin, now, 3); err != nil {
		t.Fatalf("ensure lookahead=3: %v", err)
	}
	for _, name := range names {
		if !partitionExists(t, admin, name) {
			t.Fatalf("%s missing after lookahead=3", name)
		}
	}
	// 超出 lookahead 的月份不应被创建。
	if partitionExists(t, admin, "usage_logs_203005") {
		t.Fatal("lookahead=3 must not create month +4")
	}
	for _, name := range names {
		var owner string
		if err := admin.QueryRow(
			`SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = to_regclass($1)`, name).
			Scan(&owner); err != nil {
			t.Fatalf("read owner of %s: %v", name, err)
		}
		if owner != retentionRoleName {
			t.Fatalf("%s owner = %s, want %s", name, owner, retentionRoleName)
		}
	}

	// lookahead=0：只保证当月。
	now0 := time.Date(2031, 3, 5, 0, 0, 0, 0, time.UTC)
	only := "usage_logs_203103"
	_, _ = admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, only))
	t.Cleanup(func() { _, _ = admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, only)) })
	if err := EnsureUsagePartitionsWithLookahead(ctx, admin, now0, 0); err != nil {
		t.Fatalf("ensure lookahead=0: %v", err)
	}
	if !partitionExists(t, admin, only) {
		t.Fatal("lookahead=0 must still create the current month")
	}
	if partitionExists(t, admin, "usage_logs_203104") {
		t.Fatal("lookahead=0 must not create the next month")
	}
}
