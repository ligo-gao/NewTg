package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"newtg/gateway/internal/perfmetrics"
)

// 本文件是 platform-05 §3.5 模型性能采集管道的持久化实现（G-M6C-13）。
//
// 【独立指标库】Repo 连接的是 PERF_METRICS_DSN 指向的**专用 PostgreSQL
// 指标库**，不是主库、也不是日志库（DB_LOGS_DSN）。性能指标是观测事实，
// 不是计费真源；platform-01 §30 明确不落业务库。这里的 PostgreSQL 指标库
// 是 M7 指标库产品（VictoriaMetrics / ClickHouse / Mimir）确定前的过渡
// 实现；perfmetrics.Store 接缝保持不变，产品替换时只需换 Repo。
//
// 【绝对快照】每次 flush 写入的是「该分钟桶到目前为止的累计量」，不是增量。
// 唯一键 (bucket_start, model, node, epoch) + flush_seq 条件 upsert 保证：
//   - 同一快照重复写入（网络重试/停机重放）是 no-op；
//   - 迟到样本让进程内累计量变大，flush_seq 变大后覆盖旧值；
//   - 旧 epoch 的末尾快照与新 epoch 的快照共存，查询时跨 epoch 求和。
//
// 【两层表与保留】model_perf_buckets 是 1 分钟绝对快照；RunRetention 先把
// 闭合小时物化到 model_perf_hourly_buckets，再删除 7 天前的分钟行与超过
// PERF_METRICS_RETENTION_DAYS 的小时行。1h 查询优先读已物化小时行，最近
// 7 天由分钟行在线 rollup（避免物化滞后造成空洞）。

const (
	// modelPerfRetentionLockKey 是跨副本保留清理的 PG 咨询锁键。与迁移锁
	// (727272) 分开，避免 migrate 与 retention 互相阻塞。
	modelPerfRetentionLockKey = 727275
	// modelPerfUpsertBatch 是单条多行 upsert 的快照数上限。
	modelPerfUpsertBatch = 100
)

// modelPerfInsertColumns 是分钟快照写入列（updated_at 由 NOW() 生成）。
var modelPerfInsertColumns = []string{
	"bucket_start", "model", "node", "epoch", "flush_seq",
	"success", "error", "refused",
	"latency_count", "latency_sum_ms",
	"lat_b0", "lat_b1", "lat_b2", "lat_b3", "lat_b4", "lat_b5", "lat_b6", "lat_b7", "lat_b8",
	"ttft_count",
	"ttft_b0", "ttft_b1", "ttft_b2", "ttft_b3", "ttft_b4", "ttft_b5", "ttft_b6", "ttft_b7", "ttft_b8",
	"completion_tokens", "generation_seconds",
}

// modelPerfCounterColumns 是分钟/小时共有的计数与直方图列（不含
// updated_at / observed_seconds / up_seconds）。
var modelPerfCounterColumns = []string{
	"success", "error", "refused",
	"latency_count", "latency_sum_ms",
	"lat_b0", "lat_b1", "lat_b2", "lat_b3", "lat_b4", "lat_b5", "lat_b6", "lat_b7", "lat_b8",
	"ttft_count",
	"ttft_b0", "ttft_b1", "ttft_b2", "ttft_b3", "ttft_b4", "ttft_b5", "ttft_b6", "ttft_b7", "ttft_b8",
	"completion_tokens", "generation_seconds",
}

func quotedCSV(columns []string) string { return strings.Join(columns, ", ") }

func sumCSV(columns []string) string {
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = "COALESCE(SUM(" + column + "), 0)"
	}
	return strings.Join(parts, ", ")
}

// modelPerfMinuteSelect 是分钟表的聚合表达式。直方图是每桶非累计计数，
// 查询只做 SUM；+Inf 由调用方用 count - Σbuckets 推导。
var modelPerfMinuteSelect = sumCSV(modelPerfCounterColumns) + ", COALESCE(MAX(updated_at), 'epoch'::timestamptz)"

// modelPerfHourlySelect 是已物化小时表的聚合表达式。observed/up 已按分钟
// 判定后累加，查询直接 SUM，不重新判定。
var modelPerfHourlySelect = sumCSV(modelPerfCounterColumns) +
	", COALESCE(SUM(observed_seconds), 0), COALESCE(SUM(up_seconds), 0)" +
	", COALESCE(MAX(updated_at), 'epoch'::timestamptz)"

// ModelPerfRepo 是 perfmetrics.Store / perfmetrics.RetentionStore 在独立
// PostgreSQL 指标库上的实现。
type ModelPerfRepo struct{ db *sql.DB }

// NewModelPerfRepo 构造仓储；db 必须是 PERF_METRICS_DSN 打开的专业指标库。
// 传入 nil 只用于单元测试的错误路径断言。
func NewModelPerfRepo(db *sql.DB) *ModelPerfRepo { return &ModelPerfRepo{db: db} }

// UpsertBuckets 批量写入分钟绝对快照；分批事务，避免单条语句参数过多。
func (r *ModelPerfRepo) UpsertBuckets(ctx context.Context, snapshots []perfmetrics.Snapshot) error {
	if r == nil || r.db == nil {
		return errors.New("store: model perf repo has no metrics database")
	}
	if len(snapshots) == 0 {
		return nil
	}
	for start := 0; start < len(snapshots); start += modelPerfUpsertBatch {
		end := start + modelPerfUpsertBatch
		if end > len(snapshots) {
			end = len(snapshots)
		}
		if err := r.upsertBatch(ctx, snapshots[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (r *ModelPerfRepo) upsertBatch(ctx context.Context, snapshots []perfmetrics.Snapshot) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin model perf upsert: %w", err)
	}
	args := make([]any, 0, len(snapshots)*len(modelPerfInsertColumns))
	for _, snap := range snapshots {
		args = append(args, snapshotArgs(snap)...)
	}
	if _, err := tx.ExecContext(ctx, modelPerfUpsertSQL(len(snapshots)), args...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: upsert model perf buckets: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit model perf upsert: %w", err)
	}
	return nil
}

func snapshotArgs(snap perfmetrics.Snapshot) []any {
	args := make([]any, 0, len(modelPerfInsertColumns))
	args = append(args,
		snap.BucketStart.UTC(), snap.Model, snap.Node, snap.Epoch, snap.Seq,
		snap.Success, snap.Error, snap.Refused,
		snap.LatencyCount, snap.LatencySumMS,
	)
	for _, v := range snap.LatencyBuckets {
		args = append(args, v)
	}
	args = append(args, snap.TTFTCount)
	for _, v := range snap.TTFTBuckets {
		args = append(args, v)
	}
	args = append(args, snap.CompletionTokens, snap.GenerationSeconds)
	return args
}

// modelPerfUpsertSQL 生成多行 INSERT ... ON CONFLICT。flush_seq 条件保证
// 「旧序号覆盖新序号」永远不会发生；相同序号重放是 no-op。
func modelPerfUpsertSQL(rows int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO model_perf_buckets (")
	b.WriteString(quotedCSV(modelPerfInsertColumns))
	b.WriteString(") VALUES ")
	width := len(modelPerfInsertColumns)
	for row := 0; row < rows; row++ {
		if row > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		for col := 0; col < width; col++ {
			if col > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "$%d", row*width+col+1)
		}
		b.WriteString(")")
	}
	assignments := make([]string, 0, len(modelPerfInsertColumns)-1)
	for _, column := range modelPerfInsertColumns {
		if column == "bucket_start" || column == "model" || column == "node" || column == "epoch" {
			continue
		}
		assignments = append(assignments, column+" = EXCLUDED."+column)
	}
	assignments = append(assignments, "updated_at = now()")
	b.WriteString(" ON CONFLICT (bucket_start, model, node, epoch) DO UPDATE SET ")
	b.WriteString(strings.Join(assignments, ", "))
	b.WriteString(" WHERE EXCLUDED.flush_seq > model_perf_buckets.flush_seq")
	return b.String()
}

// ModelPerfBuckets 实现 perfmetrics.Source。
//
//   - 1m：读分钟表并在线判定每分钟 up/observed；
//   - 1h：7 天以前读已物化小时表，最近 7 天读分钟表在线 rollup（分钟行在
//     7 天内仍完整），合并后按时序返回。这样既支持 30d 长窗口，也不会因
//     保留物化滞后让最近的桶消失。
func (r *ModelPerfRepo) ModelPerfBuckets(ctx context.Context, start, end time.Time,
	granularity, model string) ([]perfmetrics.Bucket, time.Time, error) {

	if r == nil || r.db == nil {
		return nil, time.Time{}, errors.New("store: model perf repo has no metrics database")
	}
	switch granularity {
	case "1m":
		return r.queryMinuteBuckets(ctx, start, end, model)
	case "1h":
		return r.queryHourlyWithRecentMinutes(ctx, start, end, model)
	default:
		return nil, time.Time{}, fmt.Errorf("store: invalid model perf granularity %q", granularity)
	}
}

func (r *ModelPerfRepo) queryMinuteBuckets(ctx context.Context, start, end time.Time,
	model string) ([]perfmetrics.Bucket, time.Time, error) {

	query, args := modelPerfWindowQuery("model_perf_buckets", modelPerfMinuteSelect, start, end, model)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: query model perf minutes: %w", err)
	}
	defer rows.Close()

	out := make([]perfmetrics.Bucket, 0, 64)
	var updated time.Time
	for rows.Next() {
		bucket, rowUpdated, err := scanMinuteBucket(rows)
		if err != nil {
			return nil, time.Time{}, err
		}
		bucket.ObservedSeconds, bucket.UpSeconds = modelPerfAvailability(bucket)
		if rowUpdated.After(updated) {
			updated = rowUpdated.UTC()
		}
		out = append(out, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: iterate model perf minutes: %w", err)
	}
	return out, updated, nil
}

func (r *ModelPerfRepo) queryHourlyBuckets(ctx context.Context, start, end time.Time,
	model string) ([]perfmetrics.Bucket, time.Time, error) {

	query, args := modelPerfWindowQuery("model_perf_hourly_buckets", modelPerfHourlySelect, start, end, model)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: query model perf hours: %w", err)
	}
	defer rows.Close()

	out := make([]perfmetrics.Bucket, 0, 64)
	var updated time.Time
	for rows.Next() {
		bucket, rowUpdated, err := scanHourlyBucket(rows)
		if err != nil {
			return nil, time.Time{}, err
		}
		if rowUpdated.After(updated) {
			updated = rowUpdated.UTC()
		}
		out = append(out, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: iterate model perf hours: %w", err)
	}
	return out, updated, nil
}

func (r *ModelPerfRepo) queryHourlyWithRecentMinutes(ctx context.Context, start, end time.Time,
	model string) ([]perfmetrics.Bucket, time.Time, error) {

	// 分钟表固定保留 7 天；边界按 UTC 整点对齐，避免同一小时被两张表重复相加。
	minuteCutoff := time.Now().UTC().Add(-perfmetrics.MinuteRetention).Truncate(time.Hour)
	if minuteCutoff.After(end) {
		minuteCutoff = end
	}

	var (
		out     []perfmetrics.Bucket
		updated time.Time
	)
	if start.Before(minuteCutoff) {
		hours, hourUpdated, err := r.queryHourlyBuckets(ctx, start, minuteCutoff, model)
		if err != nil {
			return nil, time.Time{}, err
		}
		out = append(out, hours...)
		if hourUpdated.After(updated) {
			updated = hourUpdated
		}
	}
	minuteStart := minuteCutoff
	if minuteStart.Before(start) {
		minuteStart = start
	}
	if minuteStart.Before(end) {
		minutes, minuteUpdated, err := r.queryMinuteBuckets(ctx, minuteStart, end, model)
		if err != nil {
			return nil, time.Time{}, err
		}
		out = append(out, rollupModelPerfHours(minutes)...)
		if minuteUpdated.After(updated) {
			updated = minuteUpdated
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start.Equal(out[j].Start) {
			return out[i].Model < out[j].Model
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out, updated, nil
}

func modelPerfWindowQuery(table, selectExpr string, start, end time.Time, model string) (string, []any) {
	where := "bucket_start >= $1 AND bucket_start < $2"
	args := []any{start.UTC(), end.UTC()}
	if model != "" {
		where += " AND model = $3"
		args = append(args, model)
	}
	return "SELECT bucket_start, model, " + selectExpr +
		" FROM " + table + " WHERE " + where +
		" GROUP BY bucket_start, model ORDER BY bucket_start, model", args
}

func scanMinuteBucket(rows *sql.Rows) (perfmetrics.Bucket, time.Time, error) {
	var (
		bucket         perfmetrics.Bucket
		latencyBuckets [perfmetrics.HistogramBuckets]int64
		ttftBuckets    [perfmetrics.HistogramBuckets]int64
		rowUpdated     time.Time
	)
	if err := rows.Scan(
		&bucket.Start, &bucket.Model,
		&bucket.Success, &bucket.Error, &bucket.Refused,
		&bucket.LatencyCount, &bucket.LatencySumMS,
		&latencyBuckets[0], &latencyBuckets[1], &latencyBuckets[2],
		&latencyBuckets[3], &latencyBuckets[4], &latencyBuckets[5],
		&latencyBuckets[6], &latencyBuckets[7], &latencyBuckets[8],
		&bucket.TTFTCount,
		&ttftBuckets[0], &ttftBuckets[1], &ttftBuckets[2],
		&ttftBuckets[3], &ttftBuckets[4], &ttftBuckets[5],
		&ttftBuckets[6], &ttftBuckets[7], &ttftBuckets[8],
		&bucket.CompletionTokens, &bucket.GenerationSeconds,
		&rowUpdated,
	); err != nil {
		return perfmetrics.Bucket{}, time.Time{}, fmt.Errorf("store: scan model perf minute: %w", err)
	}
	bucket.Start = bucket.Start.UTC()
	bucket.LatencyBuckets = latencyBuckets
	bucket.TTFTBuckets = ttftBuckets
	return bucket, rowUpdated, nil
}

func scanHourlyBucket(rows *sql.Rows) (perfmetrics.Bucket, time.Time, error) {
	var (
		bucket         perfmetrics.Bucket
		latencyBuckets [perfmetrics.HistogramBuckets]int64
		ttftBuckets    [perfmetrics.HistogramBuckets]int64
		rowUpdated     time.Time
	)
	if err := rows.Scan(
		&bucket.Start, &bucket.Model,
		&bucket.Success, &bucket.Error, &bucket.Refused,
		&bucket.LatencyCount, &bucket.LatencySumMS,
		&latencyBuckets[0], &latencyBuckets[1], &latencyBuckets[2],
		&latencyBuckets[3], &latencyBuckets[4], &latencyBuckets[5],
		&latencyBuckets[6], &latencyBuckets[7], &latencyBuckets[8],
		&bucket.TTFTCount,
		&ttftBuckets[0], &ttftBuckets[1], &ttftBuckets[2],
		&ttftBuckets[3], &ttftBuckets[4], &ttftBuckets[5],
		&ttftBuckets[6], &ttftBuckets[7], &ttftBuckets[8],
		&bucket.CompletionTokens, &bucket.GenerationSeconds,
		&bucket.ObservedSeconds, &bucket.UpSeconds,
		&rowUpdated,
	); err != nil {
		return perfmetrics.Bucket{}, time.Time{}, fmt.Errorf("store: scan model perf hour: %w", err)
	}
	bucket.Start = bucket.Start.UTC()
	bucket.LatencyBuckets = latencyBuckets
	bucket.TTFTBuckets = ttftBuckets
	return bucket, rowUpdated, nil
}

// modelPerfAvailability 在**求和后的分钟计数**上判定可用性（platform-05
// §3.5.3）：样本 <5 或成功率 <90% 的分钟只计观测、不计可用。
func modelPerfAvailability(b perfmetrics.Bucket) (observed, up float64) {
	samples := b.Success + b.Error + b.Refused
	if samples <= 0 {
		return 0, 0
	}
	observed = perfmetrics.MinuteSeconds
	if samples >= perfmetrics.MinMinuteSamples &&
		float64(b.Success)/float64(samples) >= perfmetrics.UptimeThreshold {
		up = perfmetrics.MinuteSeconds
	}
	return observed, up
}

// rollupModelPerfHours 把分钟桶汇总为小时桶。up/observed 直接累加分钟判定
// 结果，而不是在小时总样本上重新判定（否则 60 个短分钟会被当成一个长分钟）。
func rollupModelPerfHours(in []perfmetrics.Bucket) []perfmetrics.Bucket {
	type key struct {
		start time.Time
		model string
	}
	byKey := make(map[key]*perfmetrics.Bucket, len(in))
	order := make([]key, 0, len(in))
	for _, bucket := range in {
		k := key{start: bucket.Start.Truncate(time.Hour), model: bucket.Model}
		dst := byKey[k]
		if dst == nil {
			dst = &perfmetrics.Bucket{Start: k.start, Model: k.model}
			byKey[k] = dst
			order = append(order, k)
		}
		dst.Success += bucket.Success
		dst.Error += bucket.Error
		dst.Refused += bucket.Refused
		dst.LatencyCount += bucket.LatencyCount
		dst.LatencySumMS += bucket.LatencySumMS
		for i := 0; i < perfmetrics.HistogramBuckets; i++ {
			dst.LatencyBuckets[i] += bucket.LatencyBuckets[i]
			dst.TTFTBuckets[i] += bucket.TTFTBuckets[i]
		}
		dst.TTFTCount += bucket.TTFTCount
		dst.CompletionTokens += bucket.CompletionTokens
		dst.GenerationSeconds += bucket.GenerationSeconds
		dst.ObservedSeconds += bucket.ObservedSeconds
		dst.UpSeconds += bucket.UpSeconds
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].start.Equal(order[j].start) {
			return order[i].model < order[j].model
		}
		return order[i].start.Before(order[j].start)
	})
	out := make([]perfmetrics.Bucket, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// RunRetention 执行指标库侧的两层保留策略（platform-05 §3.5.4）：
//  1. 把 [now-minuteRetention-1h, 当前整点) 的分钟桶物化为小时桶；
//  2. 删除早于 minuteRetention 的分钟行（固定 7 天）；
//  3. 删除早于 hourRetention 的小时行（PERF_METRICS_RETENTION_DAYS）。
//
// 全过程在一个事务内，并用 pg_try_advisory_xact_lock 串行化多副本：拿不到
// 锁的副本直接跳过（Locked=false），不重复清理。只动**独立指标库**，绝不
// 删除主库/日志库数据。
func (r *ModelPerfRepo) RunRetention(ctx context.Context, opts perfmetrics.RetentionOptions) (perfmetrics.RetentionResult, error) {
	if r == nil || r.db == nil {
		return perfmetrics.RetentionResult{}, errors.New("store: model perf repo has no metrics database")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	minuteRetention := opts.MinuteRetention
	if minuteRetention <= 0 {
		minuteRetention = perfmetrics.MinuteRetention
	}
	hourRetention := opts.HourRetention
	if hourRetention <= 0 {
		hourRetention = 400 * 24 * time.Hour
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return perfmetrics.RetentionResult{}, fmt.Errorf("store: begin model perf retention: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var locked bool
	if err := tx.QueryRowContext(ctx,
		"SELECT pg_try_advisory_xact_lock($1)", modelPerfRetentionLockKey).Scan(&locked); err != nil {
		return perfmetrics.RetentionResult{}, fmt.Errorf("store: model perf retention lock: %w", err)
	}
	if !locked {
		return perfmetrics.RetentionResult{Locked: false}, nil
	}

	materializeTo := now.Truncate(time.Hour)
	// 从小时保留期起点回看，而不是只回看 7 天：长时间停机后分钟表里可能
	// 积压了超过 7 天的行，必须先物化再删除，否则会丢历史。WHERE 只命中
	// 分钟表中实际存在的行（有 bucket_start 索引），扫描范围由表大小决定。
	materializeFrom := now.Add(-hourRetention).Truncate(time.Hour)
	materialized, err := tx.ExecContext(ctx, modelPerfMaterializeSQL,
		materializeFrom, materializeTo)
	if err != nil {
		return perfmetrics.RetentionResult{}, fmt.Errorf("store: materialize model perf hours: %w", err)
	}
	hoursMaterialized, _ := materialized.RowsAffected()

	deletedMinutes, err := tx.ExecContext(ctx,
		`DELETE FROM model_perf_buckets WHERE bucket_start < $1`,
		now.Add(-minuteRetention))
	if err != nil {
		return perfmetrics.RetentionResult{}, fmt.Errorf("store: delete old model perf minutes: %w", err)
	}
	minutesDeleted, _ := deletedMinutes.RowsAffected()

	deletedHours, err := tx.ExecContext(ctx,
		`DELETE FROM model_perf_hourly_buckets WHERE bucket_start < $1`,
		now.Add(-hourRetention))
	if err != nil {
		return perfmetrics.RetentionResult{}, fmt.Errorf("store: delete old model perf hours: %w", err)
	}
	hoursDeleted, _ := deletedHours.RowsAffected()

	if err := tx.Commit(); err != nil {
		return perfmetrics.RetentionResult{}, fmt.Errorf("store: commit model perf retention: %w", err)
	}
	return perfmetrics.RetentionResult{
		HoursMaterialized: hoursMaterialized,
		MinutesDeleted:    minutesDeleted,
		HoursDeleted:      hoursDeleted,
		Locked:            true,
	}, nil
}

// modelPerfMaterializeSQL 把分钟桶物化为小时桶。
//
// 【为什么要子查询】up/observed 必须在**每个分钟**上判定（≥5 样本且
// 成功率 ≥90%），再按小时累加；直接在小时总样本上判定会把 60 个短分钟当成
// 一个长分钟，Uptime 口径会错。
//
// 【UTC 对齐】bucket_start 是 timestamptz；date_trunc 受会话时区影响，
// 因此显式 AT TIME ZONE 'UTC'，保证桶边界跨时区一致。
var modelPerfMaterializeSQL = `
WITH minute AS (
    SELECT date_trunc('hour', bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour_start,
           model,
           bucket_start,
           SUM(success) AS success,
           SUM(error) AS error,
           SUM(refused) AS refused,
           SUM(latency_count) AS latency_count,
           SUM(latency_sum_ms) AS latency_sum_ms,
           SUM(lat_b0) AS lat_b0, SUM(lat_b1) AS lat_b1, SUM(lat_b2) AS lat_b2,
           SUM(lat_b3) AS lat_b3, SUM(lat_b4) AS lat_b4, SUM(lat_b5) AS lat_b5,
           SUM(lat_b6) AS lat_b6, SUM(lat_b7) AS lat_b7, SUM(lat_b8) AS lat_b8,
           SUM(ttft_count) AS ttft_count,
           SUM(ttft_b0) AS ttft_b0, SUM(ttft_b1) AS ttft_b1, SUM(ttft_b2) AS ttft_b2,
           SUM(ttft_b3) AS ttft_b3, SUM(ttft_b4) AS ttft_b4, SUM(ttft_b5) AS ttft_b5,
           SUM(ttft_b6) AS ttft_b6, SUM(ttft_b7) AS ttft_b7, SUM(ttft_b8) AS ttft_b8,
           SUM(completion_tokens) AS completion_tokens,
           SUM(generation_seconds) AS generation_seconds
      FROM model_perf_buckets
     WHERE bucket_start >= $1 AND bucket_start < $2
     GROUP BY 1, 2, bucket_start
), scored AS (
    SELECT hour_start, model, success, error, refused, latency_count, latency_sum_ms,
           lat_b0, lat_b1, lat_b2, lat_b3, lat_b4, lat_b5, lat_b6, lat_b7, lat_b8,
           ttft_count, ttft_b0, ttft_b1, ttft_b2, ttft_b3, ttft_b4, ttft_b5, ttft_b6, ttft_b7, ttft_b8,
           completion_tokens, generation_seconds,
           CASE WHEN (success + error + refused) > 0 THEN 60 ELSE 0 END AS observed_seconds,
           CASE WHEN (success + error + refused) >= 5
                      AND success::double precision
                          / NULLIF(success + error + refused, 0) >= 0.9
                THEN 60 ELSE 0 END AS up_seconds
      FROM minute
)
INSERT INTO model_perf_hourly_buckets (
    bucket_start, model, ` + quotedCSV(modelPerfCounterColumns) + `,
    observed_seconds, up_seconds, updated_at
)
SELECT hour_start, model,
       SUM(success), SUM(error), SUM(refused), SUM(latency_count), SUM(latency_sum_ms),
       SUM(lat_b0), SUM(lat_b1), SUM(lat_b2), SUM(lat_b3), SUM(lat_b4), SUM(lat_b5),
       SUM(lat_b6), SUM(lat_b7), SUM(lat_b8),
       SUM(ttft_count), SUM(ttft_b0), SUM(ttft_b1), SUM(ttft_b2), SUM(ttft_b3), SUM(ttft_b4),
       SUM(ttft_b5), SUM(ttft_b6), SUM(ttft_b7), SUM(ttft_b8),
       SUM(completion_tokens), SUM(generation_seconds),
       SUM(observed_seconds), SUM(up_seconds), now()
  FROM scored
 GROUP BY 1, 2
ON CONFLICT (bucket_start, model) DO UPDATE SET
    success = EXCLUDED.success,
    error = EXCLUDED.error,
    refused = EXCLUDED.refused,
    latency_count = EXCLUDED.latency_count,
    latency_sum_ms = EXCLUDED.latency_sum_ms,
    lat_b0 = EXCLUDED.lat_b0, lat_b1 = EXCLUDED.lat_b1, lat_b2 = EXCLUDED.lat_b2,
    lat_b3 = EXCLUDED.lat_b3, lat_b4 = EXCLUDED.lat_b4, lat_b5 = EXCLUDED.lat_b5,
    lat_b6 = EXCLUDED.lat_b6, lat_b7 = EXCLUDED.lat_b7, lat_b8 = EXCLUDED.lat_b8,
    ttft_count = EXCLUDED.ttft_count,
    ttft_b0 = EXCLUDED.ttft_b0, ttft_b1 = EXCLUDED.ttft_b1, ttft_b2 = EXCLUDED.ttft_b2,
    ttft_b3 = EXCLUDED.ttft_b3, ttft_b4 = EXCLUDED.ttft_b4, ttft_b5 = EXCLUDED.ttft_b5,
    ttft_b6 = EXCLUDED.ttft_b6, ttft_b7 = EXCLUDED.ttft_b7, ttft_b8 = EXCLUDED.ttft_b8,
    completion_tokens = EXCLUDED.completion_tokens,
    generation_seconds = EXCLUDED.generation_seconds,
    observed_seconds = EXCLUDED.observed_seconds,
    up_seconds = EXCLUDED.up_seconds,
    updated_at = now()`

// ---------------------------------------------------------------------------
// 模型性能测试（渠道测速）主库仓储
// ---------------------------------------------------------------------------
//
// 本段与上面的「独立指标库」实现是两条互不相干的读/写面：
//   - 上面的 ModelPerfRepo 连接 PERF_METRICS_DSN，保存持续的分钟/小时观测；
//   - 下面的 SQLModelPerformanceStore 连接主库，保存管理员显式发起的单次
//     测速（汇总行 + 逐次明细），并依赖 channels/users 的外键。
//
// 汇总列全部由调用方在写入前算好（成功率、P50/P95、平均 TPS），仓储只负责
// 原子落库与按读面取数；空值用指针表达，读面必须输出 null / "—"，不得
// 回退成 0。

// ModelPerformanceRun 是一次测速里的单次请求观测。
type ModelPerformanceRun struct {
	RunIndex     int
	Success      bool
	TTFTMS       *int
	LatencyMS    *int
	OutputTokens *int
	TPS          *float64
	ErrorClass   string
}

// ModelPerformanceTest 是一次模型性能测试的汇总事实。
type ModelPerformanceTest struct {
	ID              int64
	ChannelID       int64
	Model           string
	Provider        string
	Runs            int
	SuccessCount    int
	SuccessRate     float64
	TTFTMSP50       *int
	TTFTMSP95       *int
	LatencyMSP50    *int
	LatencyMSP95    *int
	TPSAvg          *float64
	OutputTokensAvg *float64
	CreatedBy       *int64
	CreatedAt       time.Time
}

// ModelPerformanceStore 是测速管理面的存储接缝（单测注入内存实现）。
type ModelPerformanceStore interface {
	// Create 在同一事务内写入汇总行与其全部逐次明细，返回带生成 id 与
	// created_at 的汇总。
	Create(ctx context.Context, test ModelPerformanceTest, runs []ModelPerformanceRun) (ModelPerformanceTest, error)
	// ListRecent 返回某渠道最近的测试；model 为空表示不按模型过滤。
	// limit 收敛到 1..100（<=0 取默认 20）。
	ListRecent(ctx context.Context, channelID int64, model string, limit int) ([]ModelPerformanceTest, error)
	// LatestByModel 返回该模型（不限渠道）最新的一条测试；无记录时 ok=false。
	LatestByModel(ctx context.Context, model string) (ModelPerformanceTest, bool, error)
}

// SQLModelPerformanceStore 是 ModelPerformanceStore 的 PostgreSQL 实现。
type SQLModelPerformanceStore struct {
	db *sql.DB
}

// NewSQLModelPerformanceStore 构造仓储。db 必须是主库连接（有 channels/users）。
func NewSQLModelPerformanceStore(db *sql.DB) *SQLModelPerformanceStore {
	return &SQLModelPerformanceStore{db: db}
}

// modelPerformanceTestColumns 与 scanModelPerformanceTest 的扫描顺序一一对应。
const modelPerformanceTestColumns = `id, channel_id, model, provider, runs,
	success_count, success_rate, ttft_ms_p50, ttft_ms_p95, latency_ms_p50,
	latency_ms_p95, tps_avg, output_tokens_avg, created_by, created_at`

// Create 写入一次测试的汇总行与全部明细行。
//
// 【必须单事务】汇总行与明细行是同一事实的两个投影：只写汇总会让排障明细
// 缺失，只写明细会让列表页读不到结果。任一条明细失败即整体回滚，不会留下
// 「成功率为 N 次但明细只有 M 条」的半成品。
//
// 【RETURNING 回读】id/created_at 由数据库生成；created_at 统一转 UTC，与
// 其它仓储的读出口径一致。
func (r *SQLModelPerformanceStore) Create(
	ctx context.Context,
	test ModelPerformanceTest,
	runs []ModelPerformanceRun,
) (ModelPerformanceTest, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelPerformanceTest{}, fmt.Errorf("store: begin model performance test: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRowContext(ctx, `
		INSERT INTO model_performance_tests
			(channel_id, model, provider, runs, success_count, success_rate,
			 ttft_ms_p50, ttft_ms_p95, latency_ms_p50, latency_ms_p95,
			 tps_avg, output_tokens_avg, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, created_at`,
		test.ChannelID, test.Model, test.Provider, test.Runs, test.SuccessCount,
		test.SuccessRate, test.TTFTMSP50, test.TTFTMSP95, test.LatencyMSP50,
		test.LatencyMSP95, test.TPSAvg, test.OutputTokensAvg, test.CreatedBy,
	).Scan(&test.ID, &test.CreatedAt); err != nil {
		return ModelPerformanceTest{}, fmt.Errorf(
			"store: insert model performance test(%s/%d): %w", test.Model, test.ChannelID, err)
	}
	test.CreatedAt = test.CreatedAt.UTC()

	// runs 上限是 10（schema CHECK），逐条插入保持参数简单，不做动态多行 SQL。
	for _, run := range runs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_performance_runs
				(test_id, run_index, success, ttft_ms, latency_ms, output_tokens, tps, error_class)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			test.ID, run.RunIndex, run.Success, run.TTFTMS, run.LatencyMS,
			run.OutputTokens, run.TPS, nullableErrorClass(run.ErrorClass),
		); err != nil {
			return ModelPerformanceTest{}, fmt.Errorf(
				"store: insert model performance run %d: %w", run.RunIndex, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return ModelPerformanceTest{}, fmt.Errorf("store: commit model performance test: %w", err)
	}
	return test, nil
}

// ListRecent 按 created_at DESC 返回渠道最近的测试。
//
// 同一毫秒内的两条测试用 id DESC 兜底，保证分页/快照读取顺序确定。
func (r *SQLModelPerformanceStore) ListRecent(
	ctx context.Context,
	channelID int64,
	model string,
	limit int,
) ([]ModelPerformanceTest, error) {
	limit = clampModelPerfLimit(limit)
	query := `SELECT ` + modelPerformanceTestColumns +
		` FROM model_performance_tests WHERE channel_id = $1`
	args := []any{channelID}
	if model != "" {
		args = append(args, model)
		query += fmt.Sprintf(" AND model = $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list model performance tests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]ModelPerformanceTest, 0, limit)
	for rows.Next() {
		test, scanErr := scanModelPerformanceTest(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan model performance test: %w", scanErr)
		}
		items = append(items, test)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate model performance tests: %w", err)
	}
	return items, nil
}

// LatestByModel 返回该模型（任意渠道）最新的一条测试。
//
// 模型相同但渠道不同是常见情形（同一模型挂在多个渠道）；本读面只回答
// 「这个模型最近一次测速结果」，因此跨渠道取 created_at 最大的一行。
// 无记录不是错误：ok=false 让调用方渲染空状态而不是 500。
func (r *SQLModelPerformanceStore) LatestByModel(
	ctx context.Context,
	model string,
) (ModelPerformanceTest, bool, error) {
	test, err := scanModelPerformanceTest(r.db.QueryRowContext(ctx,
		`SELECT `+modelPerformanceTestColumns+`
		 FROM model_performance_tests WHERE model = $1
		 ORDER BY created_at DESC, id DESC LIMIT 1`, model))
	if errors.Is(err, sql.ErrNoRows) {
		return ModelPerformanceTest{}, false, nil
	}
	if err != nil {
		return ModelPerformanceTest{}, false, fmt.Errorf(
			"store: latest model performance test(%s): %w", model, err)
	}
	return test, true, nil
}

// scanModelPerformanceTest 读取汇总行，并把可空列映射为指针。
func scanModelPerformanceTest(row interface{ Scan(...any) error }) (ModelPerformanceTest, error) {
	var (
		test     ModelPerformanceTest
		ttftP50  sql.NullInt64
		ttftP95  sql.NullInt64
		latP50   sql.NullInt64
		latP95   sql.NullInt64
		tpsAvg   sql.NullFloat64
		tokens   sql.NullFloat64
		createdB sql.NullInt64
	)
	if err := row.Scan(
		&test.ID, &test.ChannelID, &test.Model, &test.Provider, &test.Runs,
		&test.SuccessCount, &test.SuccessRate, &ttftP50, &ttftP95, &latP50, &latP95,
		&tpsAvg, &tokens, &createdB, &test.CreatedAt,
	); err != nil {
		return ModelPerformanceTest{}, err
	}
	test.TTFTMSP50 = intPtrFromNull(ttftP50)
	test.TTFTMSP95 = intPtrFromNull(ttftP95)
	test.LatencyMSP50 = intPtrFromNull(latP50)
	test.LatencyMSP95 = intPtrFromNull(latP95)
	test.TPSAvg = float64PtrFromNull(tpsAvg)
	test.OutputTokensAvg = float64PtrFromNull(tokens)
	test.CreatedBy = int64PtrFromNull(createdB)
	test.CreatedAt = test.CreatedAt.UTC()
	return test, nil
}

func intPtrFromNull(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	n := int(value.Int64)
	return &n
}

func int64PtrFromNull(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	n := value.Int64
	return &n
}

func float64PtrFromNull(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	f := value.Float64
	return &f
}

// nullableErrorClass 把「无错误分类」写成 SQL NULL 而不是空串：明细表只用
// error_class 区分失败原因，成功行（或未分类失败）没有该事实。
func nullableErrorClass(class string) any {
	if class == "" {
		return nil
	}
	return class
}

// clampModelPerfLimit 按读面契约收敛 limit：<=0 取默认 20，>100 截到 100。
// 上限防止调用方把一次列表放大成整表扫描；非正数不能直接进 LIMIT（PG 会报错）。
func clampModelPerfLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// Percentile 返回升序整数样本的 p 分位数（0 ≤ p ≤ 1），空切片返回 ok=false。
//
// 【算法：nearest-rank】rank = ceil(p*n)，再收敛到 [1,n]，返回 sorted[rank-1]。
// 选 nearest-rank 而不是线性插值，是因为延迟/TTFT 样本是整数毫秒，插值会
// 造出原始样本中不存在的值；nearest-rank 的结果必定是某个真实观测。
// p=0 返回最小值，p=1 返回最大值；p<0 / p>1 分别按 0 / 1 处理，NaN 按 0。
//
// 【输入必须升序，本函数不排序】调用方（测速汇总）已经持有排好序的样本，
// 重复调用时再排序是 O(n log n) 的无谓开销。传入未排序切片不会 panic——
// 返回的仍会是切片内的某个元素，但不再是统计意义上的分位数；这是调用方
// 的契约错误，函数不做静默纠正。
func Percentile(sorted []int, p float64) (int, bool) {
	n := len(sorted)
	if n == 0 {
		return 0, false
	}
	if math.IsNaN(p) {
		p = 0
	}
	if p <= 0 {
		return sorted[0], true
	}
	if p >= 1 {
		return sorted[n-1], true
	}
	rank := int(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1], true
}
