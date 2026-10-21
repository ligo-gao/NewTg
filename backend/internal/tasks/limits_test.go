package tasks

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件验收 W9-B4（G-M6D-27）的构造期三态语义：
//
//	未配置（nil） → 设计默认（1440m / 20）
//	显式 0        → 禁用该判定
//	正值          → 按值
//
// 真实 PG 的行为验收见 limits_integration_test.go；这里用内存 fake 把三态
// 与边界（恰好等于阈值 / 超一）钉死，避免每个变异都要起数据库才能发现。

func intPtr(v int) *int                          { return &v }
func durationPtr(v time.Duration) *time.Duration { return &v }

func newLimitsTestPoller(t *testing.T, cfg PollerConfig) *Poller {
	t.Helper()
	if cfg.Repo == nil {
		cfg.Repo = newFakePollRepo()
	}
	if cfg.Plugins == nil {
		cfg.Plugins = newTestSnapshotter(t)
	}
	if cfg.Billing == nil {
		cfg.Billing = &fakeHandoff{}
	}
	cfg.Now = func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	p, err := NewPoller(cfg)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

// TestPollerLimitsUnsetUseDesignDefaults：nil 必须回落 1440m/20，而不是 0。
func TestPollerLimitsUnsetUseDesignDefaults(t *testing.T) {
	p := newLimitsTestPoller(t, PollerConfig{})
	if p.timeout != DefaultTaskTimeout {
		t.Fatalf("unset timeout = %v, want %v", p.timeout, DefaultTaskTimeout)
	}
	if p.maxFailures != DefaultMaxPollFailures {
		t.Fatalf("unset maxFailures = %d, want %d", p.maxFailures, DefaultMaxPollFailures)
	}
	// 边界方向按设计/基线冻结为严格大于（`now - 上限*60 > submit_time`）：
	// 恰好等于阈值**不**超时，超过一秒才超时。
	exact := p.now().Add(-DefaultTaskTimeout)
	task := &Task{SubmitTime: &exact}
	if p.timedOut(task) {
		t.Fatalf("exactly the default timeout must not time out (strict >)")
	}
	over := exact.Add(-time.Second)
	task.SubmitTime = &over
	if !p.timedOut(task) {
		t.Fatalf("one second past the default timeout must time out")
	}
}

// TestPollerLimitsExplicitZeroDisables：显式 0 是禁用，绝不能当默认。
func TestPollerLimitsExplicitZeroDisables(t *testing.T) {
	p := newLimitsTestPoller(t, PollerConfig{
		Timeout:     durationPtr(0),
		MaxFailures: intPtr(0),
	})
	if p.timeout != 0 || p.maxFailures != 0 {
		t.Fatalf("explicit zero must be preserved: timeout=%v maxFailures=%d", p.timeout, p.maxFailures)
	}
	// 极旧的任务也不得判超时。
	longAgo := p.now().Add(-100 * 24 * time.Hour)
	if p.timedOut(&Task{SubmitTime: &longAgo}) {
		t.Fatalf("timeout=0 must disable the timeout judgment")
	}

	// 失败计数不得触发上限退款：连喂 50 次失败，任务仍是 submitted。
	repo := newFakePollRepo()
	task := repo.add(submittedTask(1, 0)) // channel 0 → channel_missing 计一次失败
	handoff := &fakeHandoff{}
	p = newLimitsTestPoller(t, PollerConfig{
		Repo: repo, Billing: handoff,
		Timeout: durationPtr(0), MaxFailures: intPtr(0),
	})
	for i := 0; i < 50; i++ {
		p.RunOnce(context.Background())
	}
	got := repo.get(task.ID)
	if got.State.Terminal() {
		t.Fatalf("maxFailures=0 must disable refund-on-failures: state=%s", got.State)
	}
	if handoff.refundCount() != 0 {
		t.Fatalf("maxFailures=0 refunded %d times", handoff.refundCount())
	}
	if privateInt(got.PrivateData["poll_failures"]) != 50 {
		t.Fatalf("failures must still be counted (observability), got %v", got.PrivateData["poll_failures"])
	}
}

// TestPollerLimitsPositiveValuesAndBoundaries：正值按值生效，边界含等号。
func TestPollerLimitsPositiveValuesAndBoundaries(t *testing.T) {
	p := newLimitsTestPoller(t, PollerConfig{
		Timeout:     durationPtr(time.Minute),
		MaxFailures: intPtr(2),
	})
	if p.timeout != time.Minute || p.maxFailures != 2 {
		t.Fatalf("positive config not preserved: timeout=%v maxFailures=%d", p.timeout, p.maxFailures)
	}
	exact := p.now().Add(-time.Minute)
	if p.timedOut(&Task{SubmitTime: &exact}) {
		t.Fatalf("exactly the configured timeout must not time out (strict >)")
	}
	over := exact.Add(-time.Second)
	if !p.timedOut(&Task{SubmitTime: &over}) {
		t.Fatalf("one second past the configured timeout must time out")
	}
	inside := exact.Add(time.Second)
	if p.timedOut(&Task{SubmitTime: &inside}) {
		t.Fatalf("one second inside the configured timeout must not time out")
	}

	// 失败上限 2：第 1 次失败只累计，第 2 次（恰好等于阈值）触发 FAILURE+退款。
	repo := newFakePollRepo()
	task := repo.add(submittedTask(1, 0))
	handoff := &fakeHandoff{}
	p = newLimitsTestPoller(t, PollerConfig{
		Repo: repo, Billing: handoff,
		Timeout: durationPtr(0), MaxFailures: intPtr(2),
	})
	p.RunOnce(context.Background())
	got := repo.get(task.ID)
	if got.State.Terminal() || privateInt(got.PrivateData["poll_failures"]) != 1 {
		t.Fatalf("first failure must only accumulate: state=%s failures=%v",
			got.State, got.PrivateData["poll_failures"])
	}
	p.RunOnce(context.Background())
	got = repo.get(task.ID)
	if got.State != StateFailure || got.FailReason != "poll_failures_exceeded" {
		t.Fatalf("second failure must hit the limit: state=%s reason=%s", got.State, got.FailReason)
	}
	if handoff.refundCount() != 1 {
		t.Fatalf("failure limit must refund exactly once, got %d", handoff.refundCount())
	}
}

// TestMJPollerLimitsThreeStates：MJ 领取器与插件 poller 同口径。
func TestMJPollerLimitsThreeStates(t *testing.T) {
	cases := []struct {
		name        string
		cfg         MJPollerConfig
		wantTimeout time.Duration
		wantMax     int
	}{
		{"unset", MJPollerConfig{}, DefaultTaskTimeout, DefaultMaxPollFailures},
		{"zero", MJPollerConfig{Timeout: durationPtr(0), MaxFailures: intPtr(0)}, 0, 0},
		{"positive", MJPollerConfig{Timeout: durationPtr(time.Minute), MaxFailures: intPtr(3)}, time.Minute, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePollRepo()
			tc.cfg.Repo = repo
			p, err := NewMJPoller(tc.cfg)
			if err != nil {
				t.Fatalf("NewMJPoller: %v", err)
			}
			if p.timeout != tc.wantTimeout || p.maxFailures != tc.wantMax {
				t.Fatalf("timeout=%v maxFailures=%d, want %v/%d",
					p.timeout, p.maxFailures, tc.wantTimeout, tc.wantMax)
			}
		})
	}
}

// TestMJPollerNilHandlerNeverTimesOut：Handler=nil（A0 生产装配）时，
// 即使行已超过任何超时阈值，也不得改状态——否则会出现「已失败但无人结算」。
func TestMJPollerNilHandlerNeverTimesOut(t *testing.T) {
	repo := newFakePollRepo()
	task := mjSubmittedTask(1, 7)
	longAgo := time.Now().Add(-100 * 24 * time.Hour)
	task.SubmitTime = &longAgo
	repo.add(task)

	p, err := NewMJPoller(MJPollerConfig{Repo: repo, Timeout: durationPtr(time.Minute)})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())
	got := repo.get(task.ID)
	if got.State != StateSubmitted || privateInt(got.PrivateData["poll_failures"]) != 0 {
		t.Fatalf("MJ row must stay untouched with Handler=nil: %#v", got)
	}
}

// TestMJPollerEnforcesTimeoutAndFailureLimit：Handler 注入后，同一超时/
// 失败上限生效；失败在达到阈值时转 FAILURE 并标 billing_pending。
func TestMJPollerEnforcesTimeoutAndFailureLimit(t *testing.T) {
	// 超时：Handler 不得被调用。
	repo := newFakePollRepo()
	task := repo.add(mjSubmittedTask(1, 7))
	longAgo := time.Now().Add(-2 * time.Hour)
	task.SubmitTime = &longAgo
	repo.add(task)

	called := 0
	p, err := NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: durationPtr(time.Minute),
		Handler: func(context.Context, *Task) error { called++; return nil },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())
	got := repo.get(task.ID)
	if called != 0 {
		t.Fatalf("handler must not run for a timed-out MJ task, called %d", called)
	}
	if got.State != StateFailure || got.FailReason != "task_timeout" ||
		!boolFlag(got.PrivateData["billing_pending"]) {
		t.Fatalf("timed-out MJ task not failed correctly: %#v", got)
	}

	// 失败上限：Timeout=0（禁用超时），2 次 handler 错误后转 FAILURE。
	repo = newFakePollRepo()
	task = repo.add(mjSubmittedTask(2, 7))
	p, err = NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: durationPtr(0), MaxFailures: intPtr(2),
		Handler: func(context.Context, *Task) error { return errors.New("upstream down") },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())
	got = repo.get(task.ID)
	if got.State.Terminal() || privateInt(got.PrivateData["poll_failures"]) != 1 {
		t.Fatalf("first MJ handler failure must only accumulate: %#v", got)
	}
	p.RunOnce(context.Background())
	got = repo.get(task.ID)
	if got.State != StateFailure || got.FailReason != "poll_failures_exceeded" ||
		!boolFlag(got.PrivateData["billing_pending"]) {
		t.Fatalf("MJ failure limit not enforced: %#v", got)
	}
}

// TestMJPollerTimeoutBoundaryMatchesPluginPoller：MJ 与插件族共用严格大于
// 的边界（Now 接缝注入固定时钟）。
func TestMJPollerTimeoutBoundaryMatchesPluginPoller(t *testing.T) {
	fixed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	oneMinute := time.Minute
	zeroInt := 0

	repo := newFakePollRepo()
	exact := mjSubmittedTask(1, 7)
	exactTime := fixed.Add(-oneMinute)
	exact.SubmitTime = &exactTime
	repo.add(exact)

	called := 0
	p, err := NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: &oneMinute, MaxFailures: &zeroInt,
		Now:     func() time.Time { return fixed },
		Handler: func(context.Context, *Task) error { called++; return nil },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())
	if called != 1 {
		t.Fatalf("exactly the timeout must not fail the row (strict >), handler calls=%d", called)
	}

	over := mjSubmittedTask(2, 7)
	overTime := fixed.Add(-oneMinute - time.Second)
	over.SubmitTime = &overTime
	repo.add(over)
	p.RunOnce(context.Background())
	got := repo.get(over.ID)
	if got.State != StateFailure || got.FailReason != "task_timeout" {
		t.Fatalf("one second past the timeout must fail: state=%s reason=%s",
			got.State, got.FailReason)
	}
}

// TestMJPollerZeroDisablesTimeoutAndFailureLimit：显式 0 对 MJ 同样禁用。
func TestMJPollerZeroDisablesTimeoutAndFailureLimit(t *testing.T) {
	repo := newFakePollRepo()
	task := mjSubmittedTask(1, 7)
	longAgo := time.Now().Add(-100 * 24 * time.Hour)
	task.SubmitTime = &longAgo
	repo.add(task)

	called := 0
	p, err := NewMJPoller(MJPollerConfig{
		Repo: repo, Timeout: durationPtr(0), MaxFailures: intPtr(0),
		Handler: func(context.Context, *Task) error { called++; return errors.New("upstream down") },
	})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	for i := 0; i < 10; i++ {
		p.RunOnce(context.Background())
	}
	got := repo.get(task.ID)
	if called != 10 {
		t.Fatalf("zero limits must not stop the handler, called %d", called)
	}
	if got.State.Terminal() {
		t.Fatalf("zero limits must not fail the MJ task, state=%s", got.State)
	}
	if privateInt(got.PrivateData["poll_failures"]) != 10 {
		t.Fatalf("failures still counted, got %v", got.PrivateData["poll_failures"])
	}
}
