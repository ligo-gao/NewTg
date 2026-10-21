//go:build integration

package tasks

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 任务数据面的集成验收：tasks 表的归属读取、分页/状态筛选，以及按插件
// 绑定选渠道。运行：
//
//	NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/tasks/
func openTaskTestDB(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { db.Close() })
	return db
}

func insertTaskFixture(t *testing.T, repo *Repo, ctx context.Context, publicID string, userID int64, state State, createdAt time.Time) *Task {
	t.Helper()
	now := createdAt
	task := &Task{
		PublicTaskID:   publicID,
		UpstreamTaskID: "up_" + publicID,
		Family:         "plugin",
		Action:         "generate",
		PluginKey:      "sample",
		FetchMode:      "poll",
		State:          state,
		UserID:         userID,
		Group:          "default",
		ChannelID:      1,
		Quota:          100,
		SubmitTime:     &now,
		Properties:     map[string]any{"OriginModelName": "sample-model"},
		Data:           map[string]any{"artifacts": []any{}},
	}
	if err := repo.Insert(ctx, task); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return task
}

func TestRepoInsertGetOwnership(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	task := insertTaskFixture(t, repo, ctx, fmt.Sprintf("task_%d", time.Now().UnixNano()), userID, StateSubmitted, time.Now().UTC())

	got, err := repo.GetByPublicID(ctx, userID, task.PublicTaskID, "sample")
	if err != nil {
		t.Fatalf("GetByPublicID: %v", err)
	}
	if got.UserID != userID || got.State != StateSubmitted || got.Quota != 100 {
		t.Fatalf("unexpected row: %#v", got)
	}
	// 他人读取与不存在读取同为 ErrTaskNotFound。
	if _, err := repo.GetByPublicID(ctx, userID+1, task.PublicTaskID, "sample"); err != ErrTaskNotFound {
		t.Fatalf("foreign read err = %v, want ErrTaskNotFound", err)
	}
	// plugin_key 不匹配也必须 404（插件隔离）。
	if _, err := repo.GetByPublicID(ctx, userID, task.PublicTaskID, "other"); err != ErrTaskNotFound {
		t.Fatalf("wrong plugin err = %v, want ErrTaskNotFound", err)
	}
}

func TestRepoListPaginationAndStateFilter(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		insertTaskFixture(t, repo, ctx, fmt.Sprintf("task_%d_%d", userID, i), userID, StateSubmitted, base.Add(time.Duration(i)*time.Minute))
	}
	insertTaskFixture(t, repo, ctx, fmt.Sprintf("task_%d_done", userID), userID, StateSuccess, base)

	items, total, err := repo.ListByUser(ctx, userID, ListFilter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if total != 4 || len(items) != 2 {
		t.Fatalf("total=%d items=%d，want 4/2", total, len(items))
	}
	// created_at 相同（同一事务/同秒）时按 id DESC 兜底；最后插入的 success 行在前。
	if items[0].State != StateSuccess {
		t.Fatalf("first item state = %s, want the most recently inserted row", items[0].State)
	}
	// 台账投影不含 data。
	if items[0].Data != nil {
		t.Fatalf("list projection must omit data")
	}

	filtered, total, err := repo.ListByUser(ctx, userID, ListFilter{States: []State{StateSuccess}, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListByUser filtered: %v", err)
	}
	if total != 1 || len(filtered) != 1 || filtered[0].State != StateSuccess {
		t.Fatalf("state filter total=%d items=%v", total, filtered)
	}
}

// TestRepoPollingAndSettlement CAS 语义的集成验收（T6b）：
// 未完成列表、终态 CAS、private_data 增量合并、quota 回写与补偿扫描。
func TestRepoPollingAndSettlement(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	publicID := fmt.Sprintf("task_%d_poll", userID)
	now := time.Now().UTC()
	task := &Task{
		PublicTaskID:   publicID,
		UpstreamTaskID: "up_poll",
		Family:         "plugin",
		Action:         "generate",
		PluginKey:      "sample",
		FetchMode:      "poll",
		State:          StateSubmitted,
		UserID:         userID,
		ChannelID:      999,
		Quota:          100,
		SubmitTime:     &now,
		PrivateData:    map[string]any{"request_id": "req_poll", "poll_failures": float64(2)},
		Data:           map[string]any{"artifacts": []any{}},
	}
	if err := repo.Insert(ctx, task); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// ListPending 只返回未完成行。
	pending, err := repo.ListPending(ctx, 1000)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if !containsTask(pending, task.ID) {
		t.Fatalf("pending list missing the new task")
	}

	// 首次 CAS 赢，第二次同 from 输。
	won, err := repo.ApplyPoll(ctx, task.ID, StateSubmitted, PollUpdate{
		State: StateInProgress, RawState: "RUNNING", Progress: 40,
		Data:    map[string]any{"progress_detail": "40%"},
		Private: map[string]any{"poll_failures": 0},
	})
	if err != nil || !won {
		t.Fatalf("first CAS won=%v err=%v", won, err)
	}
	won, err = repo.ApplyPoll(ctx, task.ID, StateSubmitted, PollUpdate{State: StateSuccess})
	if err != nil || won {
		t.Fatalf("stale CAS must lose (won=%v err=%v)", won, err)
	}

	got, err := repo.GetByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != StateInProgress || got.ProgressPercent != 40 {
		t.Fatalf("unexpected row after CAS: state=%s progress=%d", got.State, got.ProgressPercent)
	}
	if privateInt(got.PrivateData["poll_failures"]) != 0 {
		t.Fatalf("private_data not merged: %#v", got.PrivateData)
	}
	if got.Data["progress_detail"] != "40%" || got.Data["artifacts"] == nil {
		t.Fatalf("data increment must merge, not replace: %#v", got.Data)
	}

	// 终态 CAS + billing_pending 同语句落库。
	finish := time.Now().UTC()
	won, err = repo.ApplyPoll(ctx, task.ID, StateInProgress, PollUpdate{
		State: StateFailure, RawState: "FAILED", Progress: 100,
		Private:    map[string]any{"billing_pending": true},
		FinishTime: &finish, FailReason: stringPtr("upstream_failed"),
	})
	if err != nil || !won {
		t.Fatalf("terminal CAS won=%v err=%v", won, err)
	}

	// 补偿扫描按 private_data 标记命中；UpdateBilling 清标记并回写 quota。
	reconcilable, err := repo.ListReconcilable(ctx, 100)
	if err != nil {
		t.Fatalf("ListReconcilable: %v", err)
	}
	if !containsTask(reconcilable, task.ID) {
		t.Fatalf("reconcilable list missing billing_pending task")
	}
	zero := int64(0)
	if err := repo.UpdateBilling(ctx, task.ID, &zero, map[string]any{
		"billing_pending": false, "refund_pending": false, "billing_final": true,
	}); err != nil {
		t.Fatalf("UpdateBilling: %v", err)
	}
	got, err = repo.GetByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetByID after billing: %v", err)
	}
	if got.Quota != 0 || boolFlag(got.PrivateData["billing_pending"]) || !boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("billing update not applied: quota=%d private=%#v", got.Quota, got.PrivateData)
	}
	reconcilable, err = repo.ListReconcilable(ctx, 100)
	if err != nil {
		t.Fatalf("ListReconcilable after finish: %v", err)
	}
	if containsTask(reconcilable, task.ID) {
		t.Fatalf("finished task must leave the reconcilable set")
	}

	// 能力式读取不带 user_id；插件 key 事实仍在行上。
	anyRow, err := repo.GetByPublicIDAny(ctx, publicID)
	if err != nil {
		t.Fatalf("GetByPublicIDAny: %v", err)
	}
	if anyRow.UserID != userID || anyRow.PluginKey != "sample" {
		t.Fatalf("unexpected capability read: %#v", anyRow)
	}
}

// TestRepoGetChannelIgnoresStatus 验证产物/轮询读取渠道时不因渠道被禁用而失效。
func TestRepoGetChannelIgnoresStatus(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	name := fmt.Sprintf("t6b_ch_%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, name) })

	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
		                      model_mapping, priority, status, task_plugin_key, task_extend_plugin_keys)
		VALUES ($1, 'task_plugin', 'https://up.example.com', decode('00','hex'), '["sample-model"]'::jsonb,
		        '{}'::jsonb, 1, 2, 'sample', '[]'::jsonb)
		RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	target, err := repo.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if target.BaseURL != "https://up.example.com" {
		t.Fatalf("base url = %q", target.BaseURL)
	}
	if _, err := repo.GetChannel(ctx, id+1_000_000); err != ErrChannelNotFound {
		t.Fatalf("missing channel err = %v, want ErrChannelNotFound", err)
	}
}

func containsTask(items []*Task, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func stringPtr(v string) *string { return &v }

func TestRepoSelectChannelByBinding(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	suffix := fmt.Sprintf("t6a_%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE name LIKE $1`, suffix+"%") })

	insert := func(name, pluginKey string, extend []string, models string, priority int16, status int16) int64 {
		var extendJSON any
		if extend != nil {
			extendJSON = fmt.Sprintf(`["%s"]`, extend[0])
		}
		var id int64
		err := db.QueryRowContext(ctx, `
			INSERT INTO channels (name, type, base_url, credentials, supported_models,
			                      model_mapping, priority, status, task_plugin_key, task_extend_plugin_keys)
			VALUES ($1, 'task_plugin', 'https://up.example.com', decode('00','hex'), $2::jsonb,
			        '{"sample-model":"upstream-model"}'::jsonb, $3, $4, NULLIF($5, ''), COALESCE($6::jsonb, '[]'::jsonb))
			RETURNING id`, name, models, priority, status, pluginKey, extendJSON).Scan(&id)
		if err != nil {
			t.Fatalf("insert channel: %v", err)
		}
		return id
	}

	// 低优先级启用渠道 + 高优先级禁用渠道：必须选中低优先级的那条。
	low := insert(suffix+"_low", "sample", nil, `["sample-model"]`, 1, 1)
	insert(suffix+"_disabled", "sample", nil, `["sample-model"]`, 99, 2)
	// 未绑定该插件的渠道不得命中。
	insert(suffix+"_other", "other", nil, `["sample-model"]`, 100, 1)

	target, err := repo.SelectChannel(ctx, "sample", "sample-model", "")
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if target.ID != low {
		t.Fatalf("selected channel = %d, want %d", target.ID, low)
	}
	if target.ModelMapping["sample-model"] != "upstream-model" || target.BaseURL != "https://up.example.com" {
		t.Fatalf("unexpected target: %#v", target)
	}

	// 多绑（类型 60 的 task_extend_plugin_keys）同样可命中，且更高优先级胜出。
	multi := insert(suffix+"_multi", "", []string{"sample"}, `["sample-model"]`, 50, 1)
	target, err = repo.SelectChannel(ctx, "sample", "sample-model", "")
	if err != nil {
		t.Fatalf("SelectChannel multi: %v", err)
	}
	if target.ID != multi {
		t.Fatalf("multi-bound channel not selected: got %d, want %d", target.ID, multi)
	}

	// 无候选 → ErrChannelNotFound。
	if _, err := repo.SelectChannel(ctx, "sample", "missing-model", ""); err != ErrChannelNotFound {
		t.Fatalf("missing model err = %v, want ErrChannelNotFound", err)
	}
}

func TestRepoBatchCreateAndAggregate(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_batches WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	done := insertTaskFixture(t, repo, ctx, fmt.Sprintf("task_%d_done", userID), userID, StateSuccess, time.Now().UTC())
	pending := insertTaskFixture(t, repo, ctx, fmt.Sprintf("task_%d_pending", userID), userID, StateSubmitted, time.Now().UTC())

	batchID, err := NewBatchID()
	if err != nil {
		t.Fatalf("NewBatchID: %v", err)
	}
	// total=3 但只关联两条任务：第三条模拟「上游提交失败、没有任务行」的行。
	batch := &Batch{ID: batchID, UserID: userID, Status: BatchStatusInProgress, Total: 3}
	if err := repo.InsertBatch(ctx, batch); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	for _, publicID := range []string{done.PublicTaskID, pending.PublicTaskID} {
		if err := repo.LinkBatchItem(ctx, batchID, userID, publicID); err != nil {
			t.Fatalf("LinkBatchItem(%s): %v", publicID, err)
		}
	}
	view, err := repo.GetBatchByID(ctx, userID, batchID)
	if err != nil {
		t.Fatalf("GetBatchByID: %v", err)
	}
	if view.Completed != 1 || view.Failed != 1 || view.Status != BatchStatusInProgress {
		t.Fatalf("view = %#v, want completed=1 failed=1 in_progress", view)
	}
	if len(view.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(view.Items))
	}

	// 子任务推进到终态后，同一批次下一次读取即为 completed。
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state = 'success' WHERE id = $1`, pending.ID); err != nil {
		t.Fatalf("update task: %v", err)
	}
	view, err = repo.GetBatchByID(ctx, userID, batchID)
	if err != nil {
		t.Fatalf("GetBatchByID after terminal: %v", err)
	}
	if view.Completed != 2 || view.Failed != 1 || view.Status != BatchStatusCompleted {
		t.Fatalf("terminal view = %#v, want completed=2 failed=1 completed-status", view)
	}

	// 归属过滤：他人读取与不存在同为 ErrBatchNotFound。
	if _, err := repo.GetBatchByID(ctx, userID+1, batchID); err != ErrBatchNotFound {
		t.Fatalf("foreign batch err = %v, want ErrBatchNotFound", err)
	}

	// 关联不存在的任务 → 该条关联失败且不落行；父批次与已有链接不受影响
	// （R-16 的三段式：父行先落，单条关联的失败只让该行计入 failed）。
	badID, err := NewBatchID()
	if err != nil {
		t.Fatalf("NewBatchID: %v", err)
	}
	bad := &Batch{ID: badID, UserID: userID, Status: BatchStatusInProgress, Total: 1}
	if err := repo.InsertBatch(ctx, bad); err != nil {
		t.Fatalf("InsertBatch(bad): %v", err)
	}
	if err := repo.LinkBatchItem(ctx, badID, userID, "task_does_not_exist"); err == nil {
		t.Fatal("LinkBatchItem must fail when the task does not exist")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM task_batches_items WHERE batch_id = $1`, badID).Scan(&count); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed link left rows behind: count=%d", count)
	}
	badView, err := repo.GetBatchByID(ctx, userID, badID)
	if err != nil {
		t.Fatalf("GetBatchByID(bad): %v", err)
	}
	if badView.Failed != 1 || badView.Status != BatchStatusFailed {
		t.Fatalf("unlinked row must count as failed: %#v", badView)
	}
}
