package reconcile

import (
	"context"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

// ---- in-memory collaborators (the narrow interfaces from runner.go) ----

type fakeAccounts struct{ accounts []store.UpstreamAccount }

func (f *fakeAccounts) ListEnabled(context.Context) ([]store.UpstreamAccount, error) {
	return f.accounts, nil
}

func (f *fakeAccounts) Get(_ context.Context, id int64) (*store.UpstreamAccount, error) {
	for i := range f.accounts {
		if f.accounts[i].ID == id {
			return &f.accounts[i], nil
		}
	}
	return nil, store.ErrUpstreamAccountNotFound
}

func (f *fakeAccounts) ChannelIDs(context.Context, int64) ([]int64, error) { return nil, nil }

type fakeSnapshots struct{ rows []store.UpstreamSnapshot }

func (f *fakeSnapshots) ListRange(context.Context, int64, time.Time, time.Time) ([]store.UpstreamSnapshot, error) {
	return f.rows, nil
}

type fakeUsage struct {
	rows   []store.LocalUsage
	latest time.Time
}

func (f *fakeUsage) Aggregate(context.Context, int64, []int64, string, time.Time, time.Time) ([]store.LocalUsage, error) {
	return f.rows, nil
}

func (f *fakeUsage) LatestCreatedAt(context.Context) (time.Time, error) { return f.latest, nil }

type fakeRunStore struct {
	runs  []store.ReconcileRun
	items [][]store.ReconcileItem
}

func (f *fakeRunStore) InsertRun(_ context.Context, run store.ReconcileRun) (int64, error) {
	f.runs = append(f.runs, run)
	return int64(len(f.runs)), nil
}

func (f *fakeRunStore) FinishRun(_ context.Context, id int64, status, runErr string) error {
	f.runs[id-1].Status, f.runs[id-1].Error = status, runErr
	return nil
}

func (f *fakeRunStore) InsertItems(_ context.Context, items []store.ReconcileItem) error {
	f.items = append(f.items, items)
	return nil
}

type fakeSettings struct{ s Settings }

func (f *fakeSettings) ReconcileSettings(context.Context) (Settings, error) { return f.s, nil }

func runnerFixture(now time.Time) (*Runner, *fakeRunStore) {
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	zero := int64(0)
	cent := int64(100)
	st := &fakeRunStore{}
	r, err := NewRunner(RunnerConfig{
		Accounts: &fakeAccounts{accounts: []store.UpstreamAccount{{ID: 1, Status: 1, Timezone: "Asia/Shanghai"}}},
		Snapshots: &fakeSnapshots{rows: []store.UpstreamSnapshot{
			{AccountID: 1, UsageDate: day, UpstreamModel: "A", CacheHitTokens: &zero, CacheMissTokens: &zero, OutputTokens: &zero, AmountMinor: &cent},
			{AccountID: 1, UsageDate: day, UpstreamModel: "B"},
		}},
		Usage: &fakeUsage{
			rows:   []store.LocalUsage{{UsageDate: day, Model: "A", Requests: 10, CostCredits: 100}},
			latest: now.Add(-time.Minute),
		},
		Store:    st,
		Settings: &fakeSettings{s: Settings{Enabled: true, LookbackDays: 3, TokenBP: 100, MinDeltaTokens: 1000, AmountToleranceMinor: 100, QuotaPerUnit: 100}},
		Clock:    func() time.Time { return now },
	})
	if err != nil {
		panic(err)
	}
	return r, st
}

// ---- the four runner tests from the task brief ----

func TestRunnerBuildsItemsFromLocalAndSnapshotUnion(t *testing.T) {
	r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
	out, err := r.Run(context.Background(), "manual", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Items != 2 {
		t.Fatalf("items = %d, want 2", out.Items)
	}
	items := st.items[0]
	if len(items) != 2 || items[0].UpstreamModel != "A" || items[1].UpstreamModel != "B" {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Severity != "ok" || items[1].Severity != "missing_local" {
		t.Fatalf("severity = %s / %s", items[0].Severity, items[1].Severity)
	}
	if st.runs[0].Status != "succeeded" || st.runs[0].QuotaPerUnit != 100 || st.runs[0].TokenToleranceBP != 100 {
		t.Fatalf("run row = %+v", st.runs[0])
	}
}

func TestRunnerMarksPartialWhenLogStreamLags(t *testing.T) {
	r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
	r.usage.(*fakeUsage).latest = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC) // 2.5h lag
	if _, err := r.Run(context.Background(), "worker", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.runs[0].Status != "partial" || st.runs[0].Error != "log_stream_lag" {
		t.Fatalf("run = %+v", st.runs[0])
	}
	if len(st.items[0]) != 2 {
		t.Fatalf("items must still be written on partial runs")
	}
}

func TestRunnerRerunCreatesANewRunWithTheSameItemKeys(t *testing.T) {
	r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
	_, _ = r.Run(context.Background(), "manual", nil)
	_, _ = r.Run(context.Background(), "manual", nil)
	if len(st.runs) != 2 || len(st.items) != 2 {
		t.Fatalf("runs=%d item batches=%d, want 2/2", len(st.runs), len(st.items))
	}
	if st.items[0][0].UpstreamModel != st.items[1][0].UpstreamModel {
		t.Fatal("a rerun must produce the same item keys")
	}
}

func TestRunnerMarksExpiredWindow(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	r, st := runnerFixture(now)
	ago := now.AddDate(0, 0, -100) // older than the default 90-day retention
	r.settings.(*fakeSettings).s.LookbackDays = 120
	// Local logs for that date are gone; only the upstream snapshot remains.
	r.usage.(*fakeUsage).rows = nil
	r.snapshots.(*fakeSnapshots).rows = []store.UpstreamSnapshot{{AccountID: 1, UsageDate: ago, UpstreamModel: "A"}}
	_, _ = r.Run(context.Background(), "manual", nil)
	if st.items[0][0].Severity != "local_expired" {
		t.Fatalf("severity = %s, want local_expired", st.items[0][0].Severity)
	}
}

// ---- extra coverage required by the task instructions ----

// AggregateLocal only converts the store's per-(day, model) rows; repeated keys
// are summed defensively so a legacy/upstream_model collision cannot silently
// drop half the usage.
func TestAggregateLocalSumsRowsPerDayAndModel(t *testing.T) {
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	rows := []store.LocalUsage{
		{UsageDate: day, Model: "A", Requests: 2, CacheHitTokens: 10, CostCredits: 30},
		{UsageDate: day, Model: "A", Requests: 3, CacheHitTokens: 5, OutputTokens: 7, CostCredits: 70},
		{UsageDate: next, Model: "A", Requests: 1, CostCredits: 100},
	}
	got := AggregateLocal(rows)
	if len(got) != 2 {
		t.Fatalf("keys = %d, want 2", len(got))
	}
	a := got[Key{UsageDate: day, UpstreamModel: "A"}]
	if a.Requests != 5 || a.CacheHitTokens != 15 || a.OutputTokens != 7 || a.CostCredits != 100 {
		t.Fatalf("day aggregate = %+v", a)
	}
	if got[Key{UsageDate: next, UpstreamModel: "A"}].Requests != 1 {
		t.Fatalf("next-day aggregate = %+v", got[Key{UsageDate: next, UpstreamModel: "A"}])
	}
}

// A local aggregate without a snapshot must still carry the gateway-side
// revenue; leaving it zero because the console numbers were not imported yet
// would systematically under-report income and margin.
func TestRunnerKeepsRevenueWhenSnapshotIsMissing(t *testing.T) {
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
	r.snapshots.(*fakeSnapshots).rows = nil
	r.usage.(*fakeUsage).rows = []store.LocalUsage{{UsageDate: day, Model: "A", CostCredits: 5000}}
	if _, err := r.Run(context.Background(), "manual", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	it := st.items[0][0]
	if it.Severity != "missing_snapshot" {
		t.Fatalf("severity = %s, want missing_snapshot", it.Severity)
	}
	if it.RevenueMinor == nil || *it.RevenueMinor != 5000 {
		t.Fatalf("revenue = %v, want 5000", it.RevenueMinor)
	}
	if it.UpstreamCostMinor != nil || it.MarginMinor != nil || it.MarginBP != nil {
		t.Fatalf("cost/margin must stay nil without a snapshot: %+v", it)
	}
}
