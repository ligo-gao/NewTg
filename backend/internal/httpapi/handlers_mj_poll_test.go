package httpapi

// W8/MJ-b 轮询驱动的单测（内存 CAS fake + 假上游结果）。
//
// 覆盖冻结语义：
//   - SUCCESS → 终态 CAS + billing_pending + 保留截止，交给 Settler 恰好一次；
//   - FAILURE/CANCEL → 终态失败 + refund_pending 路径（由 Settler 执行）；
//   - 未知状态 → 落 unknown 但不累计失败、不结算（R-MJ-15）；
//   - 上游错误 → 不改状态、不结算；
//   - 并发 CAS 单赢家（不得双结算）。

import (
	"context"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// fakeMJPollStore 用互斥锁模拟 ApplyPoll 的 (id, from_state) CAS。
type fakeMJPollStore struct {
	mu      sync.Mutex
	rows    map[int64]*tasks.Task
	updates []tasks.PollUpdate
}

func newFakeMJPollStore(t *tasks.Task) *fakeMJPollStore {
	copied := *t
	copied.Data = cloneAnyMap(t.Data)
	copied.PrivateData = cloneAnyMap(t.PrivateData)
	return &fakeMJPollStore{rows: map[int64]*tasks.Task{t.ID: &copied}}
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *fakeMJPollStore) ApplyPoll(_ context.Context, id int64, from tasks.State, u tasks.PollUpdate) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.State != from {
		return false, nil
	}
	row.State = u.State
	row.RawState = u.RawState
	row.ProgressPercent = u.Progress
	if row.Data == nil {
		row.Data = map[string]any{}
	}
	for k, v := range u.Data {
		row.Data[k] = v
	}
	if row.PrivateData == nil {
		row.PrivateData = map[string]any{}
	}
	for k, v := range u.Private {
		row.PrivateData[k] = v
	}
	if u.FailReason != nil {
		row.FailReason = *u.FailReason
	}
	if u.FinishTime != nil {
		row.FinishTime = u.FinishTime
	}
	if u.Quota != nil {
		row.Quota = *u.Quota
	}
	if u.RetentionDeadline != nil {
		row.RetentionDeadline = u.RetentionDeadline
	}
	s.updates = append(s.updates, u)
	return true, nil
}

func (s *fakeMJPollStore) row(id int64) *tasks.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id]
}

// fakeMJSettler 记录结算调用。
type fakeMJSettler struct {
	mu    sync.Mutex
	calls []int64
}

func (s *fakeMJSettler) SettleTerminal(_ context.Context, task *tasks.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task == nil {
		return
	}
	s.calls = append(s.calls, task.ID)
}

func (s *fakeMJSettler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// mjPollFixture 是一次驱动用例的装配。
type mjPollFixture struct {
	driver   *MJPollDriver
	store    *fakeMJPollStore
	channels *fakeMJChannels
	querier  *fakeMJQuerier
	settler  *fakeMJSettler
	sealer   *SecretSealer
	now      time.Time
}

func newMJPollFixture(t *testing.T, task *tasks.Task) *mjPollFixture {
	t.Helper()
	sealer := NewSecretSealer("mj-poll-test-key")
	sealed, err := sealer.Seal([]byte(`{"api_key":"poll-channel-key"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{
		10: {ID: 10, BaseURL: "http://upstream.example", Credentials: sealed, Status: 1, Type: mj.ChannelTypeMidjourney},
	}}
	store := newFakeMJPollStore(task)
	querier := &fakeMJQuerier{}
	settler := &fakeMJSettler{}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	driver, err := NewMJPollDriver(MJPollDriverConfig{
		Store: store, Channels: channels, Sealer: sealer, Client: querier, Settler: settler,
		Now:           func() time.Time { return now },
		RetentionDays: func() int64 { return 30 },
	})
	if err != nil {
		t.Fatalf("NewMJPollDriver: %v", err)
	}
	return &mjPollFixture{driver: driver, store: store, channels: channels,
		querier: querier, settler: settler, sealer: sealer, now: now}
}

func mjPollTask(state tasks.State) *tasks.Task {
	return &tasks.Task{
		ID: 1, PublicTaskID: "task_poll", UpstreamTaskID: "mj_poll",
		Family: tasks.FamilyMJ, Action: string(mj.ActionImagine),
		UserID: 42, ChannelID: 10, State: state, Quota: 50,
		Properties:  map[string]any{mj.PropertyPrompt: "a cat"},
		Data:        map[string]any{mj.DataImageURL: "https://cdn/old.png"},
		PrivateData: map[string]any{mj.PrivateRequestID: "req_1", mj.PrivateTokenID: float64(7)},
	}
}

func TestMJPollTaskSuccessSettlesExactlyOnce(t *testing.T) {
	fixture := newMJPollFixture(t, mjPollTask(tasks.StateInProgress))
	task := mjPollTask(tasks.StateInProgress)
	fixture.querier.queryResult = &mj.QueryResult{
		Status: "SUCCESS", Progress: "100%",
		ImageURL: "https://cdn/new.png", VideoURLs: []string{"https://cdn/v.mp4"},
		Properties: map[string]any{"numberOfQueues": float64(0)},
	}

	if err := fixture.driver.PollTask(context.Background(), task); err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	row := fixture.store.row(task.ID)
	if row.State != tasks.StateSuccess || row.RawState != "SUCCESS" || row.ProgressPercent != 100 {
		t.Fatalf("row state/raw/progress = %s/%q/%d", row.State, row.RawState, row.ProgressPercent)
	}
	if row.Data[mj.DataImageURL] != "https://cdn/new.png" {
		t.Fatalf("image url not backfilled: %v", row.Data)
	}
	if row.PrivateData["billing_pending"] != true {
		t.Fatalf("terminal CAS must write billing_pending in the same update: %v", row.PrivateData)
	}
	if row.FinishTime == nil {
		t.Fatalf("finish_time not written")
	}
	wantDeadline := fixture.now.Add(30 * 24 * time.Hour)
	if row.RetentionDeadline == nil || !row.RetentionDeadline.Equal(wantDeadline) {
		t.Fatalf("retention deadline = %v, want %v", row.RetentionDeadline, wantDeadline)
	}
	if fixture.settler.count() != 1 {
		t.Fatalf("settler calls = %d, want exactly 1", fixture.settler.count())
	}
	if fixture.querier.queries() != 1 {
		t.Fatalf("upstream queries = %d, want 1", fixture.querier.queries())
	}
}

func TestMJPollTaskFailureUsesRefundPath(t *testing.T) {
	for _, raw := range []string{"FAILURE", "CANCEL"} {
		t.Run(raw, func(t *testing.T) {
			fixture := newMJPollFixture(t, mjPollTask(tasks.StateInProgress))
			task := mjPollTask(tasks.StateInProgress)
			fixture.querier.queryResult = &mj.QueryResult{
				Status: raw, Progress: "100%", FailReason: "user cancelled",
			}
			if err := fixture.driver.PollTask(context.Background(), task); err != nil {
				t.Fatalf("PollTask: %v", err)
			}
			row := fixture.store.row(task.ID)
			if row.State != tasks.StateFailure || row.RawState != raw {
				t.Fatalf("row state/raw = %s/%q, want failure/%s", row.State, row.RawState, raw)
			}
			if row.FailReason != "user cancelled" {
				t.Fatalf("fail reason = %q", row.FailReason)
			}
			if fixture.settler.count() != 1 {
				t.Fatalf("settler calls = %d, want exactly 1", fixture.settler.count())
			}
		})
	}
}

func TestMJPollTaskUnknownDoesNotAccumulateFailure(t *testing.T) {
	task := mjPollTask(tasks.StateInProgress)
	task.PrivateData["poll_failures"] = float64(3)
	fixture := newMJPollFixture(t, task)
	live := mjPollTask(tasks.StateInProgress)
	live.PrivateData["poll_failures"] = float64(3)
	fixture.querier.queryResult = &mj.QueryResult{Status: "SOMETHING_NEW", Progress: "10%"}

	if err := fixture.driver.PollTask(context.Background(), live); err != nil {
		t.Fatalf("unknown status must not be a handler error: %v", err)
	}
	row := fixture.store.row(live.ID)
	if row.State != tasks.StateUnknown || row.RawState != "SOMETHING_NEW" {
		t.Fatalf("row state/raw = %s/%q, want unknown/SOMETHING_NEW", row.State, row.RawState)
	}
	if got := row.PrivateData["poll_failures"]; got != float64(3) {
		t.Fatalf("unknown must not accumulate failures: poll_failures = %v", got)
	}
	if fixture.settler.count() != 0 {
		t.Fatalf("unknown status must never settle")
	}
}

func TestMJPollTaskUpstreamErrorKeepsState(t *testing.T) {
	fixture := newMJPollFixture(t, mjPollTask(tasks.StateInProgress))
	task := mjPollTask(tasks.StateInProgress)
	fixture.querier.queryErr = &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionUpstreamError, Status: 502}

	if err := fixture.driver.PollTask(context.Background(), task); err == nil {
		t.Fatalf("upstream error must be returned so MJPoller counts a failure")
	}
	row := fixture.store.row(task.ID)
	if row.State != tasks.StateInProgress || row.RawState != "" {
		t.Fatalf("upstream error changed state: %s/%q", row.State, row.RawState)
	}
	if len(fixture.store.updates) != 0 || fixture.settler.count() != 0 {
		t.Fatalf("upstream error must not write or settle")
	}
}

func TestMJPollTaskConcurrentCASHasSingleSettler(t *testing.T) {
	fixture := newMJPollFixture(t, mjPollTask(tasks.StateInProgress))
	fixture.querier.queryResult = &mj.QueryResult{Status: "SUCCESS", Progress: "100%"}

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		task := mjPollTask(tasks.StateInProgress)
		go func() {
			<-start
			errs <- fixture.driver.PollTask(context.Background(), task)
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("PollTask: %v", err)
		}
	}
	if fixture.settler.count() != 1 {
		t.Fatalf("concurrent polls settled %d times, want exactly 1", fixture.settler.count())
	}
	if row := fixture.store.row(1); row.State != tasks.StateSuccess {
		t.Fatalf("row state = %s, want success", row.State)
	}
}

func TestMJPollTaskTerminalRowOnlySettles(t *testing.T) {
	task := mjPollTask(tasks.StateSuccess)
	fixture := newMJPollFixture(t, task)
	live := mjPollTask(tasks.StateSuccess)
	if err := fixture.driver.PollTask(context.Background(), live); err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if fixture.querier.queries() != 0 {
		t.Fatalf("terminal row must not query upstream")
	}
	if fixture.settler.count() != 1 {
		t.Fatalf("terminal row must settle exactly once")
	}
}

func TestMJPollTaskRetentionDisabled(t *testing.T) {
	fixture := newMJPollFixture(t, mjPollTask(tasks.StateInProgress))
	disabled, err := NewMJPollDriver(MJPollDriverConfig{
		Store: fixture.store, Channels: fixture.channels, Sealer: fixture.sealer,
		Client: fixture.querier, Settler: fixture.settler,
		RetentionDays: func() int64 { return 0 },
	})
	if err != nil {
		t.Fatalf("NewMJPollDriver: %v", err)
	}
	fixture.querier.queryResult = &mj.QueryResult{Status: "SUCCESS", Progress: "100%"}
	if err := disabled.PollTask(context.Background(), mjPollTask(tasks.StateInProgress)); err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if row := fixture.store.row(1); row.RetentionDeadline != nil {
		t.Fatalf("retention days=0 must not write a deadline: %v", row.RetentionDeadline)
	}
}

// TestMJPollTaskRejectsNonMJBeforeSettling 验收 M-1：family 防御必须先于
// SettleTerminal，避免把误送的插件终态行按 MJ 语义结算。
func TestMJPollTaskRejectsNonMJBeforeSettling(t *testing.T) {
	fixture := newMJPollFixture(t, mjPollTask(tasks.StateSuccess))
	pluginTask := mjPollTask(tasks.StateSuccess)
	pluginTask.Family = tasks.FamilyPlugin

	if err := fixture.driver.PollTask(context.Background(), pluginTask); err == nil {
		t.Fatalf("non-mj terminal task must be rejected")
	}
	if fixture.settler.count() != 0 {
		t.Fatalf("non-mj task must not be settled, got %d calls", fixture.settler.count())
	}
	if fixture.querier.queries() != 0 {
		t.Fatalf("non-mj task must not query upstream")
	}
}
