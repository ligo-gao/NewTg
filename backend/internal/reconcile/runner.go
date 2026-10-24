package reconcile

// 本文件实现上游用量对账的执行器：按账号聚合本地 usage_logs、读取控制台
// 快照、逐 (日期, 模型) 比较，并把 run 与 items 落库。
//
// 口径（上游用量对账设计 §7.3、§11）：
//   - 一个账号一次运行一条 run；重跑生成新 run，旧 run 不可变；
//   - 日志流滞后超过 15 分钟时 run 标记 partial/log_stream_lag，但 items
//     照常写入——半套账也要可观测，而不是整次丢弃；
//   - 日期早于本地日志保留窗口的键写 local_expired，而不是 missing_local；
//   - 账号级错误只让该账号的 run 变成 failed，继续处理其它账号；
//   - 收入（revenue_minor）是网关侧的量：只要本地聚合存在就必须写入，
//     绝不因为还没有控制台快照就记成 0。

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"newtg/gateway/internal/store"
)

const (
	// DefaultRetention 是 usage_logs 的默认保留窗口（设计 §7.3：分区按
	// 90 天删除；上游快照与 run 永久保留）。
	DefaultRetention = 90 * 24 * time.Hour

	// DefaultLookbackDays / DefaultTokenBP / DefaultMinDeltaTokens /
	// DefaultAmountToleranceMinor / DefaultQuotaPerUnit 与系统设置注册表
	// reconcile.* / billing.quota_per_unit 的出厂默认值保持一致。
	DefaultLookbackDays         = 3
	DefaultTokenBP              = 100
	DefaultMinDeltaTokens       = int64(1000)
	DefaultAmountToleranceMinor = int64(100)
	DefaultQuotaPerUnit         = int64(100)

	// logStreamLagThreshold 是判定 usage_logs 日志流滞后的阈值。
	logStreamLagThreshold = 15 * time.Minute

	// finishTimeout 是收尾写库（FinishRun）的独立上限。run 的取数可能吃到
	// 手动触发的 timeout，但收尾不能跟着一起被取消。
	finishTimeout = 5 * time.Second

	triggerWorker = "worker"
	triggerManual = "manual"

	runStatusRunning   = "running"
	runStatusSucceeded = "succeeded"
	runStatusPartial   = "partial"
	runStatusFailed    = "failed"

	runErrorLogStreamLag = "log_stream_lag"

	outcomeStatusSucceeded = "succeeded"
	outcomeStatusPartial   = "partial"
	outcomeStatusFailed    = "failed"
	outcomeStatusDisabled  = "disabled"
	outcomeStatusSkipped   = "skipped"

	itemStatusOpen = "open"
)

// Settings 是 runner 需要的对账设置快照。
//
// 具体实现由装配层提供（Task 9/10）：读取 settings section "reconcile" 的
// 五个键，并把 quota_per_unit 从 billing.quota_per_unit 解析进来。Retention
// 不是注册表键，而是 RunnerConfig.Retention（缺省 DefaultRetention）。
type Settings struct {
	Enabled              bool
	LookbackDays         int
	TokenBP              int
	MinDeltaTokens       int64
	AmountToleranceMinor int64
	QuotaPerUnit         int64
}

// DefaultSettings 返回与注册表一致的出厂默认值，供装配层在键未落库时使用。
// settings.Service 的 Get 已经会用注册表默认值填充未落库键，因此这里只是
// 第二处可读的文档，不参与运行时判定。
func DefaultSettings() Settings {
	return Settings{
		Enabled:              true,
		LookbackDays:         DefaultLookbackDays,
		TokenBP:              DefaultTokenBP,
		MinDeltaTokens:       DefaultMinDeltaTokens,
		AmountToleranceMinor: DefaultAmountToleranceMinor,
		QuotaPerUnit:         DefaultQuotaPerUnit,
	}
}

// SettingsReader 是对账设置的读取缝。生产实现应由装配层包装
// *settings.Service（Get(ctx, "reconcile")）与 billing.quota_per_unit。
type SettingsReader interface {
	ReconcileSettings(ctx context.Context) (Settings, error)
}

// Store 是 runner 需要的写入口，*store.ReconcileRepo 满足该接口。
type Store interface {
	InsertRun(ctx context.Context, run store.ReconcileRun) (int64, error)
	FinishRun(ctx context.Context, id int64, status, runErr string) error
	InsertItems(ctx context.Context, items []store.ReconcileItem) error
}

// AccountSource 是账号读取缝，*store.UpstreamAccountRepo 满足该接口。
type AccountSource interface {
	ListEnabled(ctx context.Context) ([]store.UpstreamAccount, error)
	Get(ctx context.Context, id int64) (*store.UpstreamAccount, error)
	ChannelIDs(ctx context.Context, accountID int64) ([]int64, error)
}

// SnapshotSource 是快照读取缝，*store.UpstreamSnapshotRepo 满足该接口。
type SnapshotSource interface {
	ListRange(ctx context.Context, accountID int64, from, to time.Time) ([]store.UpstreamSnapshot, error)
}

// UsageSource 是本地日志聚合读取缝，*store.UsageAggregateRepo 满足该接口。
type UsageSource interface {
	Aggregate(ctx context.Context, accountID int64, channelIDs []int64, tz string, start, end time.Time) ([]store.LocalUsage, error)
	LatestCreatedAt(ctx context.Context) (time.Time, error)
}

// RunnerConfig 是 Runner 的装配输入。Clock/Retention/Logger 可省略。
type RunnerConfig struct {
	Accounts  AccountSource
	Snapshots SnapshotSource
	Usage     UsageSource
	Store     Store
	Settings  SettingsReader
	Clock     func() time.Time // nil => time.Now
	Retention time.Duration    // zero => DefaultRetention
	Logger    *slog.Logger     // nil => slog.Default
}

// Runner 执行一次或多次账号对账。
//
// usage/snapshots/settings 刻意保持小写字段名：它们也是测试替换协作者的接缝。
type Runner struct {
	accounts  AccountSource
	snapshots SnapshotSource
	usage     UsageSource
	store     Store
	settings  SettingsReader
	clock     func() time.Time
	retention time.Duration
	log       *slog.Logger
}

// finishCtx 返回不与调用方联动的上下文：手动触发的 ctx 带 timeout，超时
// 正是最需要把 run 从 running 收敛掉的时刻，用同一个 ctx 写库必然失败。
func finishCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
}

// logger 返回诊断 logger（零值回退 slog.Default）。
func (r *Runner) logger() *slog.Logger {
	if r != nil && r.log != nil {
		return r.log
	}
	return slog.Default()
}

// NewRunner 校验必需协作者并应用 Clock/Retention 默认值。
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	switch {
	case cfg.Accounts == nil:
		return nil, errors.New("reconcile: Accounts is required")
	case cfg.Snapshots == nil:
		return nil, errors.New("reconcile: Snapshots is required")
	case cfg.Usage == nil:
		return nil, errors.New("reconcile: Usage is required")
	case cfg.Store == nil:
		return nil, errors.New("reconcile: Store is required")
	case cfg.Settings == nil:
		return nil, errors.New("reconcile: Settings is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	retention := cfg.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		accounts:  cfg.Accounts,
		snapshots: cfg.Snapshots,
		usage:     cfg.Usage,
		store:     cfg.Store,
		settings:  cfg.Settings,
		clock:     clock,
		retention: retention,
		log:       log,
	}, nil
}

// Outcome 是一次 Run 的汇总结果。
type Outcome struct {
	RunIDs []int64
	Items  int
	Alerts int
	Status string
}

// Run 对 accountIDs 指定的账号（空表示全部启用账号）执行对账。
//
// trigger 必须是 "worker" 或 "manual"。reconcile.enabled 只约束自动任务：
// 手动触发是管理员的显式补跑，即使开关关闭也执行。
//
// 账号级失败不会让 Run 返回 error——它已作为 failed run 落库；只有读取
// 设置、枚举账号或非法参数这类全局错误才返回 error。
func (r *Runner) Run(ctx context.Context, trigger string, accountIDs []int64) (Outcome, error) {
	if trigger != triggerWorker && trigger != triggerManual {
		return Outcome{}, fmt.Errorf("reconcile: invalid trigger %q", trigger)
	}
	settings, err := r.settings.ReconcileSettings(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("reconcile: read settings: %w", err)
	}
	if trigger == triggerWorker && !settings.Enabled {
		return Outcome{Status: outcomeStatusDisabled}, nil
	}
	accounts, err := r.resolveAccounts(ctx, accountIDs)
	if err != nil {
		return Outcome{}, err
	}

	out := Outcome{RunIDs: make([]int64, 0, len(accounts))}
	statuses := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.Status != 1 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		runID, status, items, alerts := r.runAccount(ctx, account, trigger, settings)
		if runID != 0 {
			out.RunIDs = append(out.RunIDs, runID)
		}
		out.Items += items
		out.Alerts += alerts
		statuses = append(statuses, status)
	}
	out.Status = summariseOutcome(statuses)
	return out, nil
}

// resolveAccounts 返回本次要处理的账号。accountIDs 为空时取全部启用账号；
// 显式 id 按传入顺序去重，已删除的 id 静默跳过（无法为不存在的账号建 run）。
func (r *Runner) resolveAccounts(ctx context.Context, accountIDs []int64) ([]store.UpstreamAccount, error) {
	if len(accountIDs) == 0 {
		accounts, err := r.accounts.ListEnabled(ctx)
		if err != nil {
			return nil, fmt.Errorf("reconcile: list enabled upstream accounts: %w", err)
		}
		return accounts, nil
	}

	out := make([]store.UpstreamAccount, 0, len(accountIDs))
	seen := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		account, err := r.accounts.Get(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrUpstreamAccountNotFound) {
				continue
			}
			return nil, fmt.Errorf("reconcile: get upstream account %d: %w", id, err)
		}
		if account == nil {
			continue
		}
		out = append(out, *account)
	}
	return out, nil
}

// runAccount 处理单个账号，返回 run id、最终状态、写入条目数与 alert 数。
//
// run 行在真正取数之前插入（status=running），这样任何后续账号级错误都能
// 通过 FinishRun(failed) 留下可审计的记录。
func (r *Runner) runAccount(ctx context.Context, account store.UpstreamAccount, trigger string, settings Settings) (int64, string, int, int) {
	now := r.clock().UTC()
	// 空时区在领域层等价于 UTC；但把空串直接交给 Aggregate 的
	// `AT TIME ZONE $4` 会被 PostgreSQL 拒绝，所以先归一成 "UTC"。
	timezone := strings.TrimSpace(account.Timezone)
	loc, tzErr := ParseAccountTimeZone(timezone)
	if tzErr != nil {
		// 时区非法时仍要留下 failed run；用 UTC 兜底只为让 window 两列满足
		// DDL（window_end > window_start），绝不用该窗口取数。
		loc = time.UTC
	}
	if timezone == "" {
		timezone = "UTC"
	}
	// 原始键解码可能给出 0/负数。若不夹到默认值，log 窗口会退化成
	// startDate == endDate，撞上 upstream_reconciliation_runs_window_chk，
	// InsertRun 直接失败且无处记录原因。
	if settings.LookbackDays < 1 {
		settings.LookbackDays = DefaultLookbackDays
	}
	localNow := now.In(loc)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	startDate := today.AddDate(0, 0, -settings.LookbackDays)
	// 设计 §7.3 的伪代码是 end = today(account.timezone) - 1 day（今天仍在累积，
	// 有意停在昨天）；本分支把今天也算进窗口，以覆盖验收夹具的时区切日边界。
	// 代价：进行中的本地日会为每个模型产出一条 missing_snapshot，直到录入快照；
	// 日终前录入的快照会与部分数据比较。
	endDate := today
	start, _ := DayWindow(startDate, loc)
	_, end := DayWindow(endDate, loc)

	runID, err := r.store.InsertRun(ctx, store.ReconcileRun{
		AccountID:            account.ID,
		WindowStart:          startDate,
		WindowEnd:            endDate,
		TriggeredBy:          trigger,
		Status:               runStatusRunning,
		TokenToleranceBP:     settings.TokenBP,
		AmountToleranceMinor: settings.AmountToleranceMinor,
		QuotaPerUnit:         settings.QuotaPerUnit,
		Timezone:             timezone,
		StartedAt:            now,
	})
	if err != nil {
		return 0, runStatusFailed, 0, 0
	}

	fail := func(runErr error) (int64, string, int, int) {
		finish, cancel := finishCtx(ctx)
		defer cancel()
		if err := r.store.FinishRun(finish, runID, runStatusFailed, runErr.Error()); err != nil {
			// 收尾失败是诊断，不是第二个账号级失败：run 的最终状态仍按
			// runErr 汇报，但必须留痕，否则超时的 run 会永远停在 running。
			r.logger().Error("reconcile: finish run failed",
				"run_id", runID, "status", runStatusFailed, "error", err)
		}
		return runID, runStatusFailed, 0, 0
	}
	if tzErr != nil {
		return fail(tzErr)
	}

	status, runErr := runStatusSucceeded, ""
	latest, err := r.usage.LatestCreatedAt(ctx)
	if err != nil {
		return fail(fmt.Errorf("latest usage log time: %w", err))
	}
	if now.Sub(latest) > logStreamLagThreshold {
		status, runErr = runStatusPartial, runErrorLogStreamLag
	}

	channelIDs, err := r.accounts.ChannelIDs(ctx, account.ID)
	if err != nil {
		return fail(fmt.Errorf("channel ids: %w", err))
	}
	locals, err := r.usage.Aggregate(ctx, account.ID, channelIDs, timezone, start, end)
	if err != nil {
		return fail(fmt.Errorf("aggregate local usage: %w", err))
	}
	snapshots, err := r.snapshots.ListRange(ctx, account.ID, startDate, endDate)
	if err != nil {
		return fail(fmt.Errorf("list upstream snapshots: %w", err))
	}

	items := r.buildItems(runID, account.ID, locals, snapshots, settings, now)
	if len(items) > 0 {
		if err := r.store.InsertItems(ctx, items); err != nil {
			return fail(fmt.Errorf("insert reconciliation items: %w", err))
		}
	}
	finish, cancel := finishCtx(ctx)
	defer cancel()
	if err := r.store.FinishRun(finish, runID, status, runErr); err != nil {
		r.logger().Error("reconcile: finish run failed",
			"run_id", runID, "status", status, "error", err)
		return runID, runStatusFailed, len(items), countAlerts(items)
	}
	return runID, status, len(items), countAlerts(items)
}

// buildItems 取本地聚合与快照的并集，按键（日期升序、模型升序）逐条比较。
func (r *Runner) buildItems(runID, accountID int64, locals []store.LocalUsage, snapshots []store.UpstreamSnapshot, settings Settings, now time.Time) []store.ReconcileItem {
	localIndex := AggregateLocal(locals)
	snapshotIndex := make(map[Key]store.UpstreamSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		snapshotIndex[keyFor(snapshot.UsageDate, snapshot.UpstreamModel)] = snapshot
	}

	keys := make([]Key, 0, len(localIndex)+len(snapshotIndex))
	for key := range localIndex {
		keys = append(keys, key)
	}
	for key := range snapshotIndex {
		if _, exists := localIndex[key]; !exists {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if c := a.UsageDate.Compare(b.UsageDate); c != 0 {
			return c
		}
		return cmp.Compare(a.UpstreamModel, b.UpstreamModel)
	})

	cutoff := now.Add(-r.retention)
	items := make([]store.ReconcileItem, 0, len(keys))
	for _, key := range keys {
		var local *LocalAggregate
		if aggregate, ok := localIndex[key]; ok {
			aggregateCopy := aggregate
			local = &aggregateCopy
		}
		var snapshot *store.UpstreamSnapshot
		if row, ok := snapshotIndex[key]; ok {
			rowCopy := row
			snapshot = &rowCopy
		}
		items = append(items, buildItem(runID, accountID, key, local, snapshot, settings, cutoff))
	}
	return items
}

// buildItem 把一条键的两侧数据转成 items 行。
//
// 当 usageDate 早于保留窗口时不做比较，直接写 local_expired（上游快照比
// 本地日志活得更久，lookback 超过保留期仍然有意义）。存在本地聚合时无论
// 有没有快照都写入收入。
func buildItem(runID, accountID int64, key Key, local *LocalAggregate, snapshot *store.UpstreamSnapshot, settings Settings, cutoff time.Time) store.ReconcileItem {
	item := store.ReconcileItem{
		RunID:         runID,
		AccountID:     accountID,
		UsageDate:     key.UsageDate,
		UpstreamModel: key.UpstreamModel,
		Status:        itemStatusOpen,
	}
	if local != nil {
		item.LocalRequests = local.Requests
		item.LocalEstimatedRequests = local.EstimatedRequests
		item.LocalRefundedRequests = local.RefundedRequests
		item.LocalCacheHitTokens = local.CacheHitTokens
		item.LocalCacheMissTokens = local.CacheMissTokens
		item.LocalOutputTokens = local.OutputTokens
		item.LocalWalletCredits = local.WalletCredits
		item.LocalSubscriptionCredits = local.SubscriptionCredits
		item.LocalCostCredits = local.CostCredits
		item.LocalUnknownRequests = local.UnknownRequests
		item.LocalMultiAttemptRequests = local.MultiAttemptRequests
	}
	if snapshot != nil {
		item.SnapshotCacheHitTokens = snapshot.CacheHitTokens
		item.SnapshotCacheMissTokens = snapshot.CacheMissTokens
		item.SnapshotCacheWriteTokens = snapshot.CacheWriteTokens
		item.SnapshotOutputTokens = snapshot.OutputTokens
		item.SnapshotRequests = snapshot.Requests
		item.SnapshotAmountMinor = snapshot.AmountMinor
		item.UpstreamCostMinor = snapshot.AmountMinor
	}

	if key.UsageDate.Before(cutoff) {
		item.Severity = string(SeverityLocalExpired)
		if local != nil {
			revenue := CreditsToMinor(local.CostCredits, settings.QuotaPerUnit)
			item.RevenueMinor = &revenue
		}
	} else {
		comparison := Compare(Input{
			UsageDate:     key.UsageDate,
			UpstreamModel: key.UpstreamModel,
			Local:         local,
			Snapshot:      snapshotInput(snapshot),
			Tolerances: Tolerances{
				TokenBP:        settings.TokenBP,
				MinDeltaTokens: settings.MinDeltaTokens,
				AmountMinor:    settings.AmountToleranceMinor,
			},
			QuotaPerUnit: settings.QuotaPerUnit,
		})
		item.Severity = string(comparison.Severity)
		item.SnapshotPartial = comparison.SnapshotPartial
		item.DeltaCacheHitTokens = comparison.DeltaCacheHitTokens
		item.DeltaCacheMissTokens = comparison.DeltaCacheMissTokens
		item.DeltaOutputTokens = comparison.DeltaOutputTokens
		item.DeltaTokenBP = comparison.DeltaTokenBP
		if local != nil {
			// Compare 在 missing_snapshot 路径同样返回网关侧收入；这里只在
			// 本地聚合存在时落库，missing_local 时收入未知，保持 NULL。
			revenue := comparison.RevenueMinor
			item.RevenueMinor = &revenue
		}
	}

	if item.RevenueMinor != nil && item.UpstreamCostMinor != nil {
		margin := *item.RevenueMinor - *item.UpstreamCostMinor
		item.MarginMinor = &margin
		if bp, ok := MarginBP(margin, *item.RevenueMinor); ok {
			item.MarginBP = &bp
		}
	}
	return item
}

// snapshotInput 把仓储行转成领域层的快照视图；nil 表示没有快照。
func snapshotInput(snapshot *store.UpstreamSnapshot) *Snapshot {
	if snapshot == nil {
		return nil
	}
	return &Snapshot{
		CacheHitTokens:   snapshot.CacheHitTokens,
		CacheMissTokens:  snapshot.CacheMissTokens,
		CacheWriteTokens: snapshot.CacheWriteTokens,
		OutputTokens:     snapshot.OutputTokens,
		Requests:         snapshot.Requests,
		AmountMinor:      snapshot.AmountMinor,
	}
}

// countAlerts 统计一批条目里的 alert 数。
func countAlerts(items []store.ReconcileItem) int {
	n := 0
	for _, item := range items {
		if item.Severity == string(SeverityAlert) {
			n++
		}
	}
	return n
}

// summariseOutcome 把各账号 run 的状态汇总成一次 Run 的状态：
// 全部成功 → succeeded；有失败或 partial → partial（只要还有 run 成功）；
// 全部失败 → failed；没有任何可处理的账号 → skipped。
func summariseOutcome(statuses []string) string {
	if len(statuses) == 0 {
		return outcomeStatusSkipped
	}
	var succeeded, partial, failed bool
	for _, status := range statuses {
		switch status {
		case runStatusSucceeded:
			succeeded = true
		case runStatusPartial:
			partial = true
		case runStatusFailed:
			failed = true
		}
	}
	switch {
	case failed && !succeeded && !partial:
		return outcomeStatusFailed
	case failed || partial:
		return outcomeStatusPartial
	default:
		return outcomeStatusSucceeded
	}
}
