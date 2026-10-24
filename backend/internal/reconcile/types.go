// Package reconcile 实现上游用量对账的纯领域逻辑：金额换算、日界计算与
// 本地/控制台快照的差异比较。
//
// 本包不接触数据库、HTTP 或任何其它新包，只做确定性的整数运算，便于被
// store、runner 与 handler 复用，也便于单测穷举边界。
//
// 约定：
//   - 金额一律使用整数最小单位（分，int64），包内不出现任何浮点运算；
//   - 时间戳使用 UTC，但「一天」的边界由账号时区决定（见 DayWindow）；
//   - 快照的指针字段为 nil 表示「控制台没有提供该维度」，与明确的 0 不同。
package reconcile

import "time"

// Severity 是对账条目的严重程度，取值与数据库 CHECK 约束保持一致。
type Severity string

const (
	// SeverityOK 表示所有可比维度都在容差内。
	SeverityOK Severity = "ok"
	// SeverityWarn 表示存在超出容差的差异，但未达到告警门槛，或存在语义未知的请求。
	SeverityWarn Severity = "warn"
	// SeverityAlert 表示真实且显著的差异（token 差异率与绝对量同时超阈值）。
	SeverityAlert Severity = "alert"
	// SeverityPartialSnapshot 表示快照缺少部分维度，只能比较已提供的维度。
	SeverityPartialSnapshot Severity = "partial_snapshot"
	// SeverityMissingSnapshot 表示有本地数据但没有控制台快照（待录入，不算告警）。
	SeverityMissingSnapshot Severity = "missing_snapshot"
	// SeverityMissingLocal 表示有快照但没有本地数据（可能漏记或渠道未归属）。
	SeverityMissingLocal Severity = "missing_local"
	// SeverityLocalExpired 表示该日期已超出本地日志保留窗口，本地数据被有意删除。
	SeverityLocalExpired Severity = "local_expired"
)

// Tolerances 是对账的容差配置。
type Tolerances struct {
	// TokenBP 是 token 差异率阈值，单位为万分之一（bp）。
	TokenBP int
	// MinDeltaTokens 是触发 alert 所需的绝对 token 差异下限，用于过滤小基数噪声。
	MinDeltaTokens int64
	// AmountMinor 是金额差异阈值，单位为分。
	AmountMinor int64
}

// LocalAggregate 是本地 usage_logs 在某个账号、某个自然日内的聚合值。
type LocalAggregate struct {
	Requests, EstimatedRequests, RefundedRequests   int64
	CacheHitTokens, CacheMissTokens, OutputTokens   int64
	WalletCredits, SubscriptionCredits, CostCredits int64
	UnknownRequests, MultiAttemptRequests           int64
}

// Snapshot 是上游控制台录入的当日快照。指针为 nil 表示控制台未提供该维度。
type Snapshot struct {
	CacheHitTokens, CacheMissTokens, CacheWriteTokens, OutputTokens *int64
	Requests, AmountMinor                                           *int64
}

// Input 是一次对账比较的输入。
type Input struct {
	UsageDate     time.Time
	UpstreamModel string
	Local         *LocalAggregate
	Snapshot      *Snapshot
	Tolerances    Tolerances
	QuotaPerUnit  int64
}

// Comparison 是一次对账比较的结果。可空字段为 nil 表示该维度不可比较。
type Comparison struct {
	DeltaCacheHitTokens, DeltaCacheMissTokens, DeltaOutputTokens *int64
	DeltaTokenBP                                                 *int64
	SnapshotPartial                                              bool
	RevenueMinor                                                 int64
	Severity                                                     Severity
}
