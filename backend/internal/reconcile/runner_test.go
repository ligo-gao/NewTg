package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

type fakeSnapshots struct {
	rows []store.UpstreamSnapshot

	// 捕获 ListRange 的入参，用来钉住账号与日期区间。
	gotAccountID int64
	gotFrom      time.Time
	gotTo        time.Time
}

func (f *fakeSnapshots) ListRange(_ context.Context, accountID int64, from, to time.Time) ([]store.UpstreamSnapshot, error) {
	f.gotAccountID, f.gotFrom, f.gotTo = accountID, from, to
	return f.rows, nil
}

type fakeUsage struct {
	rows   []store.LocalUsage
	latest time.Time

	// 捕获 Aggregate 的入参，用来钉住账号时区与窗口换算。
	gotTZ    string
	gotStart time.Time
	gotEnd   time.Time

	// 故障注入：对 errAccount 这个账号返回 aggregateErr。
	errAccount   int64
	aggregateErr error

	// beforeAggregate 在返回前执行，用来模拟「取数失败时调用方 ctx 已被
	// 手动触发的 timeout 取消」。
	beforeAggregate func()
}

func (f *fakeUsage) Aggregate(_ context.Context, accountID int64, _ []int64, tz string, start, end time.Time) ([]store.LocalUsage, error) {
	f.gotTZ, f.gotStart, f.gotEnd = tz, start, end
	if f.beforeAggregate != nil {
		f.beforeAggregate()
	}
	if f.aggregateErr != nil && accountID == f.errAccount {
		return nil, f.aggregateErr
	}
	return f.rows, nil
}

func (f *fakeUsage) LatestCreatedAt(context.Context) (time.Time, error) { return f.latest, nil }

type fakeRunStore struct {
	runs      []store.ReconcileRun
	items     [][]store.ReconcileItem
	itemsErr  error
	finishErr error

	// 捕获 FinishRun 的入参：收尾必须使用不随调用方取消的上下文。
	finishCalls      int
	finishStatus     string
	finishCtxErr     error
	finishCtxHasDone bool
}

func (f *fakeRunStore) InsertRun(_ context.Context, run store.ReconcileRun) (int64, error) {
	f.runs = append(f.runs, run)
	return int64(len(f.runs)), nil
}

func (f *fakeRunStore) FinishRun(ctx context.Context, id int64, status, runErr string) error {
	f.finishCalls++
	f.finishStatus = status
	f.finishCtxErr = ctx.Err()
	f.finishCtxHasDone = ctx.Done() != nil
	f.runs[id-1].Status, f.runs[id-1].Error = status, runErr
	return f.finishErr
}

func (f *fakeRunStore) InsertItems(_ context.Context, items []store.ReconcileItem) error {
	if f.itemsErr != nil {
		return f.itemsErr
	}
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
	// 金额非 nil，用来钉住 SnapshotAmountMinor 在「不做比较」的分支上也会
	// 透传（若赋值被挪进 compare 分支，这里会变 nil）。
	amount := int64(1234)
	r.usage.(*fakeUsage).rows = nil
	r.snapshots.(*fakeSnapshots).rows = []store.UpstreamSnapshot{{
		AccountID: 1, UsageDate: ago, UpstreamModel: "A", AmountMinor: &amount,
	}}
	_, _ = r.Run(context.Background(), "manual", nil)
	item := st.items[0][0]
	if item.Severity != "local_expired" {
		t.Fatalf("severity = %s, want local_expired", item.Severity)
	}
	if item.SnapshotAmountMinor == nil || *item.SnapshotAmountMinor != amount {
		t.Fatalf("expired item snapshot_amount_minor = %v, want %d", item.SnapshotAmountMinor, amount)
	}
	if item.UpstreamCostMinor == nil || *item.UpstreamCostMinor != amount {
		t.Fatalf("expired item upstream_cost_minor = %v, want %d", item.UpstreamCostMinor, amount)
	}
	// local_expired 必须在 Compare 之前短路：这个快照的 token 维度全为
	// nil，若真的比较过，SnapshotPartial 会变成 true。
	if item.SnapshotPartial {
		t.Fatal("local_expired item must not run Compare (SnapshotPartial was set)")
	}
	if item.DeltaCacheHitTokens != nil || item.DeltaCacheMissTokens != nil ||
		item.DeltaOutputTokens != nil || item.DeltaTokenBP != nil {
		t.Fatalf("local_expired item must not carry deltas: %+v", item)
	}
}

// ---- extra coverage required by the task instructions ----

// AggregateLocal only converts the store's per-(day, model) rows; repeated keys
// are summed defensively so a legacy/upstream_model collision cannot silently
// drop half the usage.
func TestAggregateLocalSumsRowsPerDayAndModel(t *testing.T) {
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	rows := []store.LocalUsage{
		{UsageDate: day, Model: "A", Requests: 2, CacheHitTokens: 10, CostCredits: 30},
		{UsageDate: day, Model: "A", Requests: 3, CacheHitTokens: 5, OutputTokens: 7, CostCredits: 70},
		{UsageDate: next, Model: "A", Requests: 1, CostCredits: 100},
		// Shanghai 的 2026-10-23 00:00 是 UTC 的 10-22 16:00：按键必须
		// 归到 10-23，而不是被 UTC 截断到前一天。
		{UsageDate: time.Date(2026, 10, 23, 0, 0, 0, 0, shanghai), Model: "B", Requests: 1},
	}
	got := AggregateLocal(rows)
	if len(got) != 3 {
		t.Fatalf("keys = %d, want 3", len(got))
	}
	a := got[Key{UsageDate: day, UpstreamModel: "A"}]
	if a.Requests != 5 || a.CacheHitTokens != 15 || a.OutputTokens != 7 || a.CostCredits != 100 {
		t.Fatalf("day aggregate = %+v", a)
	}
	if got[Key{UsageDate: next, UpstreamModel: "A"}].Requests != 1 {
		t.Fatalf("next-day aggregate = %+v", got[Key{UsageDate: next, UpstreamModel: "A"}])
	}
	if b := got[Key{UsageDate: day, UpstreamModel: "B"}]; b.Requests != 1 {
		t.Fatalf("Shanghai-midnight row = %+v, want it keyed to 2026-10-23", b)
	}
	if _, wrong := got[Key{UsageDate: day.AddDate(0, 0, -1), UpstreamModel: "B"}]; wrong {
		t.Fatal("Shanghai-midnight row must not be keyed to the UTC date 2026-10-22")
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

// SnapshotAmountMinor 必须与 UpstreamCostMinor 一样透传：fixture 里模型 A
// 有金额、模型 B 没有，两者都要被钉住（0 与 NULL 语义不同）。
func TestRunnerCarriesSnapshotAmountMinor(t *testing.T) {
	r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
	if _, err := r.Run(context.Background(), "manual", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	items := st.items[0]
	if items[0].SnapshotAmountMinor == nil || *items[0].SnapshotAmountMinor != 100 {
		t.Fatalf("snapshot_amount_minor = %v, want 100", items[0].SnapshotAmountMinor)
	}
	if items[0].UpstreamCostMinor == nil || *items[0].UpstreamCostMinor != 100 {
		t.Fatalf("upstream_cost_minor = %v, want 100", items[0].UpstreamCostMinor)
	}
	if items[1].SnapshotAmountMinor != nil || items[1].UpstreamCostMinor != nil {
		t.Fatalf("model B has no amount; got %v / %v",
			items[1].SnapshotAmountMinor, items[1].UpstreamCostMinor)
	}
}

// runs.window_start/window_end 是包含两端的本地日历日（设计 §4.5 `-- 含`）：
// lookback=3 时应记录最早（today-3）到最晚（today）的对账日。
func TestRunnerRecordsInclusiveLocalDateWindow(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	r, st := runnerFixture(now)
	if _, err := r.Run(context.Background(), "manual", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := st.runs[0]
	if got := run.WindowStart.In(loc).Format("2006-01-02"); got != "2026-10-21" {
		t.Fatalf("window_start = %s, want 2026-10-21", got)
	}
	if got := run.WindowEnd.In(loc).Format("2006-01-02"); got != "2026-10-24" {
		t.Fatalf("window_end = %s, want 2026-10-24", got)
	}
	if want := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC); !run.WindowStart.Equal(want) {
		t.Fatalf("window_start instant = %s, want %s", run.WindowStart, want)
	}
	if want := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC); !run.WindowEnd.Equal(want) {
		t.Fatalf("window_end instant = %s, want %s", run.WindowEnd, want)
	}
}

// today 必须取自账号时区。now=2026-10-24T20:00Z 在 Shanghai 已是 10-25：
// 若误把 now 当本地时间（localNow := now，但仍用 loc 构造日界、仍传 tz
// 字符串），窗口会整体前移一天，而只比较 tz 或 UTC 同日期的测试发现不了。
func TestRunnerDerivesTodayFromAccountTimeZoneNotUTC(t *testing.T) {
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	usage := &fakeUsage{latest: now.Add(-time.Minute)}
	st := &fakeRunStore{}
	r, err := NewRunner(RunnerConfig{
		Accounts:  &fakeAccounts{accounts: []store.UpstreamAccount{{ID: 1, Status: 1, Timezone: "Asia/Shanghai"}}},
		Snapshots: &fakeSnapshots{},
		Usage:     usage,
		Store:     st,
		Settings: &fakeSettings{s: Settings{
			Enabled: true, LookbackDays: 3, TokenBP: 100,
			MinDeltaTokens: 1000, AmountToleranceMinor: 100, QuotaPerUnit: 100,
		}},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := r.Run(context.Background(), "manual", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := st.runs[0]
	if got := run.WindowStart.In(loc).Format("2006-01-02"); got != "2026-10-22" {
		t.Fatalf("window_start = %s, want 2026-10-22 (today is 10-25 in Shanghai, not 10-24 UTC)", got)
	}
	if got := run.WindowEnd.In(loc).Format("2006-01-02"); got != "2026-10-25" {
		t.Fatalf("window_end = %s, want 2026-10-25", got)
	}
	if usage.gotTZ != "Asia/Shanghai" {
		t.Fatalf("aggregate tz = %q, want Asia/Shanghai", usage.gotTZ)
	}
	wantStart := time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC) // 2026-10-22 00:00 +08
	wantEnd := time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC)   // 2026-10-26 00:00 +08
	if !usage.gotStart.Equal(wantStart) || !usage.gotEnd.Equal(wantEnd) {
		t.Fatalf("aggregate window = [%s, %s), want [%s, %s)",
			usage.gotStart, usage.gotEnd, wantStart, wantEnd)
	}
	if got := usage.gotEnd.Sub(usage.gotStart); got != 96*time.Hour {
		t.Fatalf("window length = %s, want 96h (lookback 3 = today + 3 prior days)", got)
	}
}

// 原始键解码可能给出 0/负数；必须夹到注册表默认值，否则 start==end 会撞
// upstream_reconciliation_runs_window_chk，连一条可诊断的 run 都留不下。
func TestRunnerClampsInvalidLookbackDays(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	for _, lookback := range []int{0, -5} {
		r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
		r.settings.(*fakeSettings).s.LookbackDays = lookback
		if _, err := r.Run(context.Background(), "manual", nil); err != nil {
			t.Fatalf("lookback %d: Run: %v", lookback, err)
		}
		if len(st.runs) != 1 {
			t.Fatalf("lookback %d: runs = %d, want 1", lookback, len(st.runs))
		}
		run := st.runs[0]
		if run.Status != "succeeded" {
			t.Fatalf("lookback %d: status = %s, want succeeded", lookback, run.Status)
		}
		if got := run.WindowStart.In(loc).Format("2006-01-02"); got != "2026-10-21" {
			t.Fatalf("lookback %d: window_start = %s, want the default 3-day lookback (2026-10-21)", lookback, got)
		}
		if got := run.WindowEnd.In(loc).Format("2006-01-02"); got != "2026-10-24" {
			t.Fatalf("lookback %d: window_end = %s, want 2026-10-24", lookback, got)
		}
	}
}

// 账号时区必须原样传给日志聚合，且日界按账号时区换算：2026-11-01 是纽约
// 夏令时结束日（25 小时），lookback=1 的窗口应为 24h + 25h = 49h。
func TestRunnerUsesAccountTimeZoneAcrossDSTFallback(t *testing.T) {
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC) // 15:00 EST，本地日 11-01
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	usage := &fakeUsage{latest: now.Add(-time.Minute)}
	st := &fakeRunStore{}
	r, err := NewRunner(RunnerConfig{
		Accounts:  &fakeAccounts{accounts: []store.UpstreamAccount{{ID: 1, Status: 1, Timezone: "America/New_York"}}},
		Snapshots: &fakeSnapshots{},
		Usage:     usage,
		Store:     st,
		Settings: &fakeSettings{s: Settings{
			Enabled: true, LookbackDays: 1, TokenBP: 100,
			MinDeltaTokens: 1000, AmountToleranceMinor: 100, QuotaPerUnit: 100,
		}},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := r.Run(context.Background(), "manual", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if usage.gotTZ != "America/New_York" {
		t.Fatalf("aggregate tz = %q, want America/New_York", usage.gotTZ)
	}
	wantStart := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC) // 2026-10-31 00:00 EDT
	wantEnd := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)    // 2026-11-02 00:00 EST
	if !usage.gotStart.Equal(wantStart) || !usage.gotEnd.Equal(wantEnd) {
		t.Fatalf("aggregate window = [%s, %s), want [%s, %s)",
			usage.gotStart, usage.gotEnd, wantStart, wantEnd)
	}
	if got := usage.gotEnd.Sub(usage.gotStart); got != 49*time.Hour {
		t.Fatalf("window length = %s, want 49h (24h + 25h DST day)", got)
	}

	run := st.runs[0]
	if want := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC); !run.WindowStart.Equal(want) {
		t.Fatalf("run window_start = %s, want %s", run.WindowStart, want)
	}
	if want := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC); !run.WindowEnd.Equal(want) {
		t.Fatalf("run window_end = %s, want %s", run.WindowEnd, want)
	}
	if got := run.WindowStart.In(loc).Format("2006-01-02"); got != "2026-10-31" {
		t.Fatalf("run window_start local date = %s, want 2026-10-31", got)
	}
	if got := run.WindowEnd.In(loc).Format("2006-01-02"); got != "2026-11-01" {
		t.Fatalf("run window_end local date = %s, want 2026-11-01", got)
	}
}

// 一个账号取数失败只能让该账号的 run failed；其它账号必须照常结束。
func TestRunnerIsolatesPerAccountFailures(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	usage := &fakeUsage{
		rows:         []store.LocalUsage{{UsageDate: day, Model: "A", CostCredits: 100}},
		latest:       now.Add(-time.Minute),
		errAccount:   1,
		aggregateErr: errors.New("logs db down"),
	}
	st := &fakeRunStore{}
	r, err := NewRunner(RunnerConfig{
		Accounts: &fakeAccounts{accounts: []store.UpstreamAccount{
			{ID: 1, Status: 1, Timezone: "UTC"},
			{ID: 2, Status: 1, Timezone: "UTC"},
		}},
		Snapshots: &fakeSnapshots{},
		Usage:     usage,
		Store:     st,
		Settings: &fakeSettings{s: Settings{
			Enabled: true, LookbackDays: 3, TokenBP: 100,
			MinDeltaTokens: 1000, AmountToleranceMinor: 100, QuotaPerUnit: 100,
		}},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	out, err := r.Run(context.Background(), "manual", nil)
	if err != nil {
		t.Fatalf("Run must not fail globally when one account fails: %v", err)
	}
	if len(st.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(st.runs))
	}
	if st.runs[0].Status != "failed" || st.runs[0].Error == "" {
		t.Fatalf("account 1 run = %+v, want failed with an error", st.runs[0])
	}
	if !strings.Contains(st.runs[0].Error, "logs db down") {
		t.Fatalf("account 1 error = %q, want it to carry the cause", st.runs[0].Error)
	}
	if st.runs[1].Status != "succeeded" {
		t.Fatalf("account 2 run = %+v, want succeeded", st.runs[1])
	}
	if len(st.items) != 1 || len(st.items[0]) != 1 || st.items[0][0].AccountID != 2 {
		t.Fatalf("items = %+v, want exactly account 2's batch", st.items)
	}
	if out.Status != "partial" {
		t.Fatalf("outcome status = %s, want partial", out.Status)
	}
}

// InsertItems 失败时 run 仍必须被收尾为 failed 并带上原因。
func TestRunnerFinishesRunFailedWhenItemInsertFails(t *testing.T) {
	r, st := runnerFixture(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC))
	st.itemsErr = errors.New("duplicate reconciliation item")
	if _, err := r.Run(context.Background(), "manual", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.runs[0].Status != "failed" {
		t.Fatalf("status = %s, want failed", st.runs[0].Status)
	}
	if !strings.Contains(st.runs[0].Error, "insert reconciliation items") {
		t.Fatalf("error = %q, want it to name the item insert", st.runs[0].Error)
	}
	if len(st.items) != 0 {
		t.Fatalf("item batches = %d, want 0 after a failed insert", len(st.items))
	}
}

// 手动触发的 ctx 带 timeout：取数因超时失败时，收尾写库若继续用同一个
// cancelled ctx，FinishRun 必然失败，run 会永远停在 running。收尾必须换一个
// 与调用方取消解耦、但仍然带 deadline 的上下文。
func TestRunnerFinishesRunWithLiveContextAfterCallerCancelled(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	usage := &fakeUsage{
		latest:          now.Add(-time.Minute),
		errAccount:      1,
		aggregateErr:    errors.New("logs db down"),
		beforeAggregate: cancel, // 模拟 timeout：取数期间调用方 ctx 被取消
	}
	st := &fakeRunStore{}
	r, err := NewRunner(RunnerConfig{
		Accounts:  &fakeAccounts{accounts: []store.UpstreamAccount{{ID: 1, Status: 1, Timezone: "UTC"}}},
		Snapshots: &fakeSnapshots{},
		Usage:     usage,
		Store:     st,
		Settings: &fakeSettings{s: Settings{
			Enabled: true, LookbackDays: 3, TokenBP: 100,
			MinDeltaTokens: 1000, AmountToleranceMinor: 100, QuotaPerUnit: 100,
		}},
		Clock:  func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	out, err := r.Run(ctx, "manual", nil)
	if err != nil {
		t.Fatalf("Run must not surface the finish failure as a global error: %v", err)
	}
	if st.finishCalls != 1 {
		t.Fatalf("FinishRun calls = %d, want 1", st.finishCalls)
	}
	// (i) 收尾上下文必须是活的（未被调用方取消），但仍然带 Done。
	if st.finishCtxErr != nil {
		t.Fatalf("FinishRun ctx.Err() = %v, want nil（收尾不得复用已取消的 ctx）", st.finishCtxErr)
	}
	if !st.finishCtxHasDone {
		t.Fatal("FinishRun ctx.Done() = nil, want a deadline-bearing context")
	}
	// (ii) 状态必须是 failed。
	if st.finishStatus != "failed" || st.runs[0].Status != "failed" {
		t.Fatalf("finished status = %q / run status = %q, want failed", st.finishStatus, st.runs[0].Status)
	}
	// (iii) run 自己记录的是账号失败，不是收尾错误。
	if !strings.Contains(st.runs[0].Error, "logs db down") {
		t.Fatalf("run error = %q, want the account failure", st.runs[0].Error)
	}
	if strings.Contains(st.runs[0].Error, "context canceled") {
		t.Fatalf("run error = %q, must not leak the finish error", st.runs[0].Error)
	}
	if out.Status != "failed" {
		t.Fatalf("outcome status = %s, want failed", out.Status)
	}
}
