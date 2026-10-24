package store

// 本文件是 usage_logs（**日志库**）的对账聚合查询（上游用量对账设计 §7.1）。
//
// 关键口径：
//   - 日志库与主库是独立连接，聚合 SQL **不得 JOIN channels**。渠道归属通过
//     调用方传入的 channelIDs 作为参数集合参与过滤（legacy 兜底），账号归属
//     则读取写日志时固化的 usage_logs.upstream_account_id；
//   - 「一天」是调用方按账号时区算好的半开 UTC 区间 [start, end)，SQL 里
//     不再做 24h 运算；分组用的本地日期由 created_at AT TIME ZONE tz 得到；
//   - usage_semantic 为 NULL 的历史行语义未知：cache_hit 与 output 仍可汇总
//     （两套语义下含义一致），但缓存未命中无法确定，miss 记 0，并通过
//     unknown_requests 披露（领域层据此把 item 抬升为 warn）；
//   - 历史行没有 upstream_account_id，按 (NULL AND channel_id IN 渠道集)
//     归属，保证切换后的首次对账不会整片 missing_local；
//   - 计数**不按 status 过滤**（设计 §7.1）：失败请求的 token 上游可能已扣费，
//     必须计入；退款行同时计入 requests 与 refunded_requests，才能满足 items
//     表的 CHECK (refunded_requests <= requests)。status = 0（拒绝）也计入
//     requests——它没有 token/费用，且 requests 不参与严重度判定，只影响展示。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LocalUsage 是本地 usage_logs 在 (日期, 模型) 维度的聚合值。
// Model 取 COALESCE(upstream_model, model)：历史行没有 upstream_model 时
// 退回客户端模型名，避免同一天同一模型出现两条互不合并的记录。
type LocalUsage struct {
	UsageDate            time.Time
	Model                string
	Requests             int64
	EstimatedRequests    int64
	RefundedRequests     int64
	CacheHitTokens       int64
	CacheMissTokens      int64
	OutputTokens         int64
	WalletCredits        int64
	SubscriptionCredits  int64
	CostCredits          int64
	UnknownRequests      int64
	MultiAttemptRequests int64
}

// UsageAggregateRepo 是日志库 usage_logs 的只读聚合仓储。
type UsageAggregateRepo struct{ db *sql.DB }

// NewUsageAggregateRepo 构造仓储；logsDB 是独立的日志库连接。
func NewUsageAggregateRepo(logsDB *sql.DB) *UsageAggregateRepo {
	return &UsageAggregateRepo{db: logsDB}
}

// aggregateQuery 按 (本地日期, 上游模型) 聚合。
//
// 参数：$1 start（含）、$2 end（不含，半开区间）、$3 账号 id、
// $4 账号时区、$5 归属该账号的渠道 id 集合（legacy 兜底）。
//
// 请求数用 count(DISTINCT request_id)：重试/多次尝试共享同一个 request_id，
// 按行计数会把一次客户端请求算成多次。所有计数都**没有 status 过滤**
// （设计 §7.1）：失败请求的 token 上游可能已扣费；退款行既算 requests 也算
// refunded_requests，因此 items 表的 CHECK (refunded_requests <= requests)
// 恒可满足。status = 0（拒绝）同样计入 requests：它没有 token/费用，且
// requests 不参与 compare.go 的严重度规则，只影响展示。
//
// estimated_requests 刻意用 usage_source IN (2, 3) 而不是设计 §7.1 字面的
// `= 2`：注册表是 0=upstream_exact、1=upstream_partial、2=estimated、
// 3=reserved_default（contract/types.go），字面 2 会漏掉 reserved_default。
const aggregateQuery = `
	SELECT (created_at AT TIME ZONE $4)::date AS usage_date,
	       COALESCE(NULLIF(upstream_model, ''), model) AS model_key,
	       COUNT(DISTINCT request_id) AS requests,
	       COUNT(DISTINCT request_id) FILTER (WHERE usage_source IN (2, 3)) AS estimated_requests,
	       COUNT(DISTINCT request_id) FILTER (WHERE status = 3) AS refunded_requests,
	       COALESCE(SUM(cache_read_tokens), 0) AS hit_tokens,
	       COALESCE(SUM(CASE
	           WHEN usage_semantic IS NULL THEN 0
	           WHEN usage_semantic = 0 THEN GREATEST(prompt_tokens - cache_read_tokens, 0)
	           ELSE prompt_tokens END), 0) AS miss_tokens,
	       COALESCE(SUM(completion_tokens), 0) AS output_tokens,
	       COALESCE(SUM(cost) FILTER (WHERE other->>'billing_source' = 'wallet'), 0) AS wallet_credits,
	       COALESCE(SUM(cost) FILTER (WHERE other->>'billing_source' = 'subscription'), 0) AS subscription_credits,
	       COALESCE(SUM(cost), 0) AS cost_credits,
	       COUNT(DISTINCT request_id) FILTER (WHERE usage_semantic IS NULL) AS unknown_requests,
	       COUNT(DISTINCT request_id) FILTER (WHERE attempts > 1) AS multi_attempt_requests
	FROM usage_logs
	WHERE created_at >= $1 AND created_at < $2
	  AND (upstream_account_id = $3
	       OR (upstream_account_id IS NULL AND channel_id = ANY($5)))
	GROUP BY 1, 2`

// Aggregate 返回指定账号在 [start, end) 窗口内、按账号本地日期与模型聚合的用量。
//
// channelIDs 是主库里归属该账号的渠道集合，用于归属没有 upstream_account_id
// 的历史行；传 nil 时该分支不命中（`= ANY(NULL)` 为 NULL）。
func (r *UsageAggregateRepo) Aggregate(ctx context.Context, accountID int64, channelIDs []int64, tz string, start, end time.Time) ([]LocalUsage, error) {
	rows, err := r.db.QueryContext(ctx, aggregateQuery,
		start.UTC(), end.UTC(), accountID, tz, channelIDs)
	if err != nil {
		return nil, fmt.Errorf("store: aggregate usage for account %d: %w", accountID, err)
	}
	defer rows.Close()

	out := make([]LocalUsage, 0, 64)
	for rows.Next() {
		var (
			u         LocalUsage
			usageDate time.Time
		)
		if err := rows.Scan(&usageDate, &u.Model,
			&u.Requests, &u.EstimatedRequests, &u.RefundedRequests,
			&u.CacheHitTokens, &u.CacheMissTokens, &u.OutputTokens,
			&u.WalletCredits, &u.SubscriptionCredits, &u.CostCredits,
			&u.UnknownRequests, &u.MultiAttemptRequests); err != nil {
			return nil, fmt.Errorf("store: scan aggregated usage for account %d: %w", accountID, err)
		}
		u.UsageDate = usageDate.UTC()
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate aggregated usage for account %d: %w", accountID, err)
	}
	return out, nil
}

// LatestCreatedAt 返回最近两天内最新一条日志的时间（无数据时为 epoch）。
//
// runner 用它判断日志流是否滞后：与 now 相差超过阈值时把 run 标记为 partial，
// 而不是把「日志还没落库」误判成「本地漏记」。
const latestCreatedAtQuery = `SELECT COALESCE(max(created_at), 'epoch')
	FROM usage_logs WHERE created_at > now() - interval '2 days'`

// LatestCreatedAt 见 latestCreatedAtQuery。
func (r *UsageAggregateRepo) LatestCreatedAt(ctx context.Context) (time.Time, error) {
	var latest time.Time
	if err := r.db.QueryRowContext(ctx, latestCreatedAtQuery).Scan(&latest); err != nil {
		return time.Time{}, fmt.Errorf("store: latest usage log created_at: %w", err)
	}
	return latest.UTC(), nil
}
