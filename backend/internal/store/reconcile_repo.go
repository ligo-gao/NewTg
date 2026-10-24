package store

// 本文件是 upstream_reconciliation_runs / upstream_reconciliation_items 的
// SQL 仓储（上游用量对账设计 §4.5、§7.3、§8）。
//
// 口径：
//   - runs 不可变：一次对账一条 run，重跑生成新 run，旧 run 保留用于审计；
//   - items 的 (run_id, account_id, usage_date, upstream_model) 唯一，
//     重复插入必须报错而不是静默覆盖（那意味着 runner 的并集逻辑有 bug）；
//   - 金额列一律整数最小单位（分）；可空列用 *int64 表达「不可计算」，
//     与真实的 0 区分；
//   - 日期列以 ISO date 字面值绑定，避免 timestamptz 会话时区截断。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	// ErrRunNotFound 表示对账 run 不存在（例如 FinishRun 的目标已被删除）。
	ErrRunNotFound = errors.New("store: reconciliation run not found")
	// ErrItemNotFound 表示对账明细不存在；HTTP 层映射为 404。
	ErrItemNotFound = errors.New("store: reconciliation item not found")
	// ErrDuplicateItem 表示同一 run 内出现重复的 (account, date, model) 键
	// （pgcode 23505）。runner 必须保证并集去重，重复即程序错误。
	ErrDuplicateItem = errors.New("store: duplicate reconciliation item")
)

// ReconcileRun 是 upstream_reconciliation_runs 的一行。
// 容差与 quota_per_unit 在开跑时快照进来，之后修改设置不影响历史判定。
type ReconcileRun struct {
	ID                   int64
	AccountID            int64
	WindowStart          time.Time
	WindowEnd            time.Time
	TriggeredBy          string
	Status               string
	TokenToleranceBP     int
	AmountToleranceMinor int64
	QuotaPerUnit         int64
	Timezone             string
	StartedAt            time.Time
	FinishedAt           *time.Time
	Error                string
}

// ReconcileItem 是 upstream_reconciliation_items 的一行。
//
// 字段与迁移 20261028090300 的列一一对应：local_* 为非空计数（缺省 0），
// snapshot_*/delta_*/money 为指针（nil = 上游未报告或无法计算）。
type ReconcileItem struct {
	ID            int64
	RunID         int64
	AccountID     int64
	UsageDate     time.Time
	UpstreamModel string

	LocalRequests             int64
	LocalEstimatedRequests    int64
	LocalRefundedRequests     int64
	LocalCacheHitTokens       int64
	LocalCacheMissTokens      int64
	LocalOutputTokens         int64
	LocalWalletCredits        int64
	LocalSubscriptionCredits  int64
	LocalCostCredits          int64
	LocalUnknownRequests      int64
	LocalMultiAttemptRequests int64

	SnapshotCacheHitTokens   *int64
	SnapshotCacheMissTokens  *int64
	SnapshotCacheWriteTokens *int64
	SnapshotOutputTokens     *int64
	SnapshotRequests         *int64
	SnapshotAmountMinor      *int64

	DeltaCacheHitTokens  *int64
	DeltaCacheMissTokens *int64
	DeltaOutputTokens    *int64
	DeltaTokenBP         *int64

	SnapshotPartial bool

	RevenueMinor      *int64
	UpstreamCostMinor *int64
	MarginMinor       *int64
	MarginBP          *int64

	Severity  string
	Status    string
	Note      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SummaryDay 是按天汇总的对账概览（概览 tab 的 days 数组）。
// 金额与 delta_token_bp 可空：当天所有 item 都没有对应维度时保持 nil。
type SummaryDay struct {
	UsageDate         time.Time
	ItemCount         int64
	AlertCount        int64
	WarnCount         int64
	DeltaTokenBP      *int64
	RevenueMinor      *int64
	UpstreamCostMinor *int64
	MarginMinor       *int64
}

// ReconcileRepo 是对账 run/明细的 SQL 仓储。
type ReconcileRepo struct{ db *sql.DB }

// NewReconcileRepo 构造仓储；db 由 platform.OpenDB 提供（pgx stdlib 驱动）。
func NewReconcileRepo(db *sql.DB) *ReconcileRepo { return &ReconcileRepo{db: db} }

const reconcileRunColumns = `id, account_id, window_start, window_end, triggered_by, status,
	token_tolerance_bp, amount_tolerance_minor, quota_per_unit, timezone,
	started_at, finished_at, error`

const reconcileItemColumns = `id, run_id, account_id, usage_date, upstream_model,
	local_requests, local_estimated_requests, local_refunded_requests,
	local_cache_hit_tokens, local_cache_miss_tokens, local_output_tokens,
	local_wallet_credits, local_subscription_credits, local_cost_credits,
	local_unknown_requests, local_multi_attempt_requests,
	snapshot_cache_hit_tokens, snapshot_cache_miss_tokens, snapshot_cache_write_tokens,
	snapshot_output_tokens, snapshot_requests, snapshot_amount_minor,
	delta_cache_hit_tokens, delta_cache_miss_tokens, delta_output_tokens, delta_token_bp,
	snapshot_partial, revenue_minor, upstream_cost_minor, margin_minor, margin_bp,
	severity, status, note, created_at, updated_at`

// reconcileItemInsertColumns 是 InsertItems 的 INSERT 列清单，同时也是每行
// 参数个数的唯一来源（perRow = len(...)）。
//
// 顺序必须与 InsertItems 里 args 的追加顺序逐位一致：第 1 列对第 1 个参数，
// 以此类推（0 基下标见 reconcileItemUsageDateIndex 的注释）。此前「手写
// const perRow = 33」与这份手写列清单各自维护，才让 ::date 的下标错位溜了过去；
// 现在列数不再手写。
var reconcileItemInsertColumns = []string{
	"run_id", "account_id", "usage_date", "upstream_model",
	"local_requests", "local_estimated_requests", "local_refunded_requests",
	"local_cache_hit_tokens", "local_cache_miss_tokens", "local_output_tokens",
	"local_wallet_credits", "local_subscription_credits", "local_cost_credits",
	"local_unknown_requests", "local_multi_attempt_requests",
	"snapshot_cache_hit_tokens", "snapshot_cache_miss_tokens", "snapshot_cache_write_tokens",
	"snapshot_output_tokens", "snapshot_requests", "snapshot_amount_minor",
	"delta_cache_hit_tokens", "delta_cache_miss_tokens", "delta_output_tokens", "delta_token_bp",
	"snapshot_partial", "revenue_minor", "upstream_cost_minor", "margin_minor", "margin_bp",
	"severity", "status", "note",
}

// reconcileItemUsageDateIndex 是 usage_date 在列清单/参数清单里的 0 基下标：
// 第 1 个参数是 run_id，第 2 个是 account_id，第 3 个才是 usage_date（下标 2）。
// 下标由列清单推导，避免再次手写成 3（把 upstream_model 当成日期）。
var reconcileItemUsageDateIndex = func() int {
	i := slices.Index(reconcileItemInsertColumns, "usage_date")
	if i < 0 {
		panic("store: usage_date missing from reconcileItemInsertColumns")
	}
	return i
}()

// itemInsertRow 生成一条 VALUES 行的占位符模板，例如 base=0 时：
// ($1,$2,$3::date,$4,...)。base 是该行首参数之前已被占用的参数个数，
// 因此第 i 个占位符是 $base+i+1。
//
// 只有 usage_date 需要显式 ::date：日期以 ISO 字面值绑定，避免 pgx 按
// timestamptz 编码后被会话时区截断到相邻日期。
func itemInsertRow(base int) string {
	placeholders := make([]string, len(reconcileItemInsertColumns))
	for i := range placeholders {
		placeholders[i] = fmt.Sprintf("$%d", base+i+1)
	}
	placeholders[reconcileItemUsageDateIndex] = fmt.Sprintf("$%d::date",
		base+reconcileItemUsageDateIndex+1)
	return "(" + strings.Join(placeholders, ",") + ")"
}

// scanReconcileRun 读取一行 run；finished_at 可空。
func scanReconcileRun(row scanRow) (*ReconcileRun, error) {
	var (
		run        ReconcileRun
		windowFrom time.Time
		windowTo   time.Time
		finishedAt sql.NullTime
	)
	err := row.Scan(&run.ID, &run.AccountID, &windowFrom, &windowTo,
		&run.TriggeredBy, &run.Status, &run.TokenToleranceBP, &run.AmountToleranceMinor,
		&run.QuotaPerUnit, &run.Timezone, &run.StartedAt, &finishedAt, &run.Error)
	if err != nil {
		return nil, err
	}
	// date 在 pgx 下以 UTC 零点返回；归一位置便于调用方直接比较。
	run.WindowStart = windowFrom.UTC()
	run.WindowEnd = windowTo.UTC()
	run.StartedAt = run.StartedAt.UTC()
	if finishedAt.Valid {
		t := finishedAt.Time.UTC()
		run.FinishedAt = &t
	}
	return &run, nil
}

// scanReconcileItem 读取一行明细；可空计数/金额统一走 sql.NullInt64。
func scanReconcileItem(row scanRow) (*ReconcileItem, error) {
	var (
		it        ReconcileItem
		usageDate time.Time
		// 快照侧
		snapHit, snapMiss, snapWrite sql.NullInt64
		snapOut, snapReq, snapAmount sql.NullInt64
		// 差异侧
		deltaHit, deltaMiss, deltaOut, deltaBP sql.NullInt64
		// 金额侧
		revenue, cost, margin, marginBP sql.NullInt64
	)
	err := row.Scan(&it.ID, &it.RunID, &it.AccountID, &usageDate, &it.UpstreamModel,
		&it.LocalRequests, &it.LocalEstimatedRequests, &it.LocalRefundedRequests,
		&it.LocalCacheHitTokens, &it.LocalCacheMissTokens, &it.LocalOutputTokens,
		&it.LocalWalletCredits, &it.LocalSubscriptionCredits, &it.LocalCostCredits,
		&it.LocalUnknownRequests, &it.LocalMultiAttemptRequests,
		&snapHit, &snapMiss, &snapWrite, &snapOut, &snapReq, &snapAmount,
		&deltaHit, &deltaMiss, &deltaOut, &deltaBP,
		&it.SnapshotPartial, &revenue, &cost, &margin, &marginBP,
		&it.Severity, &it.Status, &it.Note, &it.CreatedAt, &it.UpdatedAt)
	if err != nil {
		return nil, err
	}
	it.UsageDate = usageDate.UTC()
	it.CreatedAt = it.CreatedAt.UTC()
	it.UpdatedAt = it.UpdatedAt.UTC()
	it.SnapshotCacheHitTokens = nullInt64Ptr(snapHit)
	it.SnapshotCacheMissTokens = nullInt64Ptr(snapMiss)
	it.SnapshotCacheWriteTokens = nullInt64Ptr(snapWrite)
	it.SnapshotOutputTokens = nullInt64Ptr(snapOut)
	it.SnapshotRequests = nullInt64Ptr(snapReq)
	it.SnapshotAmountMinor = nullInt64Ptr(snapAmount)
	it.DeltaCacheHitTokens = nullInt64Ptr(deltaHit)
	it.DeltaCacheMissTokens = nullInt64Ptr(deltaMiss)
	it.DeltaOutputTokens = nullInt64Ptr(deltaOut)
	it.DeltaTokenBP = nullInt64Ptr(deltaBP)
	it.RevenueMinor = nullInt64Ptr(revenue)
	it.UpstreamCostMinor = nullInt64Ptr(cost)
	it.MarginMinor = nullInt64Ptr(margin)
	it.MarginBP = nullInt64Ptr(marginBP)
	return &it, nil
}

// InsertRun 插入一条 running 状态的 run 并返回 id。
// Status 为空时落 running；StartedAt 为零时由数据库 now() 填充。
func (r *ReconcileRepo) InsertRun(ctx context.Context, run ReconcileRun) (int64, error) {
	status := run.Status
	if status == "" {
		status = "running"
	}
	var startedAt any
	if !run.StartedAt.IsZero() {
		startedAt = run.StartedAt.UTC()
	}

	var id int64
	err := r.db.QueryRowContext(ctx, `INSERT INTO upstream_reconciliation_runs
		(account_id, window_start, window_end, triggered_by, status,
		 token_tolerance_bp, amount_tolerance_minor, quota_per_unit, timezone, started_at)
		VALUES ($1,$2::date,$3::date,$4,$5,$6,$7,$8,$9,COALESCE($10, now()))
		RETURNING id`,
		run.AccountID, formatSQLDate(run.WindowStart), formatSQLDate(run.WindowEnd),
		run.TriggeredBy, status, run.TokenToleranceBP, run.AmountToleranceMinor,
		run.QuotaPerUnit, run.Timezone, startedAt).Scan(&id)
	if isForeignKeyViolation(err) {
		return 0, ErrUpstreamAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: insert reconciliation run: %w", err)
	}
	return id, nil
}

// FinishRun 结束一次 run：写入最终状态、错误文本与 finished_at。
// 目标不存在返回 ErrRunNotFound。
func (r *ReconcileRepo) FinishRun(ctx context.Context, id int64, status string, runErr string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE upstream_reconciliation_runs
		SET status = $2, error = $3, finished_at = now()
		WHERE id = $1`, id, status, runErr)
	if err != nil {
		return fmt.Errorf("store: finish reconciliation run %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish reconciliation run %d rows: %w", id, err)
	}
	if n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// InsertItems 在单事务内写入一批明细。
//
// 不做任何去重：同一 run 内重复的 (account, date, model) 由唯一约束拒绝，
// 映射为 ErrDuplicateItem（runner 的并集逻辑必须保证键唯一）。
// run_id 不存在（FK 23503）映射为 ErrRunNotFound。
func (r *ReconcileRepo) InsertItems(ctx context.Context, items []ReconcileItem) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: insert reconciliation items begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	perRow := len(reconcileItemInsertColumns)
	valueRows := make([]string, 0, len(items))
	args := make([]any, 0, len(items)*perRow)
	for _, it := range items {
		status := it.Status
		if status == "" {
			status = "open"
		}
		base := len(args)
		valueRows = append(valueRows, itemInsertRow(base))
		// 参数顺序必须与 reconcileItemInsertColumns 逐位对应（0 基）：
		//   0=run_id, 1=account_id, 2=usage_date, 3=upstream_model,
		//   4..14=local_*, 15..20=snapshot_*, 21..24=delta_*, 25=snapshot_partial,
		//   26..29=金额/利润率, 30=severity, 31=status, 32=note。
		// 新增/调整列时两处一起改，并由 reconcile_repo_test.go 断言列数一致。
		args = append(args,
			it.RunID, it.AccountID, formatSQLDate(it.UsageDate), it.UpstreamModel,
			it.LocalRequests, it.LocalEstimatedRequests, it.LocalRefundedRequests,
			it.LocalCacheHitTokens, it.LocalCacheMissTokens, it.LocalOutputTokens,
			it.LocalWalletCredits, it.LocalSubscriptionCredits, it.LocalCostCredits,
			it.LocalUnknownRequests, it.LocalMultiAttemptRequests,
			nullableInt64(it.SnapshotCacheHitTokens), nullableInt64(it.SnapshotCacheMissTokens),
			nullableInt64(it.SnapshotCacheWriteTokens), nullableInt64(it.SnapshotOutputTokens),
			nullableInt64(it.SnapshotRequests), nullableInt64(it.SnapshotAmountMinor),
			nullableInt64(it.DeltaCacheHitTokens), nullableInt64(it.DeltaCacheMissTokens),
			nullableInt64(it.DeltaOutputTokens), nullableInt64(it.DeltaTokenBP),
			it.SnapshotPartial, nullableInt64(it.RevenueMinor),
			nullableInt64(it.UpstreamCostMinor), nullableInt64(it.MarginMinor),
			nullableInt64(it.MarginBP), it.Severity, status, it.Note)
	}

	query := `INSERT INTO upstream_reconciliation_items (` +
		strings.Join(reconcileItemInsertColumns, ", ") + `)
		VALUES ` + strings.Join(valueRows, ", ")

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateItem
		}
		if isForeignKeyViolation(err) {
			return ErrRunNotFound
		}
		return fmt.Errorf("store: insert reconciliation items: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: insert reconciliation items commit: %w", err)
	}
	return nil
}

// ListRuns 返回 offset 分页的 run 与总数；accountID 为 nil 表示不筛选。
// 按 started_at 降序（最新优先），id 降序作为稳定 tie-breaker。
func (r *ReconcileRepo) ListRuns(ctx context.Context, accountID *int64, page, pageSize int) ([]ReconcileRun, int64, error) {
	page, pageSize = normalizePage(page, pageSize)

	where := ""
	args := make([]any, 0, 1)
	if accountID != nil {
		args = append(args, *accountID)
		where = " WHERE account_id = $1"
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM upstream_reconciliation_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count reconciliation runs: %w", err)
	}

	query := `SELECT ` + reconcileRunColumns + ` FROM upstream_reconciliation_runs` + where +
		fmt.Sprintf(` ORDER BY started_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)

	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list reconciliation runs: %w", err)
	}
	defer rows.Close()

	items := make([]ReconcileRun, 0, pageSize)
	for rows.Next() {
		run, err := scanReconcileRun(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan reconciliation run: %w", err)
		}
		items = append(items, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate reconciliation runs: %w", err)
	}
	return items, total, nil
}

// ListItems 返回某个 run 的明细与总数；severity/status 为空串表示不筛选。
// 按 usage_date 降序、模型升序（同一天内模型名稳定）。
func (r *ReconcileRepo) ListItems(ctx context.Context, runID int64, severity, status string, page, pageSize int) ([]ReconcileItem, int64, error) {
	page, pageSize = normalizePage(page, pageSize)

	clauses := []string{"run_id = $1"}
	args := []any{runID}
	if severity != "" {
		args = append(args, severity)
		clauses = append(clauses, fmt.Sprintf("severity = $%d", len(args)))
	}
	if status != "" {
		args = append(args, status)
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	where := " WHERE " + strings.Join(clauses, " AND ")

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM upstream_reconciliation_items`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count reconciliation items for run %d: %w", runID, err)
	}

	query := `SELECT ` + reconcileItemColumns + ` FROM upstream_reconciliation_items` + where +
		fmt.Sprintf(` ORDER BY usage_date DESC, upstream_model ASC, id ASC LIMIT $%d OFFSET $%d`,
			len(args)+1, len(args)+2)
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)

	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list reconciliation items for run %d: %w", runID, err)
	}
	defer rows.Close()

	items := make([]ReconcileItem, 0, pageSize)
	for rows.Next() {
		it, err := scanReconcileItem(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan reconciliation item: %w", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate reconciliation items for run %d: %w", runID, err)
	}
	return items, total, nil
}

// GetItem 按 id 读取一条明细；不存在返回 ErrItemNotFound。
func (r *ReconcileRepo) GetItem(ctx context.Context, id int64) (*ReconcileItem, error) {
	it, err := scanReconcileItem(r.db.QueryRowContext(ctx,
		`SELECT `+reconcileItemColumns+` FROM upstream_reconciliation_items WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get reconciliation item %d: %w", id, err)
	}
	return it, nil
}

// AckItem 更新一条明细的运营处置状态与备注。
// status 的闭集由数据库 CHECK 兜底；目标不存在返回 ErrItemNotFound。
func (r *ReconcileRepo) AckItem(ctx context.Context, id int64, status, note string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE upstream_reconciliation_items
		SET status = $2, note = $3, updated_at = now()
		WHERE id = $1`, id, status, note)
	if err != nil {
		return fmt.Errorf("store: ack reconciliation item %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: ack reconciliation item %d rows: %w", id, err)
	}
	if n == 0 {
		return ErrItemNotFound
	}
	return nil
}

// summaryQuery 按天汇总一个 run 的明细。
//
// delta_token_bp 用「当日绝对差之和 / 当日快照 token 总量」重新计算，而不是
// 对逐 item 的 bp 取平均：后者会让小样本的 item 与大盘 item 等权。
// 分子用 numeric 运算再 clamp 到 int64，避免 token 基数接近 1e15 时溢出。
const summaryQuery = `
	SELECT usage_date,
	       count(*) AS item_count,
	       count(*) FILTER (WHERE severity = 'alert') AS alert_count,
	       count(*) FILTER (WHERE severity = 'warn') AS warn_count,
	       CASE
	         WHEN SUM(COALESCE(snapshot_cache_hit_tokens, 0)
	                + COALESCE(snapshot_cache_miss_tokens, 0)
	                + COALESCE(snapshot_output_tokens, 0)) > 0
	         THEN LEAST(
	             (COALESCE(SUM(ABS(delta_cache_hit_tokens)), 0)
	              + COALESCE(SUM(ABS(delta_cache_miss_tokens)), 0)
	              + COALESCE(SUM(ABS(delta_output_tokens)), 0))::numeric * 10000
	             / SUM(COALESCE(snapshot_cache_hit_tokens, 0)
	                 + COALESCE(snapshot_cache_miss_tokens, 0)
	                 + COALESCE(snapshot_output_tokens, 0)),
	             9223372036854775807)::bigint
	       END AS delta_token_bp,
	       SUM(revenue_minor)::bigint AS revenue_minor,
	       SUM(upstream_cost_minor)::bigint AS upstream_cost_minor,
	       SUM(margin_minor)::bigint AS margin_minor
	FROM upstream_reconciliation_items
	WHERE run_id = $1
	GROUP BY usage_date
	ORDER BY usage_date ASC`

// Summary 返回一个 run 的按天汇总（概览 tab 的数据源）。
func (r *ReconcileRepo) Summary(ctx context.Context, runID int64) ([]SummaryDay, error) {
	rows, err := r.db.QueryContext(ctx, summaryQuery, runID)
	if err != nil {
		return nil, fmt.Errorf("store: reconciliation summary for run %d: %w", runID, err)
	}
	defer rows.Close()

	days := make([]SummaryDay, 0, 16)
	for rows.Next() {
		var (
			day       SummaryDay
			usageDate time.Time
			deltaBP   sql.NullInt64
			revenue   sql.NullInt64
			cost      sql.NullInt64
			margin    sql.NullInt64
		)
		if err := rows.Scan(&usageDate, &day.ItemCount, &day.AlertCount, &day.WarnCount,
			&deltaBP, &revenue, &cost, &margin); err != nil {
			return nil, fmt.Errorf("store: scan reconciliation summary: %w", err)
		}
		day.UsageDate = usageDate.UTC()
		day.DeltaTokenBP = nullInt64Ptr(deltaBP)
		day.RevenueMinor = nullInt64Ptr(revenue)
		day.UpstreamCostMinor = nullInt64Ptr(cost)
		day.MarginMinor = nullInt64Ptr(margin)
		days = append(days, day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate reconciliation summary for run %d: %w", runID, err)
	}
	return days, nil
}
