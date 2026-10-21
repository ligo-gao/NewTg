//go:build integration

package tasks

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件是 W9-B4（G-M6D-27）的真实 PostgreSQL 验收：
//
//   - 未配置 → 设计默认 1440 分钟 / 20 次生效；
//   - 显式 0 → 禁用超时判定与失败上限；
//   - 正值 → 按值，且「恰好等于阈值」与「超一」两侧边界正确；
//   - 坏值/负值的回落属于配置映射层（platform/task_poller_test.go），
//     本文件不重复；
//   - 插件 poller 与 MJ poller 都收到同一配置；MJ 的 Handler=nil 时
//     不因超时改状态。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w9t?sslmode=disable' \
//	  go test -tags=integration ./internal/tasks/ -run W9B4 -count=1

// w9b4UserID 为每次测试取一个足够大的唯一用户号，清理时按它删除，
// 避免同一隔离库内不同用例互相看到对方的待轮询行。
func w9b4UserID(t *testing.T) int64 {
	t.Helper()
	return 700_000_000 + time.Now().UnixNano()%100_000_000
}

func insertW9B4Task(t *testing.T, repo *Repo, ctx context.Context, userID int64,
	publicID, family string, submitTime time.Time) *Task {
	t.Helper()
	submit := submitTime.UTC()
	task := &Task{
		PublicTaskID:   publicID,
		UpstreamTaskID: "up_" + publicID,
		Family:         family,
		Action:         "generate",
		PluginKey:      "sample",
		FetchMode:      "poll",
		State:          StateSubmitted,
		UserID:         userID,
		Group:          "default",
		ChannelID:      0, // 无渠道 → 插件 poller 走 recordFailure，便于钉失败上限
		Quota:          100,
		SubmitTime:     &submit,
		Properties:     map[string]any{"OriginModelName": "sample-model"},
		PrivateData: map[string]any{
			"request_id": publicID + "_req",
			"token_id":   int64(9),
		},
		Data: map[string]any{},
	}
	if family == FamilyMJ {
		// R-MJ-03：family=mj ⇒ 无插件绑定。
		task.PluginKey = ""
		task.Action = "IMAGINE"
	}
	if err := repo.Insert(ctx, task); err != nil {
		t.Fatalf("insert %s task: %v", family, err)
	}
	return task
}

func openW9B4Repo(t *testing.T) (*Repo, context.Context) {
	t.Helper()
	return NewRepo(openTaskTestDB(t)), context.Background()
}

func w9b4NewPluginPoller(t *testing.T, repo *Repo, handoff *fakeHandoff,
	timeout *time.Duration, maxFailures *int) *Poller {
	t.Helper()
	p, err := NewPoller(PollerConfig{
		Repo:        repo,
		Plugins:     newTestSnapshotter(t),
		Billing:     handoff,
		Timeout:     timeout,
		MaxFailures: maxFailures,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

func getW9B4Task(t *testing.T, repo *Repo, ctx context.Context, id int64) *Task {
	t.Helper()
	task, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get task %d: %v", id, err)
	}
	return task
}

// TestW9B4PollerUnsetUsesDesignDefaults：未配置 → 1440 分钟 / 20 次。
//
// 超时侧构造「超过 1440 分钟」的行；失败侧构造一个未超时但持续失败的行，
// 跑满 19 轮仍存活、第 20 轮（恰好等于阈值）FAILURE + 退款。
func TestW9B4PollerUnsetUsesDesignDefaults(t *testing.T) {
	repo, ctx := openW9B4Repo(t)
	userID := w9b4UserID(t)
	t.Cleanup(func() {
		db := repo.db
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	now := time.Now()
	overdue := insertW9B4Task(t, repo, ctx, userID, "w9b4_default_timeout", FamilyPlugin,
		now.Add(-DefaultTaskTimeout-time.Minute))
	// 未超时（1440m 内 1 分钟）但渠道缺失 → 每轮累计一次失败。
	within := insertW9B4Task(t, repo, ctx, userID, "w9b4_default_failures", FamilyPlugin,
		now.Add(-DefaultTaskTimeout+time.Minute))

	handoff := &fakeHandoff{}
	p := w9b4NewPluginPoller(t, repo, handoff, nil, nil)

	p.RunOnce(ctx)
	gotOverdue := getW9B4Task(t, repo, ctx, overdue.ID)
	if gotOverdue.State != StateFailure || gotOverdue.FailReason != "task_timeout" {
		t.Fatalf("unset timeout must default to 1440m: state=%s reason=%s",
			gotOverdue.State, gotOverdue.FailReason)
	}
	if handoff.refundCount() != 1 {
		t.Fatalf("default timeout must refund exactly once, got %d", handoff.refundCount())
	}

	// 首轮已经完成 within 的第 1 次失败；再跑 18 轮 → 共 19 次。
	for i := 0; i < 18; i++ {
		p.RunOnce(ctx)
	}
	gotWithin := getW9B4Task(t, repo, ctx, within.ID)
	if gotWithin.State.Terminal() {
		t.Fatalf("19 failures must be below the default 20 limit: state=%s", gotWithin.State)
	}
	if privateInt(gotWithin.PrivateData["poll_failures"]) != 19 {
		t.Fatalf("expected 19 accumulated failures, got %v", gotWithin.PrivateData["poll_failures"])
	}
	// 第 20 轮：恰好等于阈值 → FAILURE + 退款。
	p.RunOnce(ctx)
	gotWithin = getW9B4Task(t, repo, ctx, within.ID)
	if gotWithin.State != StateFailure || gotWithin.FailReason != "poll_failures_exceeded" {
		t.Fatalf("20th failure must hit the default limit: state=%s reason=%s",
			gotWithin.State, gotWithin.FailReason)
	}
	if handoff.refundCount() != 2 {
		t.Fatalf("failure limit must refund exactly once more, got %d", handoff.refundCount())
	}
}

// TestW9B4PollerExplicitZeroDisables：显式 0 → 超时与失败上限都不生效。
func TestW9B4PollerExplicitZeroDisables(t *testing.T) {
	repo, ctx := openW9B4Repo(t)
	userID := w9b4UserID(t)
	t.Cleanup(func() {
		_, _ = repo.db.ExecContext(ctx, `DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	// 极旧 + 持续失败：任何「默认化」都会让它在前 20 轮内失败退款。
	task := insertW9B4Task(t, repo, ctx, userID, "w9b4_zero_disabled", FamilyPlugin,
		time.Now().Add(-100*24*time.Hour))

	zeroDuration := time.Duration(0)
	zeroInt := 0
	handoff := &fakeHandoff{}
	p := w9b4NewPluginPoller(t, repo, handoff, &zeroDuration, &zeroInt)
	for i := 0; i < 25; i++ {
		p.RunOnce(ctx)
	}

	got := getW9B4Task(t, repo, ctx, task.ID)
	if got.State != StateSubmitted {
		t.Fatalf("explicit zero must disable timeout+limit: state=%s reason=%s",
			got.State, got.FailReason)
	}
	if handoff.refundCount() != 0 {
		t.Fatalf("explicit zero must not refund, got %d", handoff.refundCount())
	}
	if privateInt(got.PrivateData["poll_failures"]) != 25 {
		t.Fatalf("failures must still be counted, got %v", got.PrivateData["poll_failures"])
	}
}

// TestW9B4PollerPositiveLimitsAndBoundaries：正值按值；超过阈值 10s 判超时、
// 阈值内 10s 不判（严格大于的实钟验收；精确等号边界用注入 Now 的单测覆盖）；
// 失败上限 2 在第 2 次触发。
func TestW9B4PollerPositiveLimitsAndBoundaries(t *testing.T) {
	repo, ctx := openW9B4Repo(t)
	userID := w9b4UserID(t)
	t.Cleanup(func() {
		_, _ = repo.db.ExecContext(ctx, `DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	oneMinute := time.Minute
	zeroDuration := time.Duration(0)
	zeroInt := 0
	now := time.Now()
	over := insertW9B4Task(t, repo, ctx, userID, "w9b4_over_timeout", FamilyPlugin,
		now.Add(-oneMinute-10*time.Second))
	inside := insertW9B4Task(t, repo, ctx, userID, "w9b4_inside_timeout", FamilyPlugin,
		now.Add(-oneMinute+10*time.Second))

	handoff := &fakeHandoff{}
	p := w9b4NewPluginPoller(t, repo, handoff, &oneMinute, &zeroInt)
	p.RunOnce(ctx)

	gotOver := getW9B4Task(t, repo, ctx, over.ID)
	if gotOver.State != StateFailure || gotOver.FailReason != "task_timeout" {
		t.Fatalf("10s past 1m must time out: state=%s reason=%s", gotOver.State, gotOver.FailReason)
	}
	gotInside := getW9B4Task(t, repo, ctx, inside.ID)
	if gotInside.State.Terminal() {
		t.Fatalf("10s inside 1m must not time out: state=%s", gotInside.State)
	}

	// 失败上限 2：同一行再跑一轮才触发（首轮已累计 1 次 channel_missing）。
	two := 2
	p = w9b4NewPluginPoller(t, repo, handoff, &zeroDuration, &two)
	p.RunOnce(ctx) // inside → failures=2 → FAILURE
	gotInside = getW9B4Task(t, repo, ctx, inside.ID)
	if gotInside.State != StateFailure || gotInside.FailReason != "poll_failures_exceeded" {
		t.Fatalf("maxFailures=2 must fail on the 2nd failure: state=%s reason=%s",
			gotInside.State, gotInside.FailReason)
	}
}

// TestW9B4MJPollerSharesLimitsAndHonorsNilHandler：MJ 领取器收到同一对值；
// Handler=nil（A0 生产装配）时超时行保持原状，Handler 注入后超时/失败
// 上限才推进终态。
func TestW9B4MJPollerSharesLimitsAndHonorsNilHandler(t *testing.T) {
	repo, ctx := openW9B4Repo(t)
	userID := w9b4UserID(t)
	t.Cleanup(func() {
		_, _ = repo.db.ExecContext(ctx, `DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	old := time.Now().Add(-2 * time.Minute)
	oneMinute := time.Minute
	zeroDuration := time.Duration(0)
	zeroInt := 0

	// Handler=nil + 超过 1m：只领取，不改状态。
	nilTask := insertW9B4Task(t, repo, ctx, userID, "w9b4_mj_nil_handler", FamilyMJ, old)
	p, err := NewMJPoller(MJPollerConfig{Repo: repo, Timeout: &oneMinute, MaxFailures: &zeroInt})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(ctx)
	gotNil := getW9B4Task(t, repo, ctx, nilTask.ID)
	if gotNil.State != StateSubmitted || privateInt(gotNil.PrivateData["poll_failures"]) != 0 {
		t.Fatalf("Handler=nil must not mutate a timed-out MJ row: %#v", gotNil)
	}

	// Handler 注入 + 超过 1m：转 FAILURE，Handler 不被调用。
	handlerTask := insertW9B4Task(t, repo, ctx, userID, "w9b4_mj_handler_timeout", FamilyMJ, old)
	called := 0
	p, err = NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: &oneMinute, MaxFailures: &zeroInt,
		Handler: func(context.Context, *Task) error { called++; return nil },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(ctx)
	gotHandler := getW9B4Task(t, repo, ctx, handlerTask.ID)
	if called != 0 {
		t.Fatalf("Handler must not run for a timed-out MJ row, called %d", called)
	}
	if gotHandler.State != StateFailure || gotHandler.FailReason != "task_timeout" ||
		!boolFlag(gotHandler.PrivateData["billing_pending"]) {
		t.Fatalf("MJ timeout not enforced with Handler: %#v", gotHandler)
	}

	// Handler 持续报错 + 失败上限 2（超时禁用）：第 2 轮转 FAILURE。
	failTask := insertW9B4Task(t, repo, ctx, userID, "w9b4_mj_failure_limit", FamilyMJ, time.Now())
	two := 2
	p, err = NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: &zeroDuration, MaxFailures: &two,
		Handler: func(context.Context, *Task) error { return errors.New("upstream down") },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(ctx)
	gotFail := getW9B4Task(t, repo, ctx, failTask.ID)
	if gotFail.State.Terminal() || privateInt(gotFail.PrivateData["poll_failures"]) != 1 {
		t.Fatalf("first MJ failure must only accumulate: %#v", gotFail)
	}
	p.RunOnce(ctx)
	gotFail = getW9B4Task(t, repo, ctx, failTask.ID)
	if gotFail.State != StateFailure || gotFail.FailReason != "poll_failures_exceeded" {
		t.Fatalf("MJ maxFailures=2 must fail on the 2nd error: state=%s reason=%s",
			gotFail.State, gotFail.FailReason)
	}
}

// TestW9B4MJPollerExplicitZeroDisables：显式 0 对 MJ 同样禁用两项判定。
func TestW9B4MJPollerExplicitZeroDisables(t *testing.T) {
	repo, ctx := openW9B4Repo(t)
	userID := w9b4UserID(t)
	t.Cleanup(func() {
		_, _ = repo.db.ExecContext(ctx, `DELETE FROM tasks WHERE user_id = $1`, userID)
	})

	task := insertW9B4Task(t, repo, ctx, userID, "w9b4_mj_zero", FamilyMJ,
		time.Now().Add(-100*24*time.Hour))
	zeroDuration := time.Duration(0)
	zeroInt := 0
	p, err := NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: &zeroDuration, MaxFailures: &zeroInt,
		Handler: func(context.Context, *Task) error { return errors.New("upstream down") },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	for i := 0; i < 5; i++ {
		p.RunOnce(ctx)
	}
	got := getW9B4Task(t, repo, ctx, task.ID)
	if got.State.Terminal() {
		t.Fatalf("explicit zero must disable MJ limits: state=%s reason=%s",
			got.State, got.FailReason)
	}
	if privateInt(got.PrivateData["poll_failures"]) != 5 {
		t.Fatalf("MJ failures still counted, got %v", got.PrivateData["poll_failures"])
	}
}
