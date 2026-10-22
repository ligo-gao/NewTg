package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 本文件是 F-06-11 渠道健康与用量指标的日志库读取面（W16-1 裁决 R-W16-01）。
//
// 数据源是**日志库** usage_logs（deps.LogDB），不是主库：指标是「发生过什么」
// 的只读事实，主库渠道行只承载配置与 last_test_*。聚合按整点小时桶，
// 24h=24 点、7d=168 点（modules-06 F-06-11 TC-F-06-11-03）。
//
// 口径冻结（modules-05 §10.2）：
//   success_rate = success / (success + error + refused)
//
// refunded 不计入分母也不计入分子（它不是失败，只是结算撤销）；refused 计入
// 分母（被限流/拒绝的请求同样是渠道质量信号）。这与 handleUsageStatsSummary
// 的「成功数 / 请求总数」不是同一口径，后者含 refunded；本文件按模块 05
// §10.2 的指标口径实现，两条口径各自有测试固定，禁止互相「对齐」。
//
// 空值语义（modules-06 F-06-11 验收「新渠道显示 — 而非 0」）：
//   - 计数类（request/success/failure/cost）无数据 → 0；
//   - 比率/均值无样本 → JSON null（Go 侧 *float64 = nil），绝不写 0。
//
// 查询只读且有 created_at 窗口，依赖 W16-1 新增的日志库索引
// usage_logs_channel_created_idx (channel_id, created_at DESC) 把扫描收敛到
// 单渠道窗口；父表索引自动下推到分区（PG 11+）。

// 渠道指标窗口（modules-06 F-06-11 的 24h / 7d Tabs）。
const (
	ChannelMetricsWindow24h = "24h"
	ChannelMetricsWindow7d  = "7d"
)

// channelMetricsBuckets 是窗口 → 整点桶数。7d 必须恰为 168 点，
// 不是 7 点（TC-F-06-11-03）。
var channelMetricsBuckets = map[string]int{
	ChannelMetricsWindow24h: 24,
	ChannelMetricsWindow7d:  7 * 24,
}

// ChannelMetricsWindowBuckets 返回窗口的桶数；未知窗口返回 0,false。
// handler 用它做 422 校验，避免「非法 window 静默回退 24h」。
func ChannelMetricsWindowBuckets(window string) (int, bool) {
	n, ok := channelMetricsBuckets[window]
	return n, ok
}

// ChannelMetricsPoint 是一个整点桶的聚合。
//
// 未导出字段只在聚合完成前使用（加权求窗口均值），不参与 JSON：
// 缓存回读后 summary 已经是最终值，不依赖这些字段。
type ChannelMetricsPoint struct {
	BucketStart     time.Time `json:"bucket_start"`
	RequestCount    int64     `json:"request_count"`
	SuccessCount    int64     `json:"success_count"`
	FailureCount    int64     `json:"failure_count"`
	SuccessRate     *float64  `json:"success_rate"`
	AvgLatencyMS    *float64  `json:"avg_latency_ms"`
	AvgFirstTokenMS *float64  `json:"avg_first_token_ms"`
	AvgCost         *float64  `json:"avg_cost"`

	latencySamples    int64
	firstTokenSamples int64
	costTotal         int64
}

// ChannelMetricsSummary 是窗口级汇总，并带上设计要求的累计/近 24h 字段。
type ChannelMetricsSummary struct {
	RequestCount    int64    `json:"request_count"`
	SuccessCount    int64    `json:"success_count"`
	FailureCount    int64    `json:"failure_count"`
	SuccessRate     *float64 `json:"success_rate"`
	AvgLatencyMS    *float64 `json:"avg_latency_ms"`
	AvgFirstTokenMS *float64 `json:"avg_first_token_ms"`
	AvgCost         *float64 `json:"avg_cost"`
	CostTotal       int64    `json:"cost_total"`
	// UsedCredits 是渠道**累计**结算积分（全量历史）；UsedCredits24h /
	// RequestCount24h 固定看最近 24 小时，与所选 window 无关（设计字段表）。
	UsedCredits     int64 `json:"used_credits"`
	UsedCredits24h  int64 `json:"used_credits_24h"`
	RequestCount24h int64 `json:"request_count_24h"`
}

// ChannelMetrics 是 GET /api/channels/{id}/metrics 的日志库侧数据。
type ChannelMetrics struct {
	Points  []ChannelMetricsPoint `json:"points"`
	Summary ChannelMetricsSummary `json:"summary"`
}

// ChannelSummary 是渠道列表内嵌的 24h 概要（一次批量查询的结果项）。
type ChannelSummary struct {
	RequestCount int64
	SuccessRate  *float64
	AvgLatencyMS *float64
}

// ChannelMetricsRepo 读取日志库的 usage_logs 聚合。
type ChannelMetricsRepo struct{ db *sql.DB }

// NewChannelMetricsRepo 构造仓储；db 必须是**日志库**（usage_logs 所在库）。
func NewChannelMetricsRepo(db *sql.DB) *ChannelMetricsRepo {
	return &ChannelMetricsRepo{db: db}
}

// Get 返回指定渠道在窗口内的逐小时桶与窗口汇总。
//
// now 由调用方注入（handler 传 time.Now().UTC()；测试传固定时刻），
// 桶边界固定对齐到整点：end = now 向下取整到小时 + 1h，start = end - N 小时。
// 最后一个桶是当前（未走完的）小时，因此「24 点」覆盖最近 24 个小时边界。
func (r *ChannelMetricsRepo) Get(ctx context.Context, channelID int64, window string, now time.Time) (*ChannelMetrics, error) {
	buckets, ok := ChannelMetricsWindowBuckets(window)
	if !ok {
		return nil, fmt.Errorf("store: unknown channel metrics window %q", window)
	}
	start, end := channelMetricsBounds(now, buckets)

	out := &ChannelMetrics{Points: make([]ChannelMetricsPoint, 0, buckets)}
	rows, err := r.db.QueryContext(ctx, channelMetricsPointsQuery, channelID, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: query channel %d metrics: %w", channelID, err)
	}
	defer rows.Close()

	var summary ChannelMetricsSummary
	for rows.Next() {
		var (
			p        ChannelMetricsPoint
			latency  sql.NullFloat64
			firstTok sql.NullFloat64
			latN     int64
			ttftN    int64
			costSum  int64
		)
		if err := rows.Scan(
			&p.BucketStart, &p.RequestCount, &p.SuccessCount, &p.FailureCount,
			&latency, &latN, &firstTok, &ttftN, &costSum,
		); err != nil {
			return nil, fmt.Errorf("store: scan channel %d metrics: %w", channelID, err)
		}
		p.BucketStart = p.BucketStart.UTC()
		p.latencySamples = latN
		p.firstTokenSamples = ttftN
		p.costTotal = costSum
		if latency.Valid {
			v := latency.Float64
			p.AvgLatencyMS = &v
		}
		if firstTok.Valid {
			v := firstTok.Float64
			p.AvgFirstTokenMS = &v
		}
		if p.RequestCount > 0 {
			v := float64(costSum) / float64(p.RequestCount)
			p.AvgCost = &v
		}
		p.SuccessRate = ratio(p.SuccessCount, p.SuccessCount+p.FailureCount)

		summary.RequestCount += p.RequestCount
		summary.SuccessCount += p.SuccessCount
		summary.FailureCount += p.FailureCount
		summary.CostTotal += p.costTotal
		out.Points = append(out.Points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel %d metrics: %w", channelID, err)
	}

	// 加权均值：逐桶 avg × 样本数再除以总样本数，等价于窗口直算，且无需
	// 再扫一遍分区。计数为 0 的桶样本数也是 0，不污染分母。
	summary.AvgLatencyMS = weightedAverage(out.Points, func(p ChannelMetricsPoint) (*float64, int64) {
		return p.AvgLatencyMS, p.latencySamples
	})
	summary.AvgFirstTokenMS = weightedAverage(out.Points, func(p ChannelMetricsPoint) (*float64, int64) {
		return p.AvgFirstTokenMS, p.firstTokenSamples
	})
	if summary.RequestCount > 0 {
		v := float64(summary.CostTotal) / float64(summary.RequestCount)
		summary.AvgCost = &v
	}
	summary.SuccessRate = ratio(summary.SuccessCount, summary.SuccessCount+summary.FailureCount)

	// 近 24h 固定取最后 24 个整点桶（即最近 24 小时，与 window 无关）。
	n24 := len(out.Points)
	if n24 > 24 {
		n24 = 24
	}
	for _, p := range out.Points[len(out.Points)-n24:] {
		summary.UsedCredits24h += p.costTotal
		summary.RequestCount24h += p.RequestCount
	}

	// 累计结算积分是跨全部历史的单行聚合；与上面共用一次往返（两句 SQL，
	// 但都在同一只读语义内，无事务需求）。
	if err := r.db.QueryRowContext(ctx, channelMetricsUsedCreditsQuery, channelID).
		Scan(&summary.UsedCredits); err != nil {
		return nil, fmt.Errorf("store: query channel %d cumulative credits: %w", channelID, err)
	}
	out.Summary = summary
	return out, nil
}

// Summaries 用**一次** GROUP BY 查询取回多个渠道的 24h 概要，供渠道列表内嵌。
// 返回 map 只包含有样本的渠道；调用方对未命中的渠道填零值概要（不是「无字段」），
// 从而把「0 次请求」与「日志库不可用」在 API 层区分开。
//
// 绝不逐渠道查询（N+1）：列表页 sizes 最大 100，逐行查询会把一次列表变成
// 101 次日志库往返（变异点 ② 的守护用例）。
func (r *ChannelMetricsRepo) Summaries(ctx context.Context, channelIDs []int64, now time.Time) (map[int64]ChannelSummary, error) {
	out := make(map[int64]ChannelSummary, len(channelIDs))
	if len(channelIDs) == 0 {
		return out, nil
	}
	// 24h 窗口对齐到整点，与 Get(window=24h) 完全一致（前端列与详情不打架）。
	start, end := channelMetricsBounds(now, 24)

	args := make([]any, 0, len(channelIDs)+2)
	args = append(args, start, end)
	placeholders := make([]string, 0, len(channelIDs))
	for _, id := range channelIDs {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	query := fmt.Sprintf(channelMetricsSummariesQuery, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query channel metrics summaries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id      int64
			req     int64
			success int64
			failure int64
			latency sql.NullFloat64
		)
		if err := rows.Scan(&id, &req, &success, &failure, &latency); err != nil {
			return nil, fmt.Errorf("store: scan channel metrics summary: %w", err)
		}
		s := ChannelSummary{
			RequestCount: req,
			SuccessRate:  ratio(success, success+failure),
		}
		if latency.Valid {
			v := latency.Float64
			s.AvgLatencyMS = &v
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel metrics summaries: %w", err)
	}
	return out, nil
}

// channelMetricsBounds 返回整点对齐的 [start, end)；end 是 now 所在小时的
// 下一小时，因此最后一个桶是当前小时（可能只走了一部分）。
func channelMetricsBounds(now time.Time, buckets int) (time.Time, time.Time) {
	end := now.UTC().Truncate(time.Hour).Add(time.Hour)
	start := end.Add(-time.Duration(buckets) * time.Hour)
	return start, end
}

// ratio 返回 0–1 比值；分母为 0 时返回 nil（JSON null，前端显示 —）。
func ratio(numerator, denominator int64) *float64 {
	if denominator <= 0 {
		return nil
	}
	v := float64(numerator) / float64(denominator)
	return &v
}

// weightedAverage 按样本数加权；所有桶都没样本时返回 nil。
func weightedAverage(
	points []ChannelMetricsPoint,
	pick func(ChannelMetricsPoint) (*float64, int64),
) *float64 {
	var (
		sum     float64
		samples int64
	)
	for _, p := range points {
		avg, n := pick(p)
		if avg == nil || n <= 0 {
			continue
		}
		sum += *avg * float64(n)
		samples += n
	}
	if samples == 0 {
		return nil
	}
	v := sum / float64(samples)
	return &v
}

// 逐桶聚合。generate_series 生成 24/168 个整点桶，LEFT JOIN 保证零流量小时
// 也出现在序列里（否则前端折线会在无流量处「跳点」）。
//
// to_timestamp(floor(epoch/3600)*3600) 而不是 date_trunc('hour', created_at)：
// date_trunc 对 timestamptz 使用会话时区，测试库/生产库时区不同会让桶边界漂移；
// 按 epoch 取整永远是 UTC 整点，与 generate_series 的 timestamptz 时刻直接可比。
const channelMetricsPointsQuery = `
WITH buckets AS (
    SELECT generate_series(
        $2::timestamptz,
        $3::timestamptz - interval '1 microsecond',
        interval '1 hour'
    ) AS bucket_start
),
agg AS (
    SELECT
        to_timestamp(floor(extract(epoch FROM created_at) / 3600) * 3600) AS bucket_start,
        count(*) AS request_count,
        count(*) FILTER (WHERE status = 1) AS success_count,
        count(*) FILTER (WHERE status IN (0, 2)) AS failure_count,
        avg(latency_ms) FILTER (WHERE latency_ms IS NOT NULL)::double precision AS avg_latency_ms,
        count(latency_ms) AS latency_samples,
        avg(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL)::double precision AS avg_first_token_ms,
        count(first_token_ms) AS first_token_samples,
        COALESCE(sum(cost), 0)::bigint AS cost_total
    FROM usage_logs
    WHERE channel_id = $1 AND created_at >= $2 AND created_at < $3
    GROUP BY 1
)
SELECT
    b.bucket_start,
    COALESCE(a.request_count, 0),
    COALESCE(a.success_count, 0),
    COALESCE(a.failure_count, 0),
    a.avg_latency_ms,
    COALESCE(a.latency_samples, 0),
    a.avg_first_token_ms,
    COALESCE(a.first_token_samples, 0),
    COALESCE(a.cost_total, 0)
FROM buckets b
LEFT JOIN agg a ON a.bucket_start = b.bucket_start
ORDER BY b.bucket_start`

// 累计结算积分：无窗口过滤（设计字段 used_credits 是渠道全量），
// 依赖 (channel_id, created_at) 索引把扫描收敛到该渠道的分区行。
const channelMetricsUsedCreditsQuery = `
SELECT COALESCE(sum(cost), 0)::bigint FROM usage_logs WHERE channel_id = $1`

// 列表 24h 概要的单次批量查询模板；%s 是 IN 列表占位符。
// 参数顺序：$1=start, $2=end, $3..=channel ids。
const channelMetricsSummariesQuery = `
SELECT
    channel_id,
    count(*) AS request_count,
    count(*) FILTER (WHERE status = 1) AS success_count,
    count(*) FILTER (WHERE status IN (0, 2)) AS failure_count,
    avg(latency_ms) FILTER (WHERE latency_ms IS NOT NULL)::double precision AS avg_latency_ms
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2
  AND channel_id IN (%s)
GROUP BY channel_id`
