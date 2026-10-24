package reconcile

// 本文件把日志库聚合仓储返回的 []store.LocalUsage 转换成按 (日期, 模型)
// 索引的 map[Key]LocalAggregate。
//
// 仓储 SQL 已经按 COALESCE(upstream_model, model) 分组合并，正常路径下每个
// 键只出现一次；这里仍然做一次累加，避免将来查询口径变化（例如按渠道分组）
// 时静默覆盖掉一半用量。函数全程无副作用，便于直接单测。

import (
	"time"

	"newtg/gateway/internal/store"
)

// Key 是本地聚合与上游快照共用的业务键：账号本地自然日 + 上游模型名。
type Key struct {
	UsageDate     time.Time
	UpstreamModel string
}

// dateOnly 把时间归一到「它自己时区里的日历日」的 UTC 零点。
//
// usage_date 在仓储层是 date 列（pgx 以 UTC 零点返回），调用方也可能传入
// 带账号时区的自然日零点；取 t.Date() 而不是 t.UTC().Date()，避免
// 2026-10-23T00:00+08:00 被错误归到 10-22。
func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// keyFor 构造规范化后的 Key；本地行与快照行必须经过同一个函数才能对齐。
func keyFor(date time.Time, model string) Key {
	return Key{UsageDate: dateOnly(date), UpstreamModel: model}
}

// AggregateLocal 把按 (日期, 模型) 聚合的本地用量折叠成 map[Key]LocalAggregate。
//
// 重复键的计数器相加；返回空 map（而不是 nil），调用方可直接索引。
func AggregateLocal(rows []store.LocalUsage) map[Key]LocalAggregate {
	out := make(map[Key]LocalAggregate, len(rows))
	for _, row := range rows {
		key := keyFor(row.UsageDate, row.Model)
		agg := out[key]
		agg.Requests += row.Requests
		agg.EstimatedRequests += row.EstimatedRequests
		agg.RefundedRequests += row.RefundedRequests
		agg.CacheHitTokens += row.CacheHitTokens
		agg.CacheMissTokens += row.CacheMissTokens
		agg.OutputTokens += row.OutputTokens
		agg.WalletCredits += row.WalletCredits
		agg.SubscriptionCredits += row.SubscriptionCredits
		agg.CostCredits += row.CostCredits
		agg.UnknownRequests += row.UnknownRequests
		agg.MultiAttemptRequests += row.MultiAttemptRequests
		out[key] = agg
	}
	return out
}
