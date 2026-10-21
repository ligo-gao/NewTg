//go:build integration

package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
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

func TestTaskRetentionDeletesOnlyEligibleTerminalRows(t *testing.T) {
	db := openTaskTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	userID := time.Now().UnixNano()%1_000_000 + 7_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

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
	if err := RunRetentionLoop(context.Background(), RetentionDeps{
		DB: db, Redis: rdb, Now: func() time.Time { return now },
		PluginRetentionDays: 0, MJRetentionDays: 0,
	}); err != nil {
		t.Fatalf("run loop disabled: %v", err)
	}
	if !taskExists(t, db, id) {
		t.Fatal("disabled retention must not delete rows")
	}
}

func TestTaskTerminalRetentionIndexExists(t *testing.T) {
	db := openTaskTestDB(t)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = 'tasks_terminal_retention_idx'`).Scan(&n); err != nil {
		t.Fatalf("query index: %v", err)
	}
	if n != 1 {
		t.Fatal("tasks_terminal_retention_idx missing; run migrations before integration tests")
	}
}
