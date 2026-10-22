// Package perfmetrics 实现 platform-05 §3.5 的模型性能指标采集管道：
// 中继完成点按请求恰好一次采样 → 进程内 1 分钟绝对快照 → 按
// PERF_METRICS_FLUSH_INTERVAL 批量写入指标存储 → 供 GET /api/model-perf
// 服务端查询。
//
// 【口径边界】本包只承载**监控口径**。它不是计费真源；计费与对账仍然只认
// usage_logs / wallet_transactions。成功率分母是完成时三态
// (success + error + refused)，refunded 不在采样口径内。
//
// 【存储】生产 Store 是 PERF_METRICS_DSN 指向的**独立 PostgreSQL 指标库**
// （M7 指标库产品确定前的过渡实现），不落主库、也不落日志库；M7 产品替换时
// 只需实现 Store/RetentionStore 接缝。
//
// 【为什么定义在这里而不是 httpapi】采集端（billing）与存储端（store）都
// 需要同一套桶边界/状态枚举；把领域类型放在叶子包，httpapi 用别名引用，
// 避免三处各复制一份桶边界后悄悄漂移。
package perfmetrics

import (
	"context"
	"time"
)

const (
	// HistogramBuckets 是固定直方图桶数（platform-05 §3.5.2）。
	// 桶边界一经发布不得在查询层更换，否则同一数据跨版本不可比。
	HistogramBuckets = 9

	// MinuteSeconds 是分钟桶的观测秒数；小时桶由分钟桶累加。
	MinuteSeconds = 60

	// MinMinuteSamples 是「可用分钟」判定的最小样本数。先在汇总计数上判定，
	// 不在副本级先判 up（platform-05 §3.5.3）。
	MinMinuteSamples = 5

	// UptimeThreshold 是可用分钟的成功率阈值（默认 90%，可标定）。
	UptimeThreshold = 0.9
)

// BucketBoundsMS 是固定桶边界（le 上界），单位毫秒：
// 50/100/200/500/1000/2000/5000/10000/30000 + +Inf。
// +Inf 桶不在这里表示；计数 = Count - ΣBuckets，由查询层推导。
var BucketBoundsMS = [HistogramBuckets]float64{
	50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000,
}

// Status 是完成时三态。数值刻意与 usage_logs.status 的 0/1/2 对齐，
// 这样 billing 侧无需再做一层映射（refunded=3 由调用方跳过）。
type Status uint8

const (
	StatusRefused Status = 0
	StatusSuccess Status = 1
	StatusError   Status = 2
)

// Bucket 是一个 (model, bucket_start) 的绝对快照，也是 /api/model-perf 的
// 查询结果单元。ChannelID / Upstream 预留给 admin 维度分解；当前采集管道
// 不把渠道写入聚合键（platform-05 §3.5.3 的基数护栏），查询层留空。
type Bucket struct {
	Model     string
	Start     time.Time
	ChannelID *int64
	Upstream  *string

	// 完成时三态；refunded 不进入采样。
	Success int64
	Error   int64
	Refused int64

	// 延迟直方图：每桶非累计计数（不是 Prometheus 累计桶）。
	LatencyCount   int64
	LatencySumMS   float64
	LatencyBuckets [HistogramBuckets]int64

	// TTFT 直方图：仅流式且 TTFT 有效的样本进入。
	TTFTCount   int64
	TTFTBuckets [HistogramBuckets]int64

	CompletionTokens  int64
	GenerationSeconds float64
	ObservedSeconds   float64
	UpSeconds         float64
}

// Source 是 platform-05 §3.5 聚合指标库的查询接缝。
//
// 实现必须返回「该窗口内」的桶快照（Gauge 语义，查询层只做求和），并给出
// 最近一次桶更新时间用于停摆判定。model 为空表示全部模型。
type Source interface {
	ModelPerfBuckets(ctx context.Context, start, end time.Time, granularity, model string) ([]Bucket, time.Time, error)
}

// Snapshot 是一次 flush 要写入的绝对快照。
//
// 【幂等】同一 (BucketStart, Model, Node, Epoch) 的更新靠 Seq 单调递增 +
// 存储侧条件 upsert 实现；重复写入同一 Seq 必须是 no-op，而不是增量累加。
type Snapshot struct {
	BucketStart time.Time
	Model       string
	Node        string
	Epoch       string
	Seq         uint64

	Success int64
	Error   int64
	Refused int64

	LatencyCount   int64
	LatencySumMS   float64
	LatencyBuckets [HistogramBuckets]int64

	TTFTCount   int64
	TTFTBuckets [HistogramBuckets]int64

	CompletionTokens  int64
	GenerationSeconds float64
}

// Store 是采集管道的持久化接缝：既是写入端（UpsertBuckets），也是读端
// （ModelPerfBuckets）。生产实现是 store.ModelPerfRepo（M7 可替换为
// VictoriaMetrics / ClickHouse / Mimir 的实现）。
type Store interface {
	UpsertBuckets(ctx context.Context, snapshots []Snapshot) error
	ModelPerfBuckets(ctx context.Context, start, end time.Time, granularity, model string) ([]Bucket, time.Time, error)
}

// 读面不可用原因。它们是**稳定、脱敏**的机器可读码，直接放进
// /api/model-perf 的 pipeline.reason；绝不携带 DSN/口令/内部错误原文。
const (
	// ReasonDisabled 表示 PERF_METRICS_ENABLED=false（部署配置关闭）。
	ReasonDisabled = "collection_disabled"
	// ReasonDSNMissing 表示启用采集但未配置 PERF_METRICS_DSN。
	ReasonDSNMissing = "metrics_dsn_missing"
	// ReasonStorageUnavailable 表示指标库连接/查询不可用。
	ReasonStorageUnavailable = "metrics_storage_unavailable"
	// ReasonQueryFailed 表示指标库查询失败（非连接类）。
	ReasonQueryFailed = "metrics_query_failed"
	// ReasonPipelineStalled 表示连续 3 个周期无桶更新或刷写失败。
	ReasonPipelineStalled = "pipeline_stalled"
	// ReasonNoData 表示采集管道已启用但尚未产生任何桶（例如刚启用）。
	ReasonNoData = "no_data"
)

// UnavailableError 是「指标库不可用」的脱敏错误。Reason 是上表中的稳定码；
// Error() 不拼接底层错误，避免把 DSN/主机信息带进响应或日志摘要。
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string {
	if e == nil || e.Reason == "" {
		return "perfmetrics: metrics storage unavailable"
	}
	return "perfmetrics: metrics storage unavailable: " + e.Reason
}

// UnavailableSource 是 fail-closed 的查询接缝：启用采集但 DSN 缺失/连不上时
// 注入它，让 /api/model-perf 明确返回不可用原因，而不是伪造数字或回退到
// 日志库/usage_logs。
type UnavailableSource struct {
	reason string
}

// NewUnavailableSource 构造不可用 source；reason 为空时回落到
// ReasonStorageUnavailable。
func NewUnavailableSource(reason string) *UnavailableSource {
	if reason == "" {
		reason = ReasonStorageUnavailable
	}
	return &UnavailableSource{reason: reason}
}

// ModelPerfBuckets 恒返回 UnavailableError；不读取任何业务库。
func (s *UnavailableSource) ModelPerfBuckets(context.Context, time.Time, time.Time, string, string) ([]Bucket, time.Time, error) {
	reason := ReasonStorageUnavailable
	if s != nil && s.reason != "" {
		reason = s.reason
	}
	return nil, time.Time{}, &UnavailableError{Reason: reason}
}

// MinuteRetention 是分钟桶的固定保留期（platform-05 §3.5.4）。小时桶保留期
// 由 PERF_METRICS_RETENTION_DAYS（30–400，默认 400）配置。
const MinuteRetention = 7 * 24 * time.Hour

// RetentionOptions 是一次指标库保留清理的输入（Now 供测试注入）。
type RetentionOptions struct {
	Now             time.Time
	MinuteRetention time.Duration
	HourRetention   time.Duration
}

// RetentionResult 是一次保留清理的结果（Locked=false 表示本副本未拿到
// 跨进程咨询锁，已跳过）。
type RetentionResult struct {
	HoursMaterialized int64
	MinutesDeleted    int64
	HoursDeleted      int64
	Locked            bool
}

// RetentionStore 是可选接缝：生产 PostgreSQL 指标库实现分钟→小时物化与
// 过期清理；若未来替换为自带 retention 的产品（VictoriaMetrics 等），
// 实现方可以只实现 Store，Collector 会自动跳过本接缝。
type RetentionStore interface {
	RunRetention(ctx context.Context, opts RetentionOptions) (RetentionResult, error)
}

// Observation 是中继完成点的一次采样。
//
// LatencyMS / FirstTokenMS 用指针区分「未知」和 0：非流式请求的 TTFT 必须
// 是 nil（平台-01 §12），不能写 0 冒充「首 token 立即到达」。
// ObservedAt 取结算完成时刻，桶按 UTC 对齐。
type Observation struct {
	Model            string
	Status           Status
	Stream           bool
	LatencyMS        *int
	FirstTokenMS     *int
	CompletionTokens int64
	ObservedAt       time.Time
}

// BucketIndex 返回 ms 落在的有限桶下标；落在 +Inf 时返回 -1。
// 序号 i 对应桶 (bounds[i-1], bounds[i]]，i=0 为 [0, 50ms]。
func BucketIndex(ms int) int {
	if ms < 0 {
		return -1
	}
	for i, bound := range BucketBoundsMS {
		if float64(ms) <= bound {
			return i
		}
	}
	return -1
}
