package tasks

// W8/MJ-b：MJ 领取器的补偿扫描与租约续期验收（无外部依赖）。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingMJSettler struct {
	ids      []int64
	delegate *Settler
}

func (s *recordingMJSettler) SettleTerminal(ctx context.Context, task *Task) {
	if task == nil {
		return
	}
	s.ids = append(s.ids, task.ID)
	if s.delegate != nil {
		s.delegate.SettleTerminal(ctx, task)
	}
}

func mjPendingTask(requestID string) *Task {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	return &Task{
		Family: FamilyMJ, Action: "IMAGINE", PluginKey: "", FetchMode: "poll",
		State: StateSubmitted, UserID: 42, ChannelID: 10, Quota: 50,
		UpstreamTaskID: "mj_1", SubmitTime: &now,
		Properties:  map[string]any{},
		Data:        map[string]any{},
		PrivateData: map[string]any{"request_id": requestID, "token_id": float64(7)},
	}
}

func TestMJPollerReconcileIsFamilyScopedAndSettlesPending(t *testing.T) {
	repo := newFakePollRepo()
	mjTerminal := &Task{
		Family: FamilyMJ, Action: "IMAGINE", State: StateFailure, UserID: 42, ChannelID: 10, Quota: 50,
		Properties: map[string]any{}, Data: map[string]any{},
		PrivateData: map[string]any{"request_id": "req_mj", "billing_pending": true},
	}
	pluginTerminal := &Task{
		Family: FamilyPlugin, Action: "generate", PluginKey: "sample", State: StateFailure,
		UserID: 42, ChannelID: 10, Quota: 50,
		Properties: map[string]any{}, Data: map[string]any{},
		PrivateData: map[string]any{"request_id": "req_plugin", "billing_pending": true},
	}
	mjRow := repo.add(mjTerminal)
	repo.add(pluginTerminal)

	settler := &recordingMJSettler{}
	p, err := NewMJPoller(MJPollerConfig{Repo: repo, Settler: settler})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())

	if len(settler.ids) != 1 || settler.ids[0] != mjRow.ID {
		t.Fatalf("settled ids = %v, want only mj row %d", settler.ids, mjRow.ID)
	}
	for _, family := range repo.requestedFamilies() {
		if family != FamilyMJ {
			t.Fatalf("MJ poller requested family %q, want only %q", family, FamilyMJ)
		}
	}
}

func TestMJPollerTimeoutFailsAndRefundsInSameCycle(t *testing.T) {
	repo := newFakePollRepo()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	submit := now.Add(-2 * time.Hour)
	task := mjPendingTask("req_timeout")
	task.SubmitTime = &submit
	// 旧 raw_state 必须是可区分的非空值：网关强制失败若沿用它会把这些
	// 文本回给客户端（I-2 的回归断言）。
	task.RawState = "IN_PROGRESS"
	row := repo.add(task)

	handoff := &fakeHandoff{}
	settler, err := NewSettler(SettlerConfig{Billing: handoff, Repo: repo})
	if err != nil {
		t.Fatalf("NewSettler: %v", err)
	}
	timeout := time.Hour
	retention := func(now time.Time) *time.Time {
		deadline := now.Add(30 * 24 * time.Hour)
		return &deadline
	}
	p, err := NewMJPoller(MJPollerConfig{
		Repo:    repo,
		Handler: func(context.Context, *Task) error { t.Fatalf("timed-out task must not query upstream"); return nil },
		Settler: settler, Billing: handoff,
		Timeout: &timeout, Now: func() time.Time { return now },
		RetentionDeadline: retention,
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())

	got := repo.get(row.ID)
	if got.State != StateFailure || got.FailReason != "task_timeout" {
		t.Fatalf("timeout row state/reason = %s/%q", got.State, got.FailReason)
	}
	// 终态 CAS 先写 billing_pending，随后同一轮的补偿扫描把退款落地并把
	// billing_pending 清零、写 billing_final——两步都必须发生。
	if got.PrivateData["billing_final"] != true {
		t.Fatalf("timeout failure must be reconciled to billing_final: %#v", got.PrivateData)
	}
	if got.RetentionDeadline == nil {
		t.Fatalf("timeout failure must write a retention deadline")
	}
	if got.RawState != "" {
		t.Fatalf("gateway-forced FAILURE must clear raw_state (I-2), got %q", got.RawState)
	}
	// 同一轮 pollOnce 之后紧接补偿扫描：超时失败的行必须被退款。
	if handoff.refundCount() != 1 {
		t.Fatalf("refunds = %d, want exactly 1 (timeout → FAILURE → refund)", handoff.refundCount())
	}
}

func TestMJPollerRenewsInflightLeases(t *testing.T) {
	repo := newFakePollRepo()
	repo.add(mjPendingTask("req_lease"))
	handoff := &fakeHandoff{}

	p, err := NewMJPoller(MJPollerConfig{
		Repo:    repo,
		Handler: func(context.Context, *Task) error { return nil },
		Billing: handoff,
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())

	refreshed := handoff.refreshSnapshot()
	found := false
	for _, id := range refreshed {
		if id == "req_lease" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MJ poller must renew inflight leases (B2), refresh calls = %v", refreshed)
	}
}

func TestMJPollerWithoutSettlerDoesNotReconcile(t *testing.T) {
	repo := newFakePollRepo()
	repo.add(&Task{
		Family: FamilyMJ, Action: "IMAGINE", State: StateSuccess, Quota: 50,
		Properties: map[string]any{}, Data: map[string]any{},
		PrivateData: map[string]any{"request_id": "req", "billing_pending": true},
	})
	p, err := NewMJPoller(MJPollerConfig{Repo: repo})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())
	// Settler 为 nil 时只领取、不补偿（A0 冻结语义）：不应请求 reconcilable。
	for _, family := range repo.requestedFamilies() {
		if family != FamilyMJ {
			t.Fatalf("unexpected family %q", family)
		}
	}
	if len(repo.requestedFamilies()) != 1 {
		t.Fatalf("without settler only pollOnce should claim: %v", repo.requestedFamilies())
	}
}

// TestMJPollerClearFailuresOnlyForKnownStates 验收 I-3：unknown 成功返回
// 不得清零已有失败计数（否则失败上限永远打不到），已知非终态才清零。
func TestMJPollerClearFailuresOnlyForKnownStates(t *testing.T) {
	cases := []struct {
		name     string
		state    State
		wantLeft int
	}{
		{"unknown preserved", StateUnknown, 3},
		{"known status clears", StateInProgress, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePollRepo()
			task := mjPendingTask("req_clear")
			task.State = tc.state
			task.PrivateData["poll_failures"] = float64(3)
			row := repo.add(task)
			p, err := NewMJPoller(MJPollerConfig{
				Repo: repo,
				// 本用例只测计数清零语义：禁用超时，避免固定 submit_time
				// 在真实 now 下先被判超时（那会走 failTask 而不是 clear）。
				Timeout: durationPtr(0),
				Handler: func(context.Context, *Task) error {
					// 观测成功但不推进状态：clearFailures 是唯一会写计数的路径。
					return nil
				},
			})
			if err != nil {
				t.Fatalf("NewMJPoller: %v", err)
			}
			p.RunOnce(context.Background())
			got := repo.get(row.ID)
			if failures := privateInt(got.PrivateData["poll_failures"]); failures != tc.wantLeft {
				t.Fatalf("poll_failures = %d, want %d (state=%s)", failures, tc.wantLeft, tc.state)
			}
		})
	}
}

// TestMJPollerSameChannelSerialWithGap 验收 M-3：同渠道任务逐条串行，任务
// 之间间隔 TaskGap（默认 1s），不并发打同一上游渠道。
func TestMJPollerSameChannelSerialWithGap(t *testing.T) {
	repo := newFakePollRepo()
	first := repo.add(mjPendingTask("req_serial_1"))
	second := repo.add(mjPendingTask("req_serial_2"))

	var order []int64
	var sleeps []time.Duration
	p, err := NewMJPoller(MJPollerConfig{
		Repo: repo,
		// 固定 submit_time 在真实 now 下会先超时；本用例只测渠道串行。
		Timeout: durationPtr(0),
		Handler: func(_ context.Context, task *Task) error {
			order = append(order, task.ID)
			return nil
		},
		TaskGap: 2 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())

	if len(order) != 2 || order[0] != first.ID || order[1] != second.ID {
		t.Fatalf("processing order = %v, want [%d %d] serial", order, first.ID, second.ID)
	}
	if len(sleeps) != 1 || sleeps[0] != 2*time.Second {
		t.Fatalf("same-channel gap sleeps = %v, want exactly [2s]", sleeps)
	}
}

// TestMJPollerConcurrentFailureIncrementIsAtomic 验收 M-6：并发轮询失败时
// 每次失败都真实 +1（SQL/jsonb 原子递增），读-改-写会丢计数。
func TestMJPollerConcurrentFailureIncrementIsAtomic(t *testing.T) {
	repo := newFakePollRepo()
	row := repo.add(mjPendingTask("req_race_fail"))
	const workers = 20
	p, err := NewMJPoller(MJPollerConfig{
		Repo:        repo,
		MaxFailures: intPtr(1000),
		// 禁用超时：并发用例只测失败计数原子递增。
		Timeout: durationPtr(0),
		Handler: func(context.Context, *Task) error {
			return errUpstreamDown
		},
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		snapshot := repo.get(row.ID)
		go func() {
			defer wg.Done()
			<-start
			p.processTask(context.Background(), snapshot)
		}()
	}
	close(start)
	wg.Wait()

	got := repo.get(row.ID)
	if failures := privateInt(got.PrivateData["poll_failures"]); failures != workers {
		t.Fatalf("concurrent failures = %d, want %d (lost increments)", failures, workers)
	}
}

var errUpstreamDown = errors.New("upstream down")
