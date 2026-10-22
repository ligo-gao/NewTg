package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"newtg/gateway/internal/perfmetrics"
)

// 本文件是 platform-05 §3.5 模型性能采集管道的持久化实现（G-M6C-13）。
//
// 【为什么在日志库而不是主库】采集数据是观测事实，不是计费真源；日志库
// 已经是 usage_logs / audit_logs 的观测面，且 newtg_app 对主库没有写权限
// 之外的分区也无需扩展。设计 §3.5.3 的「不落业务库」指的是主库业务表；
// 单 PostgreSQL 部署下没有独立的 VictoriaMetrics/ClickHouse，日志库表是
// 当前可用且与业务真源隔离的持久化落点（M7 指标库选型后可平滑替换 Repo）。
//
// 【绝对快照】每次 flush 写入的是「该分钟桶到目前为止的累计量」，不是增量。
// 唯一键 (bucket_start, model, node, epoch) + flush_seq 条件 upsert 保证：
//   - 同一快照重复写入（网络重试/停机重放）是 no-op；
//   - 迟到样本让进程内累计量变大，flush_seq 变大后覆盖旧值；
//   - 旧 epoch 的末尾快照与新 epoch 的快照共存，查询时跨 epoch 求和。
//
// 【小时桶】本实现不在写路径物化小时桶：查询 7d/30d 时按 1 小时分组
// 汇总分钟行。每分钟的 up/observed 先在分钟粒度判定再累加，满足
// 「先求和判定可用分钟」的口径。分钟行保留策略由平台既有 retention 任务
// 后续接管（本任务只接线采集与查询）。

// modelPerfInsertColumns 是快照写入列（updated_at 由 NOW() 生成）。
var modelPerfInsertColumns = []string{
	"bucket_start", "model", "node", "epoch", "flush_seq",
	"success", "error", "refused",
	"latency_count", "latency_sum_ms",
	"lat_b0", "lat_b1", "lat_b2", "lat_b3", "lat_b4", "lat_b5", "lat_b6", "lat_b7", "lat_b8",
	"ttft_count",
	"ttft_b0", "ttft_b1", "ttft_b2", "ttft_b3", "ttft_b4", "ttft_b5", "ttft_b6", "ttft_b7", "ttft_b8",
	"completion_tokens", "generation_seconds",
}

// modelPerfAggregateSelect 是读路径的聚合表达式。直方图是每桶非累计计数，
// 查询只做 SUM；+Inf 由调用方用 count - Σbuckets 推导。
var modelPerfAggregateSelect = strings.Join([]string{
	"COALESCE(SUM(success), 0)",
	"COALESCE(SUM(error), 0)",
	"COALESCE(SUM(refused), 0)",
	"COALESCE(SUM(latency_count), 0)",
	"COALESCE(SUM(latency_sum_ms), 0)",
	"COALESCE(SUM(lat_b0), 0)", "COALESCE(SUM(lat_b1), 0)", "COALESCE(SUM(lat_b2), 0)",
	"COALESCE(SUM(lat_b3), 0)", "COALESCE(SUM(lat_b4), 0)", "COALESCE(SUM(lat_b5), 0)",
	"COALESCE(SUM(lat_b6), 0)", "COALESCE(SUM(lat_b7), 0)", "COALESCE(SUM(lat_b8), 0)",
	"COALESCE(SUM(ttft_count), 0)",
	"COALESCE(SUM(ttft_b0), 0)", "COALESCE(SUM(ttft_b1), 0)", "COALESCE(SUM(ttft_b2), 0)",
	"COALESCE(SUM(ttft_b3), 0)", "COALESCE(SUM(ttft_b4), 0)", "COALESCE(SUM(ttft_b5), 0)",
	"COALESCE(SUM(ttft_b6), 0)", "COALESCE(SUM(ttft_b7), 0)", "COALESCE(SUM(ttft_b8), 0)",
	"COALESCE(SUM(completion_tokens), 0)",
	"COALESCE(SUM(generation_seconds), 0)",
	"COALESCE(MAX(updated_at), 'epoch'::timestamptz)",
}, ", ")

// ModelPerfRepo 是 perfmetrics.Store 的 PostgreSQL 实现。
type ModelPerfRepo struct{ db *sql.DB }

// NewModelPerfRepo 构造仓储；db 必须是**日志库**（model_perf_buckets 所在库）。
func NewModelPerfRepo(db *sql.DB) *ModelPerfRepo { return &ModelPerfRepo{db: db} }

// UpsertBuckets 批量写入绝对快照；分批事务，避免单条语句参数过多。
func (r *ModelPerfRepo) UpsertBuckets(ctx context.Context, snapshots []perfmetrics.Snapshot) error {
	if r == nil || r.db == nil {
		return errors.New("store: model perf repo has no logs database")
	}
	if len(snapshots) == 0 {
		return nil
	}
	const batchSize = 100
	for start := 0; start < len(snapshots); start += batchSize {
		end := start + batchSize
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
	cols := strings.Join(modelPerfInsertColumns, ", ")
	var b strings.Builder
	b.Grow(512 + rows*len(modelPerfInsertColumns)*8)
	b.WriteString("INSERT INTO model_perf_buckets (")
	b.WriteString(cols)
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
	b.WriteString(` ON CONFLICT (bucket_start, model, node, epoch) DO UPDATE SET
		flush_seq = EXCLUDED.flush_seq,
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
		updated_at = now()
	WHERE EXCLUDED.flush_seq > model_perf_buckets.flush_seq`)
	return b.String()
}

// ModelPerfBuckets 实现 perfmetrics.Source：按窗口聚合分钟桶，granularity=1h
// 时在 Go 侧汇总为小时桶（每分钟的 up/observed 先判定再累加）。
func (r *ModelPerfRepo) ModelPerfBuckets(ctx context.Context, start, end time.Time,
	granularity, model string) ([]perfmetrics.Bucket, time.Time, error) {

	if r == nil || r.db == nil {
		return nil, time.Time{}, errors.New("store: model perf repo has no logs database")
	}
	if granularity != "1m" && granularity != "1h" {
		return nil, time.Time{}, fmt.Errorf("store: invalid model perf granularity %q", granularity)
	}
	where := "bucket_start >= $1 AND bucket_start < $2"
	args := []any{start.UTC(), end.UTC()}
	if model != "" {
		where += " AND model = $3"
		args = append(args, model)
	}
	query := `SELECT bucket_start, model, ` + modelPerfAggregateSelect + `
		FROM model_perf_buckets
		WHERE ` + where + `
		GROUP BY bucket_start, model
		ORDER BY bucket_start, model`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: query model perf buckets: %w", err)
	}
	defer rows.Close()

	out := make([]perfmetrics.Bucket, 0, 64)
	var updated time.Time
	for rows.Next() {
		var (
			bucket                  perfmetrics.Bucket
			latencySum              float64
			latencyBuckets          [perfmetrics.HistogramBuckets]int64
			ttftBuckets             [perfmetrics.HistogramBuckets]int64
			rowUpdated              time.Time
			latencyCount, ttftCount int64
		)
		if err := rows.Scan(
			&bucket.Start, &bucket.Model,
			&bucket.Success, &bucket.Error, &bucket.Refused,
			&latencyCount, &latencySum,
			&latencyBuckets[0], &latencyBuckets[1], &latencyBuckets[2],
			&latencyBuckets[3], &latencyBuckets[4], &latencyBuckets[5],
			&latencyBuckets[6], &latencyBuckets[7], &latencyBuckets[8],
			&ttftCount,
			&ttftBuckets[0], &ttftBuckets[1], &ttftBuckets[2],
			&ttftBuckets[3], &ttftBuckets[4], &ttftBuckets[5],
			&ttftBuckets[6], &ttftBuckets[7], &ttftBuckets[8],
			&bucket.CompletionTokens, &bucket.GenerationSeconds,
			&rowUpdated,
		); err != nil {
			return nil, time.Time{}, fmt.Errorf("store: scan model perf bucket: %w", err)
		}
		bucket.Start = bucket.Start.UTC()
		bucket.LatencyCount = latencyCount
		bucket.LatencySumMS = latencySum
		bucket.LatencyBuckets = latencyBuckets
		bucket.TTFTCount = ttftCount
		bucket.TTFTBuckets = ttftBuckets
		bucket.ObservedSeconds, bucket.UpSeconds = modelPerfAvailability(bucket)
		if rowUpdated.After(updated) {
			updated = rowUpdated.UTC()
		}
		out = append(out, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: iterate model perf buckets: %w", err)
	}
	if granularity == "1h" {
		out = rollupModelPerfHours(out)
	}
	return out, updated, nil
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
