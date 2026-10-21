//go:build integration

package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 W10-N9 任务保留清理的真 PG + Redis 验收（R-W10-10）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10n?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration ./internal/tasks/ -run TaskRetention -count=1

func openTaskRetentionRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过任务保留集成测试")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping test redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

var retentionTaskSeq int

type retentionTaskFixture struct {
	Family            string
	State             State
	FinishTime        *time.Time
	RetentionDeadline *time.Time
	PrivateData       map[string]any
	UpdatedAt         *time.Time
}

func insertRetentionTask(t *testing.T, db *sql.DB, userID int64, f retentionTaskFixture) int64 {
	t.Helper()
	retentionTaskSeq++
	publicID := fmt.Sprintf("task_retention_%d_%d", userID, retentionTaskSeq)
	private := f.PrivateData
	if private == nil {
		private = map[string]any{}
	}
	// private_data 写成 jsonb；JSON 编码而不是手工拼字面量，避免转义问题。
	var id int64
	err := db.QueryRow(`
		INSERT INTO tasks (
			public_task_id, family, action, plugin_key, fetch_mode, state,
			user_id, "group", channel_id, quota, finish_time, retention_deadline,
			private_data, updated_at
		) VALUES (
			$1, $2, 'generate', 'sample', 'poll', $3,
			$4, 'default', 1, 100, $5, $6, $7::jsonb, COALESCE($8, now())
		) RETURNING id`,
		publicID, f.Family, string(f.State), userID,
		f.FinishTime, f.RetentionDeadline, mustJSON(t, private), f.UpdatedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert retention task: %v", err)
	}
	return id
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal private_data: %v", err)
	}
	return string(raw)
}

func taskExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("check task %d: %v", id, err)
	}
	return n > 0
}

// clearTerminalTasks 清掉库里其它测试残留的终态任务，让「删了几条」的
// 精确断言可复现。DeleteTerminalTasksBefore 的删除面是按族的全表（生产
// 语义），没有 user/batch 过滤；测试库专用且包内用例串行，清场是确定的。
func clearTerminalTasks(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM tasks
		WHERE family IN ('plugin', 'mj') AND state IN ('success', 'failure')`); err != nil {
		t.Fatalf("clear terminal tasks: %v", err)
	}
}

func TestTaskRetentionDeletesOnlyEligibleTerminalRows(t *testing.T) {
	db := openTaskTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	userID := time.Now().UnixNano()%1_000_000 + 7_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })
	clearTerminalTasks(t, db)

	old := now.Add(-40 * 24 * time.Hour) // 超过 30 天
	recent := now.Add(-10 * 24 * time.Hour)
	pastDeadline := now.Add(-24 * time.Hour)
	futureDeadline := now.Add(10 * 24 * time.Hour)

	deleted := []int64{
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &old}),
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateFailure, FinishTime: &old}),
		// retention_deadline 已过：即使 finish_time 更近也应删除。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &recent, RetentionDeadline: &pastDeadline}),
		// 缺 finish_time 的终态历史行：updated_at 兜底。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, UpdatedAt: &old}),
	}
	kept := []int64{
		// 非终态：finish_time 只作为遗留列出现，绝不参与删除。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateInProgress, FinishTime: &old}),
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateUnknown, FinishTime: &old}),
		// 未超保留期。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &recent}),
		// 自定义期限未到：族默认 30 天不能提前删除。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &old, RetentionDeadline: &futureDeadline}),
		// 待补偿标记：删除它们会丢结算/退款线索。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &old,
			PrivateData: map[string]any{"billing_pending": true}}),
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &old,
			PrivateData: map[string]any{"settlement_pending": true}}),
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &old,
			PrivateData: map[string]any{"refund_pending": true}}),
		// MJ 族不参与插件族的删除。
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyMJ, State: StateSuccess, FinishTime: &old}),
	}

	got, err := DeleteTerminalTasksBefore(ctx, db, FamilyPlugin, 30, now, 100)
	if err != nil {
		t.Fatalf("delete plugin terminal: %v", err)
	}
	if want := int64(len(deleted)); got != want {
		t.Fatalf("deleted = %d, want %d", got, want)
	}
	for _, id := range deleted {
		if taskExists(t, db, id) {
			t.Fatalf("task %d should have been deleted", id)
		}
	}
	for _, id := range kept {
		if !taskExists(t, db, id) {
			t.Fatalf("task %d should have been kept", id)
		}
	}

	// 族隔离：MJ 族的删除只清理 MJ 行。
	mjDeleted, err := DeleteTerminalTasksBefore(ctx, db, FamilyMJ, 30, now, 100)
	if err != nil {
		t.Fatalf("delete mj terminal: %v", err)
	}
	if mjDeleted != 1 {
		t.Fatalf("mj deleted = %d, want 1", mjDeleted)
	}

	// 幂等：没有可删行。
	again, err := DeleteTerminalTasksBefore(ctx, db, FamilyPlugin, 30, now, 100)
	if err != nil || again != 0 {
		t.Fatalf("repeat delete = (%d, %v), want (0, nil)", again, err)
	}
}

func TestTaskRetentionBatchLimit(t *testing.T) {
	db := openTaskTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	userID := time.Now().UnixNano()%1_000_000 + 8_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })
	clearTerminalTasks(t, db)

	old := now.Add(-40 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		insertRetentionTask(t, db, userID, retentionTaskFixture{
			Family: FamilyPlugin, State: StateSuccess, FinishTime: &old})
	}
	for _, want := range []int64{2, 2, 1} {
		got, err := DeleteTerminalTasksBefore(ctx, db, FamilyPlugin, 30, now, 2)
		if err != nil {
			t.Fatalf("delete batch: %v", err)
		}
		if got != want {
			t.Fatalf("batch deleted = %d, want %d", got, want)
		}
	}
}

func TestTaskRetentionWorkerFamilyKeysAreIndependent(t *testing.T) {
	db := openTaskTestDB(t)
	rdb := openTaskRetentionRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	userID := time.Now().UnixNano()%1_000_000 + 9_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	old := now.Add(-40 * 24 * time.Hour)
	pluginID := insertRetentionTask(t, db, userID, retentionTaskFixture{
		Family: FamilyPlugin, State: StateSuccess, FinishTime: &old})
	mjID := insertRetentionTask(t, db, userID, retentionTaskFixture{
		Family: FamilyMJ, State: StateSuccess, FinishTime: &old})

	// 插件族 0 = 关闭，MJ 族 30 天：只有 MJ 行被删。
	w, err := newRetentionWorker(RetentionDeps{
		DB:                  db,
		Redis:               rdb,
		Now:                 func() time.Time { return now },
		PluginRetentionDays: 0,
		MJRetentionDays:     30,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	w.runOnce(ctx)
	if !taskExists(t, db, pluginID) {
		t.Fatal("plugin row must survive when its retention key is 0")
	}
	if taskExists(t, db, mjID) {
		t.Fatal("mj row should have been deleted by the mj retention key")
	}
}

func TestTaskRetentionWorkerBothDisabled(t *testing.T) {
	db := openTaskTestDB(t)
	rdb := openTaskRetentionRedis(t)
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	userID := time.Now().UnixNano()%1_000_000 + 10_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	old := now.Add(-400 * 24 * time.Hour)
	id := insertRetentionTask(t, db, userID, retentionTaskFixture{
		Family: FamilyPlugin, State: StateSuccess, FinishTime: &old})

	// RunRetentionLoop 在两族都为 0 时直接返回，不触碰 DB/Redis。
	// 用带超时的 ctx：若「0=关闭」守护被删，循环会进入 24h ticker 而不是
	// 立即失败——超时让变异表现为断言红而不是测试挂死。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RunRetentionLoop(ctx, RetentionDeps{
		DB: db, Redis: rdb, Now: func() time.Time { return now },
		PluginRetentionDays: 0, MJRetentionDays: 0,
	}); err != nil {
		t.Fatalf("run loop disabled: %v", err)
	}
	if !taskExists(t, db, id) {
		t.Fatal("disabled retention must not delete rows")
	}
}

func TestTaskRetentionSkipsBatchReferencedTasks(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	userID := time.Now().UnixNano()%1_000_000 + 11_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })
	clearTerminalTasks(t, db)

	old := now.Add(-40 * 24 * time.Hour)
	id := insertRetentionTask(t, db, userID, retentionTaskFixture{
		Family: FamilyPlugin, State: StateSuccess, FinishTime: &old})

	// 父批次 total=1，子项指向该任务；删除任务会 CASCADE 掉子项，让
	// GetBatchByID 把 total-linked 计入 failed，已完成批次倒退为 failed。
	var batchID string
	if err := db.QueryRow(`INSERT INTO task_batches (user_id, token_id, status, total)
		VALUES ($1, 0, 'completed', 1) RETURNING id`, userID).Scan(&batchID); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM task_batches WHERE id = $1`, batchID) })
	if _, err := db.Exec(`INSERT INTO task_batches_items (batch_id, task_id) VALUES ($1, $2)`,
		batchID, id); err != nil {
		t.Fatalf("link batch item: %v", err)
	}

	deleted, err := DeleteTerminalTasksBefore(ctx, db, FamilyPlugin, 30, now, 100)
	if err != nil {
		t.Fatalf("delete terminal tasks: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 (task is referenced by a batch)", deleted)
	}
	if !taskExists(t, db, id) {
		t.Fatal("batch-referenced task must survive retention cleanup")
	}

	// 聚合不变：仍是 completed/1 个已完成、0 失败（不是 failed）。
	batch, err := repo.GetBatchByID(ctx, userID, batchID)
	if err != nil {
		t.Fatalf("GetBatchByID: %v", err)
	}
	if batch.Status != BatchStatusCompleted || batch.Completed != 1 || batch.Failed != 0 {
		t.Fatalf("batch aggregation = status %q completed %d failed %d, want completed/1/0",
			batch.Status, batch.Completed, batch.Failed)
	}
}

func TestRetentionLockCompareAndDelete(t *testing.T) {
	rdb := openTaskRetentionRedis(t)
	ctx := context.Background()
	key := fmt.Sprintf("w10n9:lock:test:%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = rdb.Del(ctx, key).Err() })

	token, acquired, err := acquireRetentionLock(ctx, rdb, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquire = (%v, %v), want acquired", acquired, err)
	}
	if _, second, err := acquireRetentionLock(ctx, rdb, key, time.Minute); err != nil || second {
		t.Fatalf("second acquire = (%v, %v), want not acquired", second, err)
	}
	releaseRetentionLock(ctx, rdb, key, "not-the-owner", retentionTestLogger())
	if n := rdb.Exists(ctx, key).Val(); n != 1 {
		t.Fatalf("lock removed by a wrong token; exists = %d", n)
	}
	releaseRetentionLock(ctx, rdb, key, token, retentionTestLogger())
	if n := rdb.Exists(ctx, key).Val(); n != 0 {
		t.Fatalf("lock not released by its owner; exists = %d", n)
	}
}

func TestTaskTerminalRetentionIndexExists(t *testing.T) {
	db := openTaskTestDB(t)
	ctx := context.Background()

	// 谓词断言：索引必须只覆盖终态且排除待补偿行（否则索引会跟着历史
	// 全量增长，清理扫描重新退化为大范围扫描）。
	var def string
	if err := db.QueryRowContext(ctx,
		`SELECT pg_get_indexdef('tasks_terminal_retention_idx'::regclass)`).Scan(&def); err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	for _, want := range []string{"success", "failure", "billing_pending", "settlement_pending", "refund_pending"} {
		if !strings.Contains(def, want) {
			t.Fatalf("index definition missing %q: %s", want, def)
		}
	}

	// 计划断言：清理子查询在禁用 seqscan 时必须命中该索引，证明索引可用
	// （只断言「索引存在」无法发现谓词写错导致索引不可用的回归）。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin explain tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.QueryContext(ctx, `
		EXPLAIN SELECT id FROM tasks
		WHERE family = 'plugin'
		  AND state IN ('success', 'failure')
		  AND NOT (private_data @> '{"billing_pending": true}'::jsonb
		        OR private_data @> '{"settlement_pending": true}'::jsonb
		        OR private_data @> '{"refund_pending": true}'::jsonb)
		  AND NOT EXISTS (SELECT 1 FROM task_batches_items bi WHERE bi.task_id = tasks.id)
		ORDER BY id ASC
		LIMIT 10`)
	if err != nil {
		t.Fatalf("explain retention delete: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain: %v", err)
	}
	if !strings.Contains(plan.String(), "tasks_terminal_retention_idx") {
		t.Fatalf("retention query does not use tasks_terminal_retention_idx:\n%s", plan.String())
	}
}
