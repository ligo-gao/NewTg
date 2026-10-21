//go:build integration

package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 本文件是 W8/MJ-A0 的 family 隔离验收（真实 PostgreSQL）。
//
// 覆盖：
//   - 插件族与 MJ 族的待领取/待补偿扫描互不越界（R-MJ-04 第一道防护）；
//   - 并发领取同一 MJ 行时，(id, from_state) CAS 只允许一个执行者推进
//     （与插件族的「终态恰好一次」同一锚点）。
//
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/tasks/

// insertFamilyTaskFixture 写一条指定族/状态的测试任务。
//
// MJ 行按 R-MJ-03 冻结写 plugin_key=”；billing_pending 等补偿标记直接落
// private_data（等价于终态 CAS 与结算同批写入后的状态）。
func insertFamilyTaskFixture(t *testing.T, repo *Repo, ctx context.Context,
	publicID string, userID int64, family string, state State, private map[string]any) *Task {
	t.Helper()
	now := time.Now().UTC()
	task := &Task{
		PublicTaskID:   publicID,
		UpstreamTaskID: "up_" + publicID,
		Family:         family,
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
		PrivateData:    defaultMap(private),
		Data:           map[string]any{},
	}
	if family == FamilyMJ {
		// R-MJ-03：family=mj ⇒ 无插件绑定；动作由族解释。
		task.PluginKey = ""
		task.Action = "IMAGINE"
	}
	if err := repo.Insert(ctx, task); err != nil {
		t.Fatalf("insert %s task: %v", family, err)
	}
	return task
}

func TestRepoPendingAndReconcilableAreFamilyScoped(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	pluginPending := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_plugin_pending", userID), userID, FamilyPlugin, StateSubmitted, nil)
	mjPending := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_mj_pending", userID), userID, FamilyMJ, StateSubmitted, nil)

	pluginPendingRows, err := repo.ListPendingByFamily(ctx, FamilyPlugin, 1000)
	if err != nil {
		t.Fatalf("ListPendingByFamily(plugin): %v", err)
	}
	if !containsTask(pluginPendingRows, pluginPending.ID) {
		t.Fatalf("plugin pending list missing plugin task")
	}
	if containsTask(pluginPendingRows, mjPending.ID) {
		t.Fatalf("plugin pending list must not contain an mj row (R-MJ-04)")
	}

	mjPendingRows, err := repo.ListPendingByFamily(ctx, FamilyMJ, 1000)
	if err != nil {
		t.Fatalf("ListPendingByFamily(mj): %v", err)
	}
	if !containsTask(mjPendingRows, mjPending.ID) {
		t.Fatalf("mj pending list missing mj task")
	}
	if containsTask(mjPendingRows, pluginPending.ID) {
		t.Fatalf("mj pending list must not contain a plugin row")
	}

	// 补偿扫描同样按族隔离：插件补偿器不得替 MJ 行动钱（R-MJ-04）。
	pluginTerminal := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_plugin_terminal", userID), userID, FamilyPlugin, StateFailure,
		map[string]any{"billing_pending": true})
	mjTerminal := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_mj_terminal", userID), userID, FamilyMJ, StateFailure,
		map[string]any{"billing_pending": true})

	pluginReconcilable, err := repo.ListReconcilable(ctx, FamilyPlugin, 100)
	if err != nil {
		t.Fatalf("ListReconcilable(plugin): %v", err)
	}
	if !containsTask(pluginReconcilable, pluginTerminal.ID) || containsTask(pluginReconcilable, mjTerminal.ID) {
		t.Fatalf("plugin reconcilable set crossed the family boundary: %v", taskIDs(pluginReconcilable))
	}
	mjReconcilable, err := repo.ListReconcilable(ctx, FamilyMJ, 100)
	if err != nil {
		t.Fatalf("ListReconcilable(mj): %v", err)
	}
	if !containsTask(mjReconcilable, mjTerminal.ID) || containsTask(mjReconcilable, pluginTerminal.ID) {
		t.Fatalf("mj reconcilable set crossed the family boundary: %v", taskIDs(mjReconcilable))
	}
}

// TestMJConcurrentCASHasSingleWinner：两个执行者同时领取同一 MJ 行时，只有
// 一个 (id, from_state) CAS 能赢——与插件族的「终态恰好一次」同一锚点。
//
// 两边的列表读取都在 barrier 之前完成，确保它们拿到的是同一个 from 状态；
// 否则先推进的一方会让另一方根本看不到行，测不出并发 CAS。
func TestMJConcurrentCASHasSingleWinner(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	task := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_mj_race", userID), userID, FamilyMJ, StateSubmitted, nil)

	const claimers = 2
	results := make(chan bool, claimers)
	release := make(chan struct{})
	var arrived sync.WaitGroup
	var workers sync.WaitGroup
	arrived.Add(claimers)
	workers.Add(claimers)

	for i := 0; i < claimers; i++ {
		go func() {
			defer workers.Done()
			rows, err := repo.ListPendingByFamily(ctx, FamilyMJ, 1000)
			if err != nil {
				arrived.Done()
				results <- false
				return
			}
			var snapshot *Task
			for _, row := range rows {
				if row.ID == task.ID {
					snapshot = row
					break
				}
			}
			if snapshot == nil {
				arrived.Done()
				results <- false
				return
			}
			arrived.Done()
			<-release
			won, err := repo.ApplyPoll(ctx, snapshot.ID, snapshot.State, PollUpdate{
				State:    StateQueued,
				RawState: "QUEUED",
				Private:  map[string]any{"claimed": true},
			})
			if err != nil {
				t.Errorf("ApplyPoll: %v", err)
				won = false
			}
			results <- won
		}()
	}

	arrived.Wait()
	close(release)
	workers.Wait()
	close(results)

	wins := 0
	for won := range results {
		if won {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent claims must have exactly one winner, got %d", wins)
	}

	got, err := repo.GetByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != StateQueued || got.RawState != "QUEUED" {
		t.Fatalf("task not advanced exactly once: state=%s raw=%s", got.State, got.RawState)
	}
}

// TestMJPollerClaimsOnlyMJRowsOnRealDB：在真实仓储上验证 MJ 领取器的 SQL
// 过滤——同一用户同时存在 plugin/mj 未完成任务时，Handler 只应看到 mj 行。
func TestMJPollerClaimsOnlyMJRowsOnRealDB(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_plugin_claim", userID), userID, FamilyPlugin, StateSubmitted, nil)
	mjTask := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_mj_claim", userID), userID, FamilyMJ, StateSubmitted, nil)

	var claimed []int64
	poller, err := NewMJPoller(MJPollerConfig{
		Repo: repo,
		Handler: func(_ context.Context, task *Task) error {
			claimed = append(claimed, task.ID)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	poller.RunOnce(ctx)

	if len(claimed) != 1 || claimed[0] != mjTask.ID {
		t.Fatalf("MJ poller claimed %v, want only mj task %d", claimed, mjTask.ID)
	}
}

func taskIDs(items []*Task) []int64 {
	out := make([]int64, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

// TestMJPollerIncrementFailuresAtomicOnRealDB 验收 M-6：真实 PostgreSQL 上
// 的并发失败计数必须每次 +1（jsonb 原子递增），不能因读-改-写丢计数。
func TestMJPollerIncrementFailuresAtomicOnRealDB(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	row := insertFamilyTaskFixture(t, repo, ctx,
		fmt.Sprintf("task_%d_mj_atomic", userID), userID, FamilyMJ, StateSubmitted,
		map[string]any{"request_id": "req_atomic"})
	p, err := NewMJPoller(MJPollerConfig{
		Repo:        repo,
		MaxFailures: intPtr(1000),
		Timeout:     durationPtr(0),
		Handler: func(context.Context, *Task) error {
			return errors.New("upstream down")
		},
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}

	const workers = 10
	snapshots := make([]*Task, workers)
	for i := range snapshots {
		snapshot, err := repo.GetByID(ctx, row.ID)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		snapshots[i] = snapshot
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range snapshots {
		snapshot := snapshots[i]
		go func() {
			defer wg.Done()
			<-start
			p.processTask(ctx, snapshot)
		}()
	}
	close(start)
	wg.Wait()

	got, err := repo.GetByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if failures := privateInt(got.PrivateData["poll_failures"]); failures != workers {
		t.Fatalf("poll_failures = %d, want %d (SQL increment lost updates)", failures, workers)
	}
}
