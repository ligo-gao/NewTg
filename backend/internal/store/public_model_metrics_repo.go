package store

import (
	"context"
	"database/sql"
	"time"
)

// PublicModelMetrics 是公开性能接口的一次聚合结果。
//
// 口径来自 modules-09 F-09-25：延迟取 usage_logs.latency_ms 的平均值；
// TTFT 取 first_token_ms 的平均值（仅流式调用有值）；TPS 由调用方用
// completion_tokens / (latency_sum_ms/1000) 计算。所有平均列在没有样本
// 时为 nil，调用方必须输出 null / 前端显示 "—"，不得回退成 0。
type PublicModelMetrics struct {
	SampleSize       int64
	SuccessCount     int64
	Failed24h        int64
	LatencySumMs     int64
	CompletionTokens int64
	AvgLatencyMs     *float64
	AvgTTFTMs        *float64
	Groups           []PublicGroupMetrics
}

// PublicGroupMetrics 是同一模型按调用分组拆开的性能行。
type PublicGroupMetrics struct {
	Group        string
	SampleSize   int64
	SuccessCount int64
	AvgLatencyMs *float64
	AvgTTFTMs    *float64
}

// PublicModelMetricsSource 是公开性能接口的数据接缝。
//
// 生产实现读日志库（DB_LOGS_DSN）；测试注入内存实现，避免把 HTTP 契约
// 绑在真实 PostgreSQL 上。窗口为左闭右开 [start, end)。
type PublicModelMetricsSource interface {
	LoadPublicModelMetrics(ctx context.Context, model string, start, end time.Time) (PublicModelMetrics, error)
}

// SQLPublicModelMetricsRepo 从日志库的 usage_logs 聚合公开性能指标。
type SQLPublicModelMetricsRepo struct {
	db *sql.DB
}

func NewPublicModelMetricsRepo(db *sql.DB) *SQLPublicModelMetricsRepo {
	return &SQLPublicModelMetricsRepo{db: db}
}

const publicModelMetricsSummaryQuery = `
SELECT
    count(*) AS sample_size,
    count(*) FILTER (WHERE status = 1) AS success_count,
    count(*) FILTER (WHERE status = 2 AND created_at >= now() - interval '24 hours') AS failed_24h,
    COALESCE(sum(latency_ms), 0)::bigint AS latency_sum_ms,
    COALESCE(sum(completion_tokens), 0)::bigint AS completion_tokens,
    avg(latency_ms) FILTER (WHERE latency_ms IS NOT NULL)::double precision AS avg_latency_ms,
    avg(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL)::double precision AS avg_ttft_ms
FROM usage_logs
WHERE model = $1 AND created_at >= $2 AND created_at < $3`

const publicModelMetricsGroupsQuery = `
SELECT
    COALESCE("group", '') AS group_name,
    count(*) AS sample_size,
    count(*) FILTER (WHERE status = 1) AS success_count,
    avg(latency_ms) FILTER (WHERE latency_ms IS NOT NULL)::double precision AS avg_latency_ms,
    avg(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL)::double precision AS avg_ttft_ms
FROM usage_logs
WHERE model = $1 AND created_at >= $2 AND created_at < $3
GROUP BY COALESCE("group", '')
ORDER BY group_name`

func (r *SQLPublicModelMetricsRepo) LoadPublicModelMetrics(
	ctx context.Context,
	model string,
	start, end time.Time,
) (PublicModelMetrics, error) {
	var (
		out         PublicModelMetrics
		avgLatency  sql.NullFloat64
		avgTTFT     sql.NullFloat64
		groupsQuery *sql.Rows
		err         error
	)

	if err = r.db.QueryRowContext(ctx, publicModelMetricsSummaryQuery, model, start, end).Scan(
		&out.SampleSize,
		&out.SuccessCount,
		&out.Failed24h,
		&out.LatencySumMs,
		&out.CompletionTokens,
		&avgLatency,
		&avgTTFT,
	); err != nil {
		return PublicModelMetrics{}, err
	}
	if avgLatency.Valid {
		out.AvgLatencyMs = &avgLatency.Float64
	}
	if avgTTFT.Valid {
		out.AvgTTFTMs = &avgTTFT.Float64
	}

	groupsQuery, err = r.db.QueryContext(ctx, publicModelMetricsGroupsQuery, model, start, end)
	if err != nil {
		return PublicModelMetrics{}, err
	}
	defer func() { _ = groupsQuery.Close() }()

	for groupsQuery.Next() {
		var (
			row       PublicGroupMetrics
			groupAvg  sql.NullFloat64
			groupTTFT sql.NullFloat64
		)
		if err = groupsQuery.Scan(
			&row.Group,
			&row.SampleSize,
			&row.SuccessCount,
			&groupAvg,
			&groupTTFT,
		); err != nil {
			return PublicModelMetrics{}, err
		}
		if groupAvg.Valid {
			row.AvgLatencyMs = &groupAvg.Float64
		}
		if groupTTFT.Valid {
			row.AvgTTFTMs = &groupTTFT.Float64
		}
		out.Groups = append(out.Groups, row)
	}
	if err = groupsQuery.Err(); err != nil {
		return PublicModelMetrics{}, err
	}

	return out, nil
}
