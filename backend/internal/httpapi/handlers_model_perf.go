package httpapi

// F-05-08 模型性能指标看板（modules-05 §5.5 / UI §8）。
//
// 数据口径的硬边界（platform-05 §3.5）：看板只能读采集管道的聚合指标，
// **不得**用 usage_logs 聚合值伪造 TTFT / Uptime / Incidents / 分钟级数据。
// 管道未装配、查询失败或停摆时进入显式闸门态（pipeline.enabled=false），
// 所有指标返回 null，由前端渲染「采集未启用 / 不可用」。
//
// 本文件的 ModelPerfSource 是查询接缝：真正的指标库（VictoriaMetrics /
// ClickHouse / Mimir，M7 选型）由部署侧实现并注入；单元/集成测试用内存
// fake 验证分位、成功率、TPS、Uptime 与事故派生逻辑。usage_logs 只用于
// comparison（24h 成功率并列值），绝不是看板数据源。

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/perfmetrics"
)

const (
	// 直方图桶边界固定（platform-05 §3.5.2）：50/100/200/500/1000/2000/
	// 5000/10000/30000 ms + +Inf。查询层不得换桶，否则同一数据在不同版本
	// 之间不可比。领域常量与采集端共用 perfmetrics，避免两处复制后漂移。
	modelPerfHistogramBuckets = perfmetrics.HistogramBuckets
	// modelPerfInfinityMS 是 +Inf 桶的下界标记值。UI 显示 `>30s`，接口用
	// 30000 + over_30s 标志表达「下界」而不是伪造有限分位值。
	modelPerfInfinityMS = 30000.0
	// 成功率分母样本阈值：单分钟样本 <5 的分钟既不计可用也不计观测
	// （platform-05 §3.5.3 的 Uptime 推导规则）。
	modelPerfMinMinuteSamples = perfmetrics.MinMinuteSamples
	// 单分钟可用阈值（默认 90%，可标定）。
	modelPerfUptimeThreshold = perfmetrics.UptimeThreshold
	// 低样本提示阈值（UI §8.2）。
	modelPerfLowSampleSize = 20
	// 事故合并窗口与最短时长（platform-05 §3.5.3）。
	modelPerfIncidentMergeWindow = 5 * time.Minute
	modelPerfIncidentMinDuration = 5 * time.Minute
)

var modelPerfBucketBoundsMS = perfmetrics.BucketBoundsMS

// ModelPerfSource 是 platform-05 §3.5 聚合指标库的查询接缝（领域定义在
// perfmetrics，采集端与查询端共用同一套类型）。
//
// 实现必须返回「该窗口内」的桶快照（Gauge 语义，查询层只做求和，不对
// 单调计数器求 rate），并给出最近一次桶更新时间用于停摆判定。model 为空
// 表示全部模型。
//
// 直方图编码（固定 9 桶 + +Inf，查询层不得换桶）：
//   - LatencyBuckets[i] / TTFTBuckets[i] 是**非累计**计数，第 i 桶为
//     (bounds[i-1], bounds[i]]；i=0 为 [0, 50ms]；
//   - bounds = 50/100/200/500/1000/2000/5000/10000/30000 ms；
//   - **+Inf 桶（>30s）计数 = Count - ΣBuckets**，由查询层自行推导。
//     落在 +Inf 的分位必须返回 over_30s 下界标记，不得当作 30000ms 的
//     有限值；接口通过 *_over_30s 布尔字段表达。
//   - TTFT 只统计 stream=true 的样本（非流式不进该系列）。
//
// M7 接线要求（platform-05 §3.5.5）：实现方必须由部署配置
// PERF_METRICS_ENABLED 决定是否装配本 source（false → 闸门），并用
// PERF_METRICS_FLUSH_INTERVAL 作为陈旧度判定基准（3×周期无更新 → 闸门）。
type ModelPerfSource = perfmetrics.Source

// ModelPerfBucket 是一个 (model, bucket_start) 的绝对快照。ChannelID /
// Upstream 只用于 admin 的渠道/上游分解；普通查询可留空。
type ModelPerfBucket = perfmetrics.Bucket

// modelPerfWindow 描述查询窗口；粒度固定由 range 派生（UI §8.3）。
type modelPerfWindow struct {
	Range       string    `json:"range"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Granularity string    `json:"granularity"`
}

// modelPerfSummary 是 KPI 卡契约（UI §8.9）。无样本字段为 null，前端渲染
// `—` 而不是 0（空样本语义）。
type modelPerfSummary struct {
	TTFTp50        *float64 `json:"ttft_p50_ms"`
	TTFTp95        *float64 `json:"ttft_p95_ms"`
	LatencyP50     *float64 `json:"latency_p50_ms"`
	LatencyP95     *float64 `json:"latency_p95_ms"`
	SuccessRate    *float64 `json:"success_rate"`
	Refused        int64    `json:"refused"`
	Uptime         *float64 `json:"uptime"`
	Incidents      *int64   `json:"incidents"`
	TPS            *float64 `json:"tps"`
	SampleSize     *int64   `json:"sample_size"`
	TTFTP50Over30s bool     `json:"ttft_p50_over_30s"`
	TTFTP95Over30s bool     `json:"ttft_p95_over_30s"`
	LatP50Over30s  bool     `json:"latency_p50_over_30s"`
	LatP95Over30s  bool     `json:"latency_p95_over_30s"`
}

type modelPerfBucketPoint struct {
	Start          time.Time `json:"start"`
	Model          string    `json:"model"`
	Requests       int64     `json:"requests"`
	TTFTp50        *float64  `json:"ttft_p50_ms"`
	TTFTp95        *float64  `json:"ttft_p95_ms"`
	LatencyP50     *float64  `json:"latency_p50_ms"`
	LatencyP95     *float64  `json:"latency_p95_ms"`
	SuccessRate    *float64  `json:"success_rate"`
	Uptime         *float64  `json:"uptime"`
	TPS            *float64  `json:"tps"`
	TTFTP50Over30s bool      `json:"ttft_p50_over_30s"`
	TTFTP95Over30s bool      `json:"ttft_p95_over_30s"`
	LatP50Over30s  bool      `json:"latency_p50_over_30s"`
	LatP95Over30s  bool      `json:"latency_p95_over_30s"`
}

type modelPerfIncident struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	DurationSeconds int64     `json:"duration_seconds"`
	Severity        string    `json:"severity"`
	Reason          string    `json:"reason"`
}

type modelPerfModelRow struct {
	Model          string   `json:"model"`
	TTFTp50        *float64 `json:"ttft_p50_ms"`
	TTFTp95        *float64 `json:"ttft_p95_ms"`
	LatencyP50     *float64 `json:"latency_p50_ms"`
	LatencyP95     *float64 `json:"latency_p95_ms"`
	SuccessRate    *float64 `json:"success_rate"`
	Uptime         *float64 `json:"uptime"`
	Incidents      int64    `json:"incidents"`
	SampleSize     int64    `json:"sample_size"`
	Badge          string   `json:"badge"`
	TTFTP50Over30s bool     `json:"ttft_p50_over_30s"`
	TTFTP95Over30s bool     `json:"ttft_p95_over_30s"`
	LatP50Over30s  bool     `json:"latency_p50_over_30s"`
	LatP95Over30s  bool     `json:"latency_p95_over_30s"`
}

type modelPerfPipeline struct {
	Enabled   bool       `json:"enabled"`
	UpdatedAt *time.Time `json:"updated_at"`
	// Stale 表示管道最近 3×刷写周期内没有更新（停摆 → 闸门）。与 enabled
	// 同时为 false 时前端只显示「不可用」，不显示任何历史数字。
	Stale bool `json:"stale"`
	// Reason 是稳定、脱敏的不可用原因码（collection_disabled /
	// metrics_dsn_missing / metrics_storage_unavailable /
	// metrics_query_failed / pipeline_stalled / no_data）。正常返回时省略。
	Reason string `json:"reason,omitempty"`
}

type modelPerfComparison struct {
	PipelineSuccessRate  *float64 `json:"pipeline_success_rate"`
	UsageLogsSuccessRate *float64 `json:"usage_logs_success_rate"`
	Delta                *float64 `json:"delta"`
}

type modelPerfDimension struct {
	Dimension   string   `json:"dimension"`
	Key         string   `json:"key"`
	Requests    int64    `json:"requests"`
	SuccessRate *float64 `json:"success_rate"`
	LatencyP95  *float64 `json:"latency_p95_ms"`
	TTFTp95     *float64 `json:"ttft_p95_ms"`
	Uptime      *float64 `json:"uptime"`
	// p95 落进 +Inf 桶时值仍是 30000 下界；布尔位让前端显示「>30s」。
	LatP95Over30s  bool `json:"latency_p95_over_30s"`
	TTFTP95Over30s bool `json:"ttft_p95_over_30s"`
}

type modelPerfResponse struct {
	Window     modelPerfWindow        `json:"window"`
	Buckets    []modelPerfBucketPoint `json:"buckets"`
	Summary    modelPerfSummary       `json:"summary"`
	Incidents  []modelPerfIncident    `json:"incidents"`
	Models     []modelPerfModelRow    `json:"models"`
	Pipeline   modelPerfPipeline      `json:"pipeline"`
	Comparison *modelPerfComparison   `json:"comparison"`
	Dimensions []modelPerfDimension   `json:"dimensions,omitempty"`
}

// --- 时间窗 ---------------------------------------------------------------

func parseModelPerfWindow(q url.Values, now time.Time) (time.Time, time.Time, string, string, string) {
	raw := strings.TrimSpace(q.Get("range"))
	if raw == "" {
		raw = "24h"
	}
	var span time.Duration
	var granularity string
	switch raw {
	case "1h":
		span, granularity = time.Hour, "1m"
	case "24h":
		span, granularity = 24*time.Hour, "1m"
	case "7d":
		span, granularity = 7*24*time.Hour, "1h"
	case "30d":
		span, granularity = 30*24*time.Hour, "1h"
	default:
		return time.Time{}, time.Time{}, "", "", "range must be one of 1h, 24h, 7d, 30d"
	}
	end := now.UTC()
	return end.Add(-span), end, raw, granularity, ""
}

func modelPerfBucketWidth(granularity string) time.Duration {
	if granularity == "1h" {
		return time.Hour
	}
	return time.Minute
}

// --- 聚合 -----------------------------------------------------------------

type modelPerfAggregate struct {
	Success      int64
	Error        int64
	Refused      int64
	LatencyCount int64
	LatencySumMS float64
	Latency      [modelPerfHistogramBuckets]int64
	TTFTCount    int64
	TTFT         [modelPerfHistogramBuckets]int64
	Completion   int64
	Generation   float64
	Observed     float64
	Up           float64
}

func (a *modelPerfAggregate) add(b ModelPerfBucket) {
	a.Success += b.Success
	a.Error += b.Error
	a.Refused += b.Refused
	a.LatencyCount += b.LatencyCount
	a.LatencySumMS += b.LatencySumMS
	for i := 0; i < modelPerfHistogramBuckets; i++ {
		a.Latency[i] += b.LatencyBuckets[i]
		a.TTFT[i] += b.TTFTBuckets[i]
	}
	a.TTFTCount += b.TTFTCount
	a.Completion += b.CompletionTokens
	a.Generation += b.GenerationSeconds
	a.Observed += b.ObservedSeconds
	a.Up += b.UpSeconds
}

func (a *modelPerfAggregate) sampleSize() int64 { return a.Success + a.Error + a.Refused }

func (a *modelPerfAggregate) successRate() *float64 {
	denom := a.sampleSize()
	if denom <= 0 {
		return nil
	}
	value := float64(a.Success) / float64(denom)
	return &value
}

func (a *modelPerfAggregate) uptime() *float64 {
	if a.Observed <= 0 {
		return nil
	}
	value := a.Up / a.Observed
	return &value
}

func (a *modelPerfAggregate) tps() *float64 {
	if a.Generation <= 0 {
		return nil
	}
	value := float64(a.Completion) / a.Generation
	return &value
}

// quantileFromBuckets 按 platform-05 §3.5.3 的桶内线性插值计算分位。
// 桶列表是 le 上界：首桶 [0,50ms]，其后 (前一个 le, 当前 le]。
// 返回 (值, 是否落在 +Inf 桶, 是否有样本)。
func quantileFromBuckets(count int64, buckets [modelPerfHistogramBuckets]int64, q float64) (float64, bool, bool) {
	if count <= 0 {
		return 0, false, false
	}
	rank := q * float64(count)
	cumulative := int64(0)
	for i := 0; i < modelPerfHistogramBuckets; i++ {
		next := cumulative + buckets[i]
		if float64(next) >= rank && buckets[i] > 0 {
			lower := 0.0
			if i > 0 {
				lower = modelPerfBucketBoundsMS[i-1]
			}
			upper := modelPerfBucketBoundsMS[i]
			value := lower + (upper-lower)*(rank-float64(cumulative))/float64(buckets[i])
			return value, false, true
		}
		cumulative = next
	}
	// rank 落在 +Inf：返回下界标记而不是伪造有限值。
	return modelPerfInfinityMS, true, true
}

func (a *modelPerfAggregate) summary(incidentCount int64) modelPerfSummary {
	out := modelPerfSummary{Refused: a.Refused}
	if value, over, ok := quantileFromBuckets(a.TTFTCount, a.TTFT, 0.50); ok {
		out.TTFTp50 = &value
		out.TTFTP50Over30s = over
	}
	if value, over, ok := quantileFromBuckets(a.TTFTCount, a.TTFT, 0.95); ok {
		out.TTFTp95 = &value
		out.TTFTP95Over30s = over
	}
	if value, over, ok := quantileFromBuckets(a.LatencyCount, a.Latency, 0.50); ok {
		out.LatencyP50 = &value
		out.LatP50Over30s = over
	}
	if value, over, ok := quantileFromBuckets(a.LatencyCount, a.Latency, 0.95); ok {
		out.LatencyP95 = &value
		out.LatP95Over30s = over
	}
	out.SuccessRate = a.successRate()
	out.Uptime = a.uptime()
	out.TPS = a.tps()
	if sample := a.sampleSize(); sample > 0 {
		out.SampleSize = &sample
		// 有流量才有「事故数」：无流量时保持 null（不是 0），与 Uptime 的
		// 空值语义一致（UI §8.2 / §8.7）。
		out.Incidents = &incidentCount
	}
	return out
}

// --- 事故派生 -------------------------------------------------------------

type modelPerfBucketAgg struct {
	start time.Time
	agg   modelPerfAggregate
}

func sortedBucketAggs(buckets []ModelPerfBucket) []modelPerfBucketAgg {
	byStart := make(map[time.Time]*modelPerfAggregate)
	for _, bucket := range buckets {
		key := bucket.Start.UTC()
		agg, ok := byStart[key]
		if !ok {
			agg = &modelPerfAggregate{}
			byStart[key] = agg
		}
		agg.add(bucket)
	}
	starts := make([]time.Time, 0, len(byStart))
	for start := range byStart {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	out := make([]modelPerfBucketAgg, 0, len(starts))
	for _, start := range starts {
		out = append(out, modelPerfBucketAgg{start: start, agg: *byStart[start]})
	}
	return out
}

// deriveIncidents 把连续「不可用分钟」合并为事故段（查询期派生，不落表）。
// 可用判定先在求和后的分钟样本上做（≥5 样本且成功率 ≥90%），与副本数
// 无关；无流量/样本不足的分钟既不计可用也不计观测。
//
// 分两阶段：先收集连续坏分钟段，再按「间隔 <5 分钟合并」把相邻段并起来
// （platform-05 §3.5.3）。合并必须在整段收集完成后做，否则中间夹着的
// 1–4 个正常/无流量分钟会提前断段。
func deriveIncidents(buckets []ModelPerfBucket, granularity string) []modelPerfIncident {
	ordered := sortedBucketAggs(buckets)
	width := modelPerfBucketWidth(granularity)

	type span struct {
		start time.Time
		end   time.Time
		agg   modelPerfAggregate
	}
	spans := make([]span, 0)
	var current *span
	flush := func() {
		if current != nil {
			spans = append(spans, *current)
			current = nil
		}
	}
	for _, item := range ordered {
		samples := item.agg.sampleSize()
		if item.agg.Observed <= 0 || samples < modelPerfMinMinuteSamples {
			flush()
			continue
		}
		available := float64(item.agg.Success)/float64(samples) >= modelPerfUptimeThreshold
		if available {
			flush()
			continue
		}
		end := item.start.Add(width)
		if current == nil {
			current = &span{start: item.start, end: end, agg: item.agg}
			continue
		}
		current.end = end
		current.agg.merge(item.agg)
	}
	flush()

	merged := make([]span, 0, len(spans))
	for _, item := range spans {
		if len(merged) == 0 {
			merged = append(merged, item)
			continue
		}
		last := &merged[len(merged)-1]
		if item.start.Sub(last.end) < modelPerfIncidentMergeWindow {
			last.end = item.end
			last.agg.merge(item.agg)
			continue
		}
		merged = append(merged, item)
	}

	incidents := make([]modelPerfIncident, 0, len(merged))
	for _, item := range merged {
		duration := item.end.Sub(item.start)
		if duration < modelPerfIncidentMinDuration {
			continue
		}
		severity := "minor"
		if item.agg.Observed > 0 && item.agg.Up/item.agg.Observed < 0.5 {
			severity = "major"
		}
		incidents = append(incidents, modelPerfIncident{
			Start:           item.start,
			End:             item.end,
			DurationSeconds: int64(duration.Seconds()),
			Severity:        severity,
			Reason:          "availability_below_threshold",
		})
	}
	return incidents
}

// merge 把另一个聚合结果并入自身（事故段合并时使用）。
func (a *modelPerfAggregate) merge(other modelPerfAggregate) {
	a.Success += other.Success
	a.Error += other.Error
	a.Refused += other.Refused
	a.LatencyCount += other.LatencyCount
	a.LatencySumMS += other.LatencySumMS
	for i := 0; i < modelPerfHistogramBuckets; i++ {
		a.Latency[i] += other.Latency[i]
		a.TTFT[i] += other.TTFT[i]
	}
	a.TTFTCount += other.TTFTCount
	a.Completion += other.Completion
	a.Generation += other.Generation
	a.Observed += other.Observed
	a.Up += other.Up
}

// --- 权限与入口 -----------------------------------------------------------

func (h *UsageLogHandlers) handleModelPerf(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if rejectAdminOnlyParams(w, q, viewer.admin, "dimension") {
		return
	}
	dimension := strings.TrimSpace(q.Get("dimension"))
	if dimension != "" && dimension != "channel" && dimension != "upstream" {
		writeInvalidRequest(w, "dimension must be channel or upstream")
		return
	}
	model := strings.TrimSpace(q.Get("model"))
	if len(model) > 128 {
		writeInvalidRequest(w, "model must be at most 128 characters")
		return
	}
	start, end, rangeName, granularity, detail := parseModelPerfWindow(q, h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	window := modelPerfWindow{Range: rangeName, Start: start, End: end, Granularity: granularity}

	writeGate := func(reason string, stale bool, updatedAt *time.Time) {
		writeJSON(w, http.StatusOK, modelPerfResponse{
			Window:    window,
			Buckets:   []modelPerfBucketPoint{},
			Summary:   modelPerfSummary{},
			Incidents: []modelPerfIncident{},
			Models:    []modelPerfModelRow{},
			Pipeline: modelPerfPipeline{
				Enabled: false, UpdatedAt: updatedAt, Stale: stale, Reason: reason,
			},
		})
	}

	if h.perf == nil {
		// 采集管道未装配（PERF_METRICS_ENABLED=false）：显式闸门，绝不回落
		// usage_logs；reason 让前端区分「配置关闭」与「存储不可用」。
		writeGate(perfmetrics.ReasonDisabled, true, nil)
		return
	}
	buckets, updatedAt, err := h.perf.ModelPerfBuckets(r.Context(), start, end, granularity, model)
	if err != nil {
		h.log.Warn("model perf: metrics query failed", "error", err)
		writeGate(modelPerfUnavailableReason(err), true, nil)
		return
	}
	var updatedPtr *time.Time
	if !updatedAt.IsZero() {
		utc := updatedAt.UTC()
		updatedPtr = &utc
	}
	// 停摆判定：最近 3×刷写周期内无任何桶更新（platform-05 §3.5.5 条件 3）。
	// 从未产生过数据与「有数据但停摆」用不同 reason 区分。
	if updatedAt.IsZero() {
		writeGate(perfmetrics.ReasonNoData, true, updatedPtr)
		return
	}
	if h.now().Sub(updatedAt.UTC()) > 3*h.perfFlush {
		writeGate(perfmetrics.ReasonPipelineStalled, true, updatedPtr)
		return
	}

	// 分钟/小时桶按 (start, model) 输出序列。排序确定，便于前端稳定绘制。
	type bucketKey struct {
		start time.Time
		model string
	}
	perBucket := make(map[bucketKey]*modelPerfAggregate)
	perModel := make(map[string]*modelPerfAggregate)
	byModelBuckets := make(map[string][]ModelPerfBucket)
	overall := &modelPerfAggregate{}
	for _, bucket := range buckets {
		key := bucketKey{start: bucket.Start.UTC(), model: bucket.Model}
		if perBucket[key] == nil {
			perBucket[key] = &modelPerfAggregate{}
		}
		perBucket[key].add(bucket)
		if perModel[bucket.Model] == nil {
			perModel[bucket.Model] = &modelPerfAggregate{}
		}
		perModel[bucket.Model].add(bucket)
		byModelBuckets[bucket.Model] = append(byModelBuckets[bucket.Model], bucket)
		overall.add(bucket)
	}

	incidents := deriveIncidents(buckets, granularity)
	points := make([]modelPerfBucketPoint, 0, len(perBucket))
	keys := make([]bucketKey, 0, len(perBucket))
	for key := range perBucket {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].start.Equal(keys[j].start) {
			return keys[i].model < keys[j].model
		}
		return keys[i].start.Before(keys[j].start)
	})
	for _, key := range keys {
		agg := perBucket[key]
		point := modelPerfBucketPoint{Start: key.start, Model: key.model, Requests: agg.sampleSize()}
		if value, over, ok := quantileFromBuckets(agg.TTFTCount, agg.TTFT, 0.50); ok {
			point.TTFTp50 = &value
			point.TTFTP50Over30s = over
		}
		if value, over, ok := quantileFromBuckets(agg.TTFTCount, agg.TTFT, 0.95); ok {
			point.TTFTp95 = &value
			point.TTFTP95Over30s = over
		}
		if value, over, ok := quantileFromBuckets(agg.LatencyCount, agg.Latency, 0.50); ok {
			point.LatencyP50 = &value
			point.LatP50Over30s = over
		}
		if value, over, ok := quantileFromBuckets(agg.LatencyCount, agg.Latency, 0.95); ok {
			point.LatencyP95 = &value
			point.LatP95Over30s = over
		}
		point.SuccessRate = agg.successRate()
		point.Uptime = agg.uptime()
		point.TPS = agg.tps()
		points = append(points, point)
	}

	modelNames := make([]string, 0, len(perModel))
	for name := range perModel {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	modelRows := make([]modelPerfModelRow, 0, len(modelNames))
	for _, name := range modelNames {
		agg := perModel[name]
		modelIncidents := deriveIncidents(byModelBuckets[name], granularity)
		summary := agg.summary(int64(len(modelIncidents)))
		modelRows = append(modelRows, modelPerfModelRow{
			Model:          name,
			TTFTp50:        summary.TTFTp50,
			TTFTp95:        summary.TTFTp95,
			LatencyP50:     summary.LatencyP50,
			LatencyP95:     summary.LatencyP95,
			SuccessRate:    summary.SuccessRate,
			Uptime:         summary.Uptime,
			Incidents:      int64(len(modelIncidents)),
			SampleSize:     agg.sampleSize(),
			Badge:          modelPerfBadge(summary, modelPerfIncidentOngoing(modelIncidents, end, granularity)),
			TTFTP50Over30s: summary.TTFTP50Over30s,
			TTFTP95Over30s: summary.TTFTP95Over30s,
			LatP50Over30s:  summary.LatP50Over30s,
			LatP95Over30s:  summary.LatP95Over30s,
		})
	}

	response := modelPerfResponse{
		Window:    window,
		Buckets:   points,
		Summary:   overall.summary(int64(len(incidents))),
		Incidents: incidents,
		Models:    modelRows,
		Pipeline:  modelPerfPipeline{Enabled: true, UpdatedAt: updatedPtr},
	}
	if dimension != "" {
		response.Dimensions = buildModelPerfDimensions(buckets, dimension)
	}
	response.Comparison = h.modelPerfComparison(r, start, end, rangeName, granularity, model, overall)
	writeJSON(w, http.StatusOK, response)
}

// modelPerfUnavailableReason 把指标库错误收敛为稳定、脱敏的 reason 码。
// UnavailableError 携带明确原因（DSN 缺失/存储不可用）；其余查询错误统一
// 记为 metrics_query_failed，绝不把底层错误原文放进响应。
func modelPerfUnavailableReason(err error) string {
	var unavailable *perfmetrics.UnavailableError
	if errors.As(err, &unavailable) {
		if unavailable.Reason != "" {
			return unavailable.Reason
		}
		return perfmetrics.ReasonStorageUnavailable
	}
	return perfmetrics.ReasonQueryFailed
}

// modelPerfIncidentOngoing 报告最后一段事故是否「仍在进行」：最后一段的
// 结束时间落在窗口末端的最后一个桶内（没有观察到恢复）。UI §8.6 的
// incident 徽章要求覆盖这一状态，避免把进行中的故障显示成 degraded。
func modelPerfIncidentOngoing(incidents []modelPerfIncident, windowEnd time.Time, granularity string) bool {
	if len(incidents) == 0 {
		return false
	}
	last := incidents[len(incidents)-1]
	return !last.End.Before(windowEnd.Add(-modelPerfBucketWidth(granularity)))
}

// modelPerfBadge 按 UI §8.6 的阈值分档。分档只用于当前窗口；样本 <20 或
// 管道无数据时输出 no_data（前端据此不渲染徽章）。
func modelPerfBadge(summary modelPerfSummary, ongoing bool) string {
	if summary.SampleSize == nil || *summary.SampleSize < modelPerfLowSampleSize || summary.SuccessRate == nil {
		return "no_data"
	}
	success := *summary.SuccessRate
	// success_rate 是 0–1 的比例；阈值按 UI §8.6 的百分比换算。
	// Incident 优先级最高：成功率 <90% 或存在进行中事故。
	if success < 0.90 || ongoing {
		return "incident"
	}
	if success < 0.99 {
		return "degraded"
	}
	// Degraded 的 Uptime 区间是 [99%, 99.5%)：到 99.5% 才算健康。
	if summary.Uptime != nil && *summary.Uptime < 0.995 {
		return "degraded"
	}
	if summary.TTFTp95 != nil {
		if *summary.TTFTp95 > 3000 {
			return "slow"
		}
		if *summary.TTFTp95 <= 1000 && summary.Uptime != nil && *summary.Uptime >= 0.995 {
			return "fast"
		}
	}
	return "healthy"
}

// buildModelPerfDimensions 生成 admin-only 的渠道/上游分解。渠道 ID 不进入
// 指标的聚合标签（platform-05 §3.5.3 基数护栏），只在查询期按受限维度分组。
func buildModelPerfDimensions(buckets []ModelPerfBucket, dimension string) []modelPerfDimension {
	groups := make(map[string]*modelPerfAggregate)
	for _, bucket := range buckets {
		var key string
		if dimension == "channel" {
			if bucket.ChannelID == nil {
				continue
			}
			key = strconv.FormatInt(*bucket.ChannelID, 10)
		} else {
			if bucket.Upstream == nil || strings.TrimSpace(*bucket.Upstream) == "" {
				continue
			}
			key = *bucket.Upstream
		}
		if groups[key] == nil {
			groups[key] = &modelPerfAggregate{}
		}
		groups[key].add(bucket)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := groups[keys[i]].sampleSize(), groups[keys[j]].sampleSize()
		if left == right {
			return keys[i] < keys[j]
		}
		return left > right
	})
	out := make([]modelPerfDimension, 0, len(keys))
	for _, key := range keys {
		agg := groups[key]
		item := modelPerfDimension{
			Dimension:   dimension,
			Key:         key,
			Requests:    agg.sampleSize(),
			SuccessRate: agg.successRate(),
			Uptime:      agg.uptime(),
		}
		if value, over, ok := quantileFromBuckets(agg.LatencyCount, agg.Latency, 0.95); ok {
			item.LatencyP95 = &value
			item.LatP95Over30s = over
		}
		if value, over, ok := quantileFromBuckets(agg.TTFTCount, agg.TTFT, 0.95); ok {
			item.TTFTp95 = &value
			item.TTFTP95Over30s = over
		}
		out = append(out, item)
	}
	return out
}

// modelPerfComparison 返回同一 24h 窗口的管道与 usage_logs 成功率并列值。
// 管道值不可得（闸门已在外层处理）或 usage_logs 无样本时返回 null；绝不
// 静默取舍两个口径（UI §8.2）。
func (h *UsageLogHandlers) modelPerfComparison(
	r *http.Request, start, end time.Time, rangeName, granularity, model string, overall *modelPerfAggregate,
) *modelPerfComparison {
	compStart, compEnd := start, end
	var pipeline *float64
	if rangeName == "24h" {
		pipeline = overall.successRate()
	} else if h.perf != nil {
		windowEnd := h.now().UTC()
		windowStart := windowEnd.Add(-24 * time.Hour)
		buckets, _, err := h.perf.ModelPerfBuckets(r.Context(), windowStart, windowEnd, "1m", model)
		if err != nil {
			return nil
		}
		agg := &modelPerfAggregate{}
		for _, bucket := range buckets {
			agg.add(bucket)
		}
		pipeline = agg.successRate()
		compStart, compEnd = windowStart, windowEnd
	}
	if pipeline == nil {
		return nil
	}

	where := "created_at >= $1 AND created_at < $2"
	args := []any{compStart, compEnd}
	if model != "" {
		where += " AND model = $3"
		args = append(args, model)
	}
	var success, total int64
	if err := h.logDB.QueryRowContext(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END), 0), count(*)
		FROM usage_logs WHERE `+where, args...).Scan(&success, &total); err != nil {
		h.log.Warn("model perf: usage_logs comparison query failed", "error", err)
		return nil
	}
	if total == 0 {
		return nil
	}
	usageLogs := float64(success) / float64(total)
	delta := *pipeline - usageLogs
	return &modelPerfComparison{
		PipelineSuccessRate:  pipeline,
		UsageLogsSuccessRate: &usageLogs,
		Delta:                &delta,
	}
}
