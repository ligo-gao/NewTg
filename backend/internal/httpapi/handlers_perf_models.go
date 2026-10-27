// 性能页模型汇总表：GET /api/performance/models（spec §7.3）。
//
//	GET /api/performance/models   settings:manage（只读）
//
// 一次返回表格全部行，避免逐行请求。两类数据必须分开标注，否则同一屏会
// 出现两个互相矛盾的「p95」：
//   - 线上流量（perfmetrics 分钟桶，真实中继请求）：TTFT p50/p95、端到端
//     p95、TPS、samples、spark；
//   - 主动测速（model_performance_tests 最新一行）：probe.* 与 uptime。
//
// 一句话口径：数字看线上，可用性看探针。本文件只读，不用 usage_logs 聚合
// 值伪造分位，也不把稀疏的探针样本混进延迟分位。
//
// 状态判定顺序严格为 incident → degraded → no_sample → normal（spec §7.3
// 的状态表），阈值写成包内常量，前端不重复实现判定逻辑。
package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"newtg/gateway/internal/perfmetrics"
	"newtg/gateway/internal/store"
)

const (
	// 状态词表（spec §7.3 / Task 12 前端契约），不得自创同义词。
	perfModelStatusNormal   = "normal"
	perfModelStatusDegraded = "degraded"
	perfModelStatusIncident = "incident"
	perfModelStatusNoSample = "no_sample"

	// sources[].name（沿用 Task 8 冻结的 {name,status} 形状）。
	perfSourcePerfmetrics = "perfmetrics"
	perfSourceProbes      = "probes"

	// perfModelWindow 是表格窗口：spec §7.3 固定 24h（前端不传 range），
	// 24 小时窗口一律读分钟桶。
	perfModelWindow = 24 * time.Hour
	// perfModelBaselineWindow 是 degraded 判定的基线窗口（近 7 天小时桶）。
	perfModelBaselineWindow = 7 * 24 * time.Hour
	// perfModelDegradedFactor：24h p95 超过近 7 天小时 p95 中位数的 N 倍判
	// degraded。
	perfModelDegradedFactor = 2.0
	// perfModelIncidentSuccessRate：窗口内中继请求成功率低于该值判 incident。
	perfModelIncidentSuccessRate = 0.9
	// perfModelProbeStaleAfter：最近一次测速距今超过该时长判 degraded。
	perfModelProbeStaleAfter = 36 * time.Hour
	// perfModelSparkPoints 是 spark 固定点数：24h 窗口的分钟桶按小时聚合后
	// 恰好 24 点，与页面 sparkline 的点数一致。
	perfModelSparkPoints = 24
)

// ChannelModelCatalog 是性能页 models 端点的渠道目录读取接缝。
//
// 生产实现是 store.ChannelRepo；接口在这里声明而不是直接依赖具体仓储，
// 是因为 handler 单测只需要「启用模型并集」与「每个模型的可路由渠道数」
// 两个只读查询，不应该为它们引入数据库。
type ChannelModelCatalog interface {
	// EnabledModels 返回所有启用渠道对外宣告的模型并集（含映射别名）。
	EnabledModels(ctx context.Context) ([]string, error)
	// CountRoutableChannels 返回每个模型当前可路由的渠道数。模型不在 map
	// 中表示「未知」，与「0 个渠道」必须能区分（响应里分别是 null 与 0）。
	CountRoutableChannels(ctx context.Context) (map[string]int, error)
}

// ---------------------------------------------------------------------------
// 响应形状（spec §7.3 + 本计划裁定：provider → channels）
// ---------------------------------------------------------------------------

type perfModelsResponse struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Sources     []perfSource      `json:"sources"`
	Rows        []perfModelRow    `json:"rows"`
	Summary     perfModelsSummary `json:"summary"`
}

type perfModelsSummary struct {
	Models   int `json:"models"`
	Degraded int `json:"degraded"`
	Incident int `json:"incident"`
	NoSample int `json:"no_sample"`
}

// perfModelRow 是一行模型汇总。流量列（ttft_*/latency_p95_ms/tps/samples/
// success_rate/spark）来自 perfmetrics，探针列（probe/uptime）来自
// model_performance_tests；任一数据源不可用时只让对应列变 null，端点仍 200。
type perfModelRow struct {
	Model       string          `json:"model"`
	Channels    *int            `json:"channels"`
	TTFTp50     *float64        `json:"ttft_p50_ms"`
	TTFTp95     *float64        `json:"ttft_p95_ms"`
	LatencyP95  *float64        `json:"latency_p95_ms"`
	TPS         *float64        `json:"tps"`
	Samples     int64           `json:"samples"`
	SuccessRate *float64        `json:"success_rate"`
	Probe       *perfModelProbe `json:"probe"`
	Uptime      *float64        `json:"uptime"`
	Status      string          `json:"status"`
	// Spark 长度恒为 24：小时桶没有样本的位置是 null（不能用 0 冒充，
	// 曲线会掉到零）。
	Spark []*float64 `json:"spark"`
}

// perfModelProbe 是最近一次测速的展示面。空值字段用指针表达 null。
type perfModelProbe struct {
	LastTestedAt *time.Time `json:"last_tested_at"`
	// OK 表示最近一轮测速是否全部成功（success_count == runs）。部分成功
	// 为 false，但不等于 incident——incident 只认「全失败」。
	OK          bool    `json:"ok"`
	Runs        int     `json:"runs"`
	TTFTp95     *int    `json:"ttft_p95_ms"`
	LatencyP95  *int    `json:"latency_p95_ms"`
	SuccessRate float64 `json:"success_rate"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func (h *PerformanceHandlers) handleModels(w http.ResponseWriter, r *http.Request) {
	now := h.now().UTC()
	ctx := r.Context()

	sources := []perfSource{
		{Name: perfSourcePerfmetrics, Status: perfStatusOK},
		{Name: perfSourceProbes, Status: perfStatusOK},
	}

	var (
		minuteBuckets []ModelPerfBucket
		hourBuckets   []ModelPerfBucket
		perfErr       error
	)
	if h.perf == nil {
		// 采集管道未装配：端点仍 200，流量列全部为 null（探针行照常展示），
		// 数据源状态告诉前端为什么这些列是空的。
		sources[0].Status = perfSourceUnavailable
	} else {
		minuteBuckets, _, perfErr = h.perf.ModelPerfBuckets(
			ctx, now.Add(-perfModelWindow), now, "1m", "")
		if perfErr == nil {
			hourBuckets, _, perfErr = h.perf.ModelPerfBuckets(
				ctx, now.Add(-perfModelBaselineWindow), now, "1h", "")
		}
		if perfErr != nil {
			sources[0].Status = perfSourceStatusForError(perfErr)
			h.log.Warn("performance: models metrics query failed", "err", perfErr)
		}
	}

	if perfErr != nil {
		// 指标库故障/不可用 → 显式空态（spec §7.3）：不拿探针数据拼半张表，
		// 前端按 rows 为空 + 数据源状态显示「监控不可用」。两份数据源都标
		// unavailable，因为本轮的探针读取也被跳过。
		sources[1].Status = perfSourceUnavailable
		writeJSON(w, http.StatusOK, perfModelsResponse{
			GeneratedAt: now,
			Sources:     sources,
			Rows:        []perfModelRow{},
			Summary:     perfModelsSummary{},
		})
		return
	}

	minuteByModel := groupModelPerfBuckets(minuteBuckets)
	hourByModel := groupModelPerfHourly(hourBuckets)

	// 模型集合 = 24h 流量桶模型 ∪ EnabledModels ∪ 探针行模型。EnabledModels
	// 失败只记 Warn：集合退化为其余两源，而不是整表 500。
	modelSet := make(map[string]struct{}, len(minuteByModel))
	for model := range minuteByModel {
		if model != "" {
			modelSet[model] = struct{}{}
		}
	}
	if h.channels != nil {
		enabled, err := h.channels.EnabledModels(ctx)
		if err != nil {
			h.log.Warn("performance: enabled models query failed", "err", err)
		} else {
			for _, model := range enabled {
				if model = strings.TrimSpace(model); model != "" {
					modelSet[model] = struct{}{}
				}
			}
		}
	}

	// LatestByModels 需要先有模型集（桶 ∪ 启用模型）；返回的探针行本身
	// 再并入模型集，覆盖「渠道已删、测速记录还在」的残留模型。
	probes := map[string]store.ModelPerformanceTest{}
	if h.probes != nil {
		seed := sortedModelNames(modelSet)
		latest, err := h.probes.LatestByModels(ctx, seed)
		if err != nil {
			sources[1].Status = perfSourceError
			h.log.Warn("performance: latest probe query failed", "err", err)
		} else {
			probes = latest
			for model := range probes {
				if model != "" {
					modelSet[model] = struct{}{}
				}
			}
		}
	} else {
		sources[1].Status = perfSourceUnavailable
	}

	// 渠道数失败只记 Warn，整列 null：不能把「查询失败」当成「0 个渠道」。
	var channelCounts map[string]int
	if h.channels != nil {
		counts, err := h.channels.CountRoutableChannels(ctx)
		if err != nil {
			h.log.Warn("performance: routable channel counts query failed", "err", err)
		} else {
			channelCounts = counts
		}
	}

	models := sortedModelNames(modelSet)
	rows := make([]perfModelRow, 0, len(models))
	summary := perfModelsSummary{Models: len(models)}
	for _, model := range models {
		var probe *store.ModelPerformanceTest
		if latest, ok := probes[model]; ok {
			probeCopy := latest
			probe = &probeCopy
		}
		row := buildPerfModelRow(model, minuteByModel[model], hourByModel[model], probe, channelCounts, now)
		switch row.Status {
		case perfModelStatusDegraded:
			summary.Degraded++
		case perfModelStatusIncident:
			summary.Incident++
		case perfModelStatusNoSample:
			summary.NoSample++
		}
		rows = append(rows, row)
	}

	writeJSON(w, http.StatusOK, perfModelsResponse{
		GeneratedAt: now,
		Sources:     sources,
		Rows:        rows,
		Summary:     summary,
	})
}

// ---------------------------------------------------------------------------
// 行组装与状态判定
// ---------------------------------------------------------------------------

func buildPerfModelRow(
	model string,
	minuteBuckets []ModelPerfBucket,
	hours map[time.Time]*modelPerfAggregate,
	probe *store.ModelPerformanceTest,
	channelCounts map[string]int,
	now time.Time,
) perfModelRow {
	agg := &modelPerfAggregate{}
	for _, bucket := range minuteBuckets {
		agg.add(bucket)
	}

	row := perfModelRow{
		Model:       model,
		Samples:     agg.sampleSize(),
		SuccessRate: perfFiniteFloatPtr(agg.successRate()),
		TPS:         perfFiniteFloatPtr(agg.tps()),
		Spark:       perfModelSpark(minuteBuckets, now),
	}
	if value, _, ok := quantileFromBuckets(agg.TTFTCount, agg.TTFT, 0.50); ok {
		row.TTFTp50 = perfFinitePtr(value)
	}
	if value, _, ok := quantileFromBuckets(agg.TTFTCount, agg.TTFT, 0.95); ok {
		row.TTFTp95 = perfFinitePtr(value)
	}
	if value, _, ok := quantileFromBuckets(agg.LatencyCount, agg.Latency, 0.95); ok {
		row.LatencyP95 = perfFinitePtr(value)
	}

	// 只有知道才输出数字：模型不在 map 里（查询失败或没有启用渠道）时
	// channels=null，而不是 0。
	if count, ok := channelCounts[model]; ok {
		value := count
		row.Channels = &value
	}

	if probe != nil {
		createdAt := probe.CreatedAt.UTC()
		successRate := perfFiniteValue(probe.SuccessRate)
		row.Probe = &perfModelProbe{
			LastTestedAt: &createdAt,
			OK:           probe.Runs > 0 && probe.SuccessCount >= probe.Runs,
			Runs:         probe.Runs,
			TTFTp95:      probe.TTFTMSP95,
			LatencyP95:   probe.LatencyMSP95,
			SuccessRate:  successRate,
		}
		// 裁决：uptime 取最近一次测速的成功率。LatestByModels 只给最新一行，
		// 做不到 spec 原文的「24h 加权」；Task 11 会在 OpenAPI 写成「最近一次
		// 测速成功率」，这里不为了凑「加权」造一个假值。
		value := successRate
		row.Uptime = &value
	}

	row.Status = perfModelStatus(
		agg.sampleSize() > 0, row.SuccessRate, row.LatencyP95, perfModelBaselineP95(hours), probe, now)
	return row
}

// perfModelStatus 按 spec §7.3 的表判定状态，顺序严格为
// incident → degraded → no_sample → normal：更严重的状态先返回，避免一个
// 同时「全失败」且「很久没测」的模型被显示成 degraded。
func perfModelStatus(
	hasTraffic bool,
	successRate, p95, baselineP95 *float64,
	probe *store.ModelPerformanceTest,
	now time.Time,
) string {
	if probe != nil && probe.Runs > 0 && perfFiniteValue(probe.SuccessRate) <= 0 {
		return perfModelStatusIncident
	}
	if successRate != nil && *successRate < perfModelIncidentSuccessRate {
		return perfModelStatusIncident
	}
	if p95 != nil && baselineP95 != nil && *baselineP95 > 0 &&
		*p95 > perfModelDegradedFactor**baselineP95 {
		return perfModelStatusDegraded
	}
	if probe != nil && now.Sub(probe.CreatedAt.UTC()) > perfModelProbeStaleAfter {
		return perfModelStatusDegraded
	}
	// 两类数据同时缺席才叫「没有数据」；流量少但探针正常的模型不因此变成
	// no_sample（spec §7.3 一句话口径）。
	if !hasTraffic && probe == nil {
		return perfModelStatusNoSample
	}
	return perfModelStatusNormal
}

// perfModelBaselineP95 把 7 天小时桶逐小时算 p95，再取非空小时的中位数。
// 没有任何非空小时时返回 nil（不判 degraded）。中位数的口径：奇数取中间
// 值，偶数取中间两个的平均值。
func perfModelBaselineP95(hours map[time.Time]*modelPerfAggregate) *float64 {
	values := make([]float64, 0, len(hours))
	for _, agg := range hours {
		if value, _, ok := quantileFromBuckets(agg.LatencyCount, agg.Latency, 0.95); ok {
			values = append(values, value)
		}
	}
	return medianFloat(values)
}

// perfModelSpark 把 24h 分钟桶按小时聚合为固定 24 点的 p95 序列。
//
// 24 个位置锚定到当前小时（最后一个点是当前这个未满的小时），最早的边界
// 部分小时被丢弃——那是最旧的数据，比丢掉刚刚发生的一小时更可接受。
// 没有样本的小时保持 nil，JSON 里是 null。
func perfModelSpark(buckets []ModelPerfBucket, now time.Time) []*float64 {
	byHour := make(map[time.Time]*modelPerfAggregate, perfModelSparkPoints)
	for _, bucket := range buckets {
		hour := bucket.Start.UTC().Truncate(time.Hour)
		agg := byHour[hour]
		if agg == nil {
			agg = &modelPerfAggregate{}
			byHour[hour] = agg
		}
		agg.add(bucket)
	}

	points := make([]*float64, perfModelSparkPoints)
	lastHour := now.Truncate(time.Hour)
	for i := range points {
		hour := lastHour.Add(-time.Duration(perfModelSparkPoints-1-i) * time.Hour)
		agg := byHour[hour]
		if agg == nil {
			continue
		}
		if value, _, ok := quantileFromBuckets(agg.LatencyCount, agg.Latency, 0.95); ok {
			points[i] = perfFinitePtr(value)
		}
	}
	return points
}

// ---------------------------------------------------------------------------
// 分组与数值辅助
// ---------------------------------------------------------------------------

// groupModelPerfBuckets 按模型分组窗口内的桶（保留原始粒度）。
func groupModelPerfBuckets(buckets []ModelPerfBucket) map[string][]ModelPerfBucket {
	grouped := make(map[string][]ModelPerfBucket)
	for _, bucket := range buckets {
		grouped[bucket.Model] = append(grouped[bucket.Model], bucket)
	}
	return grouped
}

// groupModelPerfHourly 按 (模型, 小时) 聚合桶。查询粒度已经是 1h，这里仍
// 按 Truncate(time.Hour) 归并，避免同一小时出现多条快照（多副本/多节点）
// 时只取到最后一条。
func groupModelPerfHourly(buckets []ModelPerfBucket) map[string]map[time.Time]*modelPerfAggregate {
	grouped := make(map[string]map[time.Time]*modelPerfAggregate)
	for _, bucket := range buckets {
		hours := grouped[bucket.Model]
		if hours == nil {
			hours = make(map[time.Time]*modelPerfAggregate)
			grouped[bucket.Model] = hours
		}
		hour := bucket.Start.UTC().Truncate(time.Hour)
		agg := hours[hour]
		if agg == nil {
			agg = &modelPerfAggregate{}
			hours[hour] = agg
		}
		agg.add(bucket)
	}
	return grouped
}

func sortedModelNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func medianFloat(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return perfFinitePtr(sorted[middle])
	}
	return perfFinitePtr((sorted[middle-1] + sorted[middle]) / 2)
}

// perfSourceStatusForError 把指标库错误收敛为数据源状态词：UnavailableError
// 是「未配置/连不上」，其余查询错误是 error。错误原文只进 Warn 日志，
// 绝不进响应。
func perfSourceStatusForError(err error) string {
	var unavailable *perfmetrics.UnavailableError
	if errors.As(err, &unavailable) {
		return perfSourceUnavailable
	}
	return perfSourceError
}

// perfFinitePtr 丢弃 NaN/±Inf：encoding/json 无法编码它们，一个坏分位就会
// 让整个端点 500。丢弃后字段为 null。
func perfFinitePtr(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

func perfFiniteFloatPtr(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}

// perfFiniteValue 用于不可为 null 的标量（probe.success_rate）：库层 CHECK
// 已限定 [0,1]，这里的兜底只为防御异常数据；非有限值按 0 处理（保守地
// 判失败，而不是宣称健康）。
func perfFiniteValue(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
