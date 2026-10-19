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
