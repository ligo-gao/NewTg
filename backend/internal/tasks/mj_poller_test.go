package tasks

// W8/MJ-b：MJ 领取器的补偿扫描与租约续期验收（无外部依赖）。

import (
	"context"
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
