// 性能页观测数据入口：GET /api/performance/overview?range=15m|1h|6h|24h
// （spec §7.2）。
//
//	GET /api/performance/overview   settings:manage（只读）
//
// 这个端点只做四件事：把时间窗翻译成 Prometheus 查询参数、把命名查询的
// 结果组装成页面要的形状、统一判定状态、在监控未接入时优雅降级。它不拼接
// 任何用户输入到 PromQL（查询名白名单在 internal/perfquery），也不返回
// 5xx——Prometheus 超时、单条查询失败、Redis 心跳读取失败都是页面上的
// 局部「unknown」或数据源状态，而不是整页错误。
//
// NaN / ±Inf 必须在写 JSON 之前剔除：空直方图的 histogram_quantile 会返回
// "NaN"，strconv.ParseFloat 能解析成 math.NaN()，而 encoding/json 无法编码
// NaN/Inf（会让整个端点 500）。规则统一为「样本丢弃；整条序列被丢光时
// value=null、status=unknown、series=[]」。
package httpapi

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"newtg/gateway/internal/perfquery"
)

// 状态词表是 Task 12 前端的契约，不得自创同义词。
//
//	vitals[].status    ∈ {ok, warn, crit, unknown}
//	sources[].status   ∈ {ok, unavailable, error}
//	gateways[].metrics_status ∈ {ok, unknown}
const (
	perfStatusOK      = "ok"
	perfStatusWarn    = "warn"
	perfStatusCrit    = "crit"
	perfStatusUnknown = "unknown"

	perfSourceUnavailable = "unavailable"
	perfSourceError       = "error"

	perfSourcePrometheus = "prometheus"
	perfSourceHeartbeat  = "heartbeat"
)

// perfDefaultOverviewRange 与 spec §7.2 的 query 参数一致：省略时按 1h。
const perfDefaultOverviewRange = "1h"

// perfWindowSpec 是 range → 查询窗口的冻结映射。points 同时决定 step：
// 15m 取 15 点（每分钟一点），其余 24 点（服务端降采样，页面 sparkline
// 正好用完）。
type perfWindowSpec struct {
	span   time.Duration
	points int
}

var perfOverviewWindows = map[string]perfWindowSpec{
	"15m": {15 * time.Minute, 15},
	"1h":  {time.Hour, 24},
	"6h":  {6 * time.Hour, 24},
	"24h": {24 * time.Hour, 24},
}

// dbPoolWarnFraction 是连接池占用的告警比例（spec §7.2：warn 0.8×max）。
const dbPoolWarnFraction = 0.8

// overviewQueries 是概览所需的全部全局查询（无 instance 维度）。
var overviewQueries = []string{
	perfquery.NameGatewayRPS,
	perfquery.NameGatewayP95,
	perfquery.NameGatewayErrorRate,
	perfquery.NameGatewayInflight,
	perfquery.NameDBPool,
	perfquery.NameDBWait,
	perfquery.NamePGBackends,
	perfquery.NamePGMaxBackends,
	perfquery.NamePGTPS,
	perfquery.NamePGSize,
	perfquery.NameRedisUp,
	perfquery.NameRedisClients,
	perfquery.NameRedisMem,
	perfquery.NameRedisHitRate,
	perfquery.NameRedisCmdRate,
	perfquery.NameHostCPU,
	perfquery.NameHostMem,
	perfquery.NameHostDisk,
	perfquery.NameHostNetIn,
	perfquery.NameHostNetOut,
	// target.up 只用作数据源健康探针（spec §7.2 sources）。
	perfquery.NameTargetUp,
}

// overviewInstanceQueries 是 gateways 区的逐副本查询。必须走
// QueryRangeLabeled：它们按 instance 返回多条序列。
var overviewInstanceQueries = []string{
	perfquery.NameGatewayByInstanceRPS,
	perfquery.NameGatewayByInstanceP95,
	perfquery.NameGatewayByInstanceInflight,
	perfquery.NameGatewayByInstanceGoroutines,
	perfquery.NameGatewayByInstanceHeapMB,
	perfquery.NameGatewayByInstanceErrorRate,
}

// gatewayDiscoveryQueries 决定 gateways 区「有哪些副本」：只有带
// service="gateway" 过滤的逐副本查询有资格发现副本。
//
// go_goroutines / go_memstats_heap_alloc_bytes 刻意**不在**这个列表里：它们
// 没有 service 过滤，同一进程的 /metrics 都会暴露，worker、node-exporter、
// Prometheus 自己都会出现在结果里。那两条查询只能用于**取值**，不能用于
// 发现副本——否则性能页会混入非网关进程（且它们没有心跳，看起来像「失联
// 副本」）。「顺手统一成 overviewInstanceQueries」会把 worker 拉回页面上。
var gatewayDiscoveryQueries = []string{
	perfquery.NameGatewayByInstanceRPS,
	perfquery.NameGatewayByInstanceP95,
	perfquery.NameGatewayByInstanceInflight,
	perfquery.NameGatewayByInstanceErrorRate,
}

// perfVitalThreshold 是状态阈值（spec §7.2「阈值写成包内常量」）。
// 只登记有明确阈值的指标；其余（gateway.rps、redis.hit_rate 等）只区分
// ok / unknown。
type perfVitalThreshold struct {
	warn float64
	crit float64
}

var vitalThresholds = map[string]perfVitalThreshold{
	perfquery.NameGatewayErrorRate: {warn: 0.01, crit: 0.05},
	perfquery.NameGatewayP95:       {warn: 1.0, crit: 3.0},
	perfquery.NameHostDisk:         {warn: 0.8, crit: 0.9},
	perfquery.NameHostMem:          {warn: 0.85, crit: 0.95},
	perfquery.NameHostCPU:          {warn: 0.8, crit: 0.9},
	perfquery.NameRedisMem:         {warn: 0.8, crit: 0.9},
}

// ---------------------------------------------------------------------------
// 响应形状
// ---------------------------------------------------------------------------

type perfOverviewResponse struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Range       string             `json:"range"`
	Windows     perfOverviewWindow `json:"windows"`
	Sources     []perfSource       `json:"sources"`
	Vitals      []perfVital        `json:"vitals"`
	Gateways    []perfGatewayInfo  `json:"gateways"`
	Resources   perfResources      `json:"resources"`
}

// perfOverviewWindow 回显服务端实际使用的窗口与步长，前端据此画 x 轴，
// 不需要在浏览器里重新推算（与 next_run_at 的口径一致：服务端说什么是
// 什么）。
type perfOverviewWindow struct {
	Range       string    `json:"range"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	StepSeconds int64     `json:"step_seconds"`
	Points      int       `json:"points"`
}

type perfSource struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type perfVital struct {
	Key    string       `json:"key"`
	Value  *float64     `json:"value"`
	Unit   string       `json:"unit"`
	Status string       `json:"status"`
	Note   string       `json:"note"`
	Series [][2]float64 `json:"series"`
}

type perfGatewayInfo struct {
	InstanceID     string              `json:"instance_id"`
	Role           string              `json:"role"`
	Version        string              `json:"version"`
	LastSeen       *time.Time          `json:"last_seen"`
	HeartbeatFresh bool                `json:"heartbeat_fresh"`
	Metrics        *perfGatewayMetrics `json:"metrics"`
	MetricsStatus  string              `json:"metrics_status"`
}

type perfGatewayMetrics struct {
	Goroutines *float64 `json:"goroutines"`
	HeapMB     *float64 `json:"heap_mb"`
	P95MS      *float64 `json:"p95_ms"`
	ErrorRate  *float64 `json:"error_rate"`
	Inflight   *float64 `json:"inflight"`
}

type perfResources struct {
	Host     perfHostResources     `json:"host"`
	Postgres perfPostgresResources `json:"postgres"`
	Redis    perfRedisResources    `json:"redis"`
}

// 资源三段的数值一律是 JSON 数字：缺失时填 0（与 spec §7.2 的示例形状一致，
// 前端在 sourceDown 时整块显示空态，不需要逐字段判空）。整个响应里的
// 「缺失」口径因此只有两种：可空字段用 null（vitals.value、gateways.metrics
// 及其子字段），资源数字用 0。
type perfHostResources struct {
	CPU    float64 `json:"cpu"`
	Mem    float64 `json:"mem"`
	Disk   float64 `json:"disk"`
	NetIn  float64 `json:"net_in"`
	NetOut float64 `json:"net_out"`
}

type perfPostgresResources struct {
	Backends  float64 `json:"backends"`
	Max       float64 `json:"max"`
	TPS       float64 `json:"tps"`
	SizeBytes float64 `json:"size_bytes"`
	LockWait  float64 `json:"lock_wait"`
}

type perfRedisResources struct {
	Up       float64 `json:"up"`
	Clients  float64 `json:"clients"`
	MemRatio float64 `json:"mem_ratio"`
	HitRate  float64 `json:"hit_rate"`
	CmdRate  float64 `json:"cmd_rate"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func (h *PerformanceHandlers) handleOverview(w http.ResponseWriter, r *http.Request) {
	rangeName := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeName == "" {
		rangeName = perfDefaultOverviewRange
	}
	window, ok := perfOverviewWindows[rangeName]
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, problemTypePerformanceInvalidValue,
			"Invalid range",
			"invalid_range: range must be one of 15m, 1h, 6h, 24h")
		return
	}

	end := h.now()
	start := end.Add(-window.span)
	step := (window.span / time.Duration(window.points)).Truncate(time.Second)
	if step <= 0 {
		step = time.Second
	}

	// PromURL 与客户端能力必须同时成立：生产装配里二者同源，单测可能只给
	// 其一。任一不成立都按「未接入」降级，而不是发出注定失败的查询。
	available := h.promURL != "" && h.prom.Available()

	var (
		series     map[string][]perfquery.Sample
		byInstance map[string]map[string][]perfquery.Sample
		queryErrs  map[string]error
	)
	if available {
		series, byInstance, queryErrs = h.queryOverview(r.Context(), start, end, step)
	}

	// 心跳读取失败不是 5xx：Redis 抖动只让 gateways 少掉「只有心跳」的行，
	// 数据源区多一条心跳源错误，页面照常渲染。
	records, heartbeatErr := h.instances(r.Context())
	if heartbeatErr != nil {
		h.log.Warn("performance: read instance heartbeats failed; overview lists metrics-only replicas",
			"err", heartbeatErr)
	}

	sources := []perfSource{{Name: perfSourcePrometheus, Status: perfStatusOK}}
	switch {
	case !available:
		sources[0].Status = perfSourceUnavailable
	case queryErrs[perfquery.NameTargetUp] != nil:
		// 探针本身失败才算数据源故障；单条 vital 查询失败不影响 source。
		sources[0].Status = perfSourceError
	}
	if heartbeatErr != nil {
		sources = append(sources, perfSource{Name: perfSourceHeartbeat, Status: perfSourceError})
	}

	resp := perfOverviewResponse{
		GeneratedAt: end,
		Range:       rangeName,
		Windows: perfOverviewWindow{
			Range:       rangeName,
			Start:       start,
			End:         end,
			StepSeconds: int64(step / time.Second),
			Points:      window.points,
		},
		Sources:   sources,
		Vitals:    buildPerfVitals(series, window.points, queryErrs),
		Gateways:  h.buildPerfGateways(records, byInstance, end),
		Resources: buildPerfResources(series),
	}
	writeJSON(w, http.StatusOK, resp)
}

// queryOverview 并发执行全部命名查询。串行 27 条在 Prometheus 慢时会把一次
// 页面刷新拖到几十秒（每条都可能耗满 5s 超时）；并发后总耗时约等于最慢的
// 一条，且查询名是固定集合，不存在扇出放大。
func (h *PerformanceHandlers) queryOverview(ctx context.Context, start, end time.Time, step time.Duration) (map[string][]perfquery.Sample, map[string]map[string][]perfquery.Sample, map[string]error) {
	series := make(map[string][]perfquery.Sample, len(overviewQueries))
	byInstance := make(map[string]map[string][]perfquery.Sample, len(overviewInstanceQueries))
	errs := make(map[string]error)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, name := range overviewQueries {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			samples, err := h.prom.QueryRange(ctx, name, start, end, step)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[name] = err
				return
			}
			series[name] = samples
		}(name)
	}
	for _, name := range overviewInstanceQueries {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			labeled, err := h.prom.QueryRangeLabeled(ctx, name, start, end, step)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[name] = err
				return
			}
			byInstance[name] = labeled
		}(name)
	}
	wg.Wait()

	// 每请求最多一条 warn：单条失败已降级成页面上的 unknown，不需要逐条
	// 刷日志；但整轮全静默会让「Prometheus 挂了」只能靠翻 debug 才发现。
	// first_query / first_error 取固定查询顺序里的第一条，日志可复现。
	if len(errs) > 0 {
		firstQuery, firstErr := "", error(nil)
		for _, name := range overviewQueries {
			if err := errs[name]; err != nil {
				firstQuery, firstErr = name, err
				break
			}
		}
		if firstErr == nil {
			for _, name := range overviewInstanceQueries {
				if err := errs[name]; err != nil {
					firstQuery, firstErr = name, err
					break
				}
			}
		}
		h.log.Warn("performance: overview queries failed",
			"failed", len(errs),
			"total", len(overviewQueries)+len(overviewInstanceQueries),
			"first_query", firstQuery,
			"first_error", firstErr)
	}
	return series, byInstance, errs
}

// ---------------------------------------------------------------------------
// vitals
// ---------------------------------------------------------------------------

// perfVitalSpec 描述一个体检格：headline 查询决定数值与 sparkline，
// statusKeys 里的指标一起参与徽标判定（例如网关格的 p95 / 错误率阈值），
// note 是给页面副标题用的「其余数字」。
type perfVitalSpec struct {
	key        string
	statusKeys []string
	note       func(get func(string) (float64, bool)) string
}

var perfVitalSpecs = []perfVitalSpec{
	{
		key:        perfquery.NameGatewayRPS,
		statusKeys: []string{perfquery.NameGatewayP95, perfquery.NameGatewayErrorRate},
		note:       gatewayVitalNote,
	},
	{
		key:        perfquery.NameDBPool,
		statusKeys: []string{perfquery.NameDBPool},
		note:       dbVitalNote,
	},
	{
		key:        perfquery.NameRedisHitRate,
		statusKeys: []string{perfquery.NameRedisMem},
		note:       redisVitalNote,
	},
	{
		key:        perfquery.NameHostDisk,
		statusKeys: []string{perfquery.NameHostDisk, perfquery.NameHostCPU, perfquery.NameHostMem},
		note:       hostVitalNote,
	},
}

func buildPerfVitals(series map[string][]perfquery.Sample, points int, queryErrs map[string]error) []perfVital {
	// series 为 nil（监控未接入）时 get 恒 false：所有格 value=null、
	// status=unknown、series=[]。
	get := func(name string) (float64, bool) { return latestFinite(series[name]) }

	vitals := make([]perfVital, 0, len(perfVitalSpecs))
	for _, spec := range perfVitalSpecs {
		vital := perfVital{
			Key:    spec.key,
			Status: perfStatusUnknown,
			Series: sparkline(series[spec.key], points),
		}
		if _, unit, ok := perfquery.Lookup(spec.key); ok {
			vital.Unit = unit
		}
		if value, ok := latestFinite(series[spec.key]); ok {
			vital.Value = perfFloatPtr(value)
			vital.Status = vitalStatus(spec, get, queryErrs)
			vital.Note = spec.note(get)
		}
		vitals = append(vitals, vital)
	}
	return vitals
}

// vitalStatus 判定一格的状态。任何一个组成查询失败（Prometheus 报错/超时）
// 都让整格变成 unknown：缺了 p95 或错误率就无法回答「这个域到底健不健康」，
// 拿剩余指标猜一个 ok 是误导。查询成功但没有数据点不算失败——那只是该指标
// 未暴露，其余指标仍可判定。
func vitalStatus(spec perfVitalSpec, get func(string) (float64, bool), queryErrs map[string]error) string {
	keys := append([]string{spec.key}, spec.statusKeys...)
	if spec.key == perfquery.NameDBPool {
		// 连接池阈值依赖 pg.max_backends，它必须参与失败判定。
		keys = append(keys, perfquery.NamePGMaxBackends)
	}
	for _, key := range keys {
		if queryErrs[key] != nil {
			return perfStatusUnknown
		}
	}

	switch spec.key {
	case perfquery.NameDBPool:
		// 连接池没有绝对阈值：warn = 用到 pg.max_backends 的 80%。
		pool, ok := get(perfquery.NameDBPool)
		if !ok {
			return perfStatusUnknown
		}
		max, ok := get(perfquery.NamePGMaxBackends)
		if !ok || max <= 0 {
			return perfStatusOK
		}
		if pool/max >= dbPoolWarnFraction {
			return perfStatusWarn
		}
		return perfStatusOK
	default:
		status := perfStatusOK
		for _, key := range spec.statusKeys {
			value, ok := get(key)
			if !ok {
				continue
			}
			threshold, ok := vitalThresholds[key]
			if !ok {
				continue
			}
			status = worseStatus(status, thresholdStatus(value, threshold))
		}
		return status
	}
}

func thresholdStatus(value float64, threshold perfVitalThreshold) string {
	switch {
	case value >= threshold.crit:
		return perfStatusCrit
	case value >= threshold.warn:
		return perfStatusWarn
	default:
		return perfStatusOK
	}
}

// worseStatus 按 ok < warn < crit 取更严重者；unknown 不参与（headline
// 缺失已在外层直接判 unknown）。
func worseStatus(a, b string) string {
	rank := func(status string) int {
		switch status {
		case perfStatusCrit:
			return 3
		case perfStatusWarn:
			return 2
		case perfStatusOK:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func gatewayVitalNote(get func(string) (float64, bool)) string {
	var parts []string
	if value, ok := get(perfquery.NameGatewayP95); ok {
		parts = append(parts, "P95 "+formatFloat(value, 2)+"s")
	}
	if value, ok := get(perfquery.NameGatewayErrorRate); ok {
		parts = append(parts, "5xx "+formatFloat(value*100, 2)+"%")
	}
	return strings.Join(parts, " · ")
}

func dbVitalNote(get func(string) (float64, bool)) string {
	var parts []string
	if value, ok := get(perfquery.NamePGTPS); ok {
		parts = append(parts, "TPS "+formatFloat(value, 1))
	}
	backends, hasBackends := get(perfquery.NamePGBackends)
	max, hasMax := get(perfquery.NamePGMaxBackends)
	if hasBackends && hasMax {
		parts = append(parts, formatFloat(backends, 0)+"/"+formatFloat(max, 0)+" conn")
	}
	return strings.Join(parts, " · ")
}

func redisVitalNote(get func(string) (float64, bool)) string {
	var parts []string
	if value, ok := get(perfquery.NameRedisCmdRate); ok {
		parts = append(parts, formatFloat(value, 0)+" ops/s")
	}
	if value, ok := get(perfquery.NameRedisClients); ok {
		parts = append(parts, formatFloat(value, 0)+" clients")
	}
	return strings.Join(parts, " · ")
}

func hostVitalNote(get func(string) (float64, bool)) string {
	var parts []string
	if value, ok := get(perfquery.NameHostCPU); ok {
		parts = append(parts, "CPU "+formatFloat(value*100, 0)+"%")
	}
	if value, ok := get(perfquery.NameHostMem); ok {
		parts = append(parts, "MEM "+formatFloat(value*100, 0)+"%")
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// resources
// ---------------------------------------------------------------------------

func buildPerfResources(series map[string][]perfquery.Sample) perfResources {
	value := func(name string) float64 {
		if v, ok := latestFinite(series[name]); ok {
			return v
		}
		return 0
	}
	return perfResources{
		Host: perfHostResources{
			CPU:    value(perfquery.NameHostCPU),
			Mem:    value(perfquery.NameHostMem),
			Disk:   value(perfquery.NameHostDisk),
			NetIn:  value(perfquery.NameHostNetIn),
			NetOut: value(perfquery.NameHostNetOut),
		},
		Postgres: perfPostgresResources{
			Backends:  value(perfquery.NamePGBackends),
			Max:       value(perfquery.NamePGMaxBackends),
			TPS:       value(perfquery.NamePGTPS),
			SizeBytes: value(perfquery.NamePGSize),
			LockWait:  value(perfquery.NameDBWait),
		},
		Redis: perfRedisResources{
			Up:       value(perfquery.NameRedisUp),
			Clients:  value(perfquery.NameRedisClients),
			MemRatio: value(perfquery.NameRedisMem),
			HitRate:  value(perfquery.NameRedisHitRate),
			CmdRate:  value(perfquery.NameRedisCmdRate),
		},
	}
}

// ---------------------------------------------------------------------------
// gateways
// ---------------------------------------------------------------------------

// buildPerfGateways 把心跳与按 instance 的指标合并成副本列表（spec §7.2）：
//   - 心跳按 role=="gateway" 过滤；
//   - 只有指标没有心跳 → 保留（heartbeat_fresh=false，last_seen=null）；
//   - 只有心跳没有指标 → metrics=null + metrics_status=unknown；
//   - 两者都缺的副本不出现。
func (h *PerformanceHandlers) buildPerfGateways(records []InstanceRecord, byInstance map[string]map[string][]perfquery.Sample, now time.Time) []perfGatewayInfo {
	rows := make(map[string]*perfGatewayInfo, len(records))
	for _, record := range records {
		if record.Role != instanceRoleGateway {
			continue
		}
		row := &perfGatewayInfo{
			InstanceID:     record.InstanceID,
			Role:           record.Role,
			Version:        record.Version,
			HeartbeatFresh: !record.LastSeen.IsZero() && now.Sub(record.LastSeen) <= InstanceHeartbeatTTL,
			MetricsStatus:  perfStatusUnknown,
		}
		if !record.LastSeen.IsZero() {
			lastSeen := record.LastSeen
			row.LastSeen = &lastSeen
		}
		rows[record.InstanceID] = row
	}

	// 指标侧可能出现心跳里没有的 instance（Redis 抖动、心跳 key 先过期）。
	// 只用带 service="gateway" 过滤的查询发现副本；go_* 两条没有 service
	// 过滤（见 gatewayDiscoveryQueries 的注释），不能把 worker / exporter
	// 拉进页面。
	for _, name := range gatewayDiscoveryQueries {
		for instance := range byInstance[name] {
			if _, ok := rows[instance]; ok {
				continue
			}
			// role 无法从指标推断，留空而不是臆造。
			rows[instance] = &perfGatewayInfo{
				InstanceID:    instance,
				MetricsStatus: perfStatusUnknown,
			}
		}
	}

	out := make([]perfGatewayInfo, 0, len(rows))
	for instance, row := range rows {
		row.Metrics = gatewayMetricsFor(byInstance, instance)
		if row.Metrics != nil {
			if gatewayMetricsComplete(row.Metrics) {
				row.MetricsStatus = perfStatusOK
			}
		}
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// gatewayMetricsFor 取该副本各项指标的最新有限值。一个值都没有时返回 nil
// （metrics=null）；部分缺失时其余字段为 null——不拿 0 冒充「没有 goroutine」。
func gatewayMetricsFor(byInstance map[string]map[string][]perfquery.Sample, instance string) *perfGatewayMetrics {
	value := func(name string) *float64 {
		v, ok := latestFinite(byInstance[name][instance])
		if !ok {
			return nil
		}
		return perfFloatPtr(v)
	}

	metrics := &perfGatewayMetrics{
		Goroutines: value(perfquery.NameGatewayByInstanceGoroutines),
		HeapMB:     value(perfquery.NameGatewayByInstanceHeapMB),
		ErrorRate:  value(perfquery.NameGatewayByInstanceErrorRate),
		Inflight:   value(perfquery.NameGatewayByInstanceInflight),
	}
	// 目录里 p95 的单位是秒，页面字段是毫秒（spec §7.2 的 p95_ms）。
	if p95, ok := latestFinite(byInstance[perfquery.NameGatewayByInstanceP95][instance]); ok {
		metrics.P95MS = perfFloatPtr(p95 * 1000)
	}

	if metrics.Goroutines == nil && metrics.HeapMB == nil && metrics.P95MS == nil &&
		metrics.ErrorRate == nil && metrics.Inflight == nil {
		return nil
	}
	return metrics
}

func gatewayMetricsComplete(metrics *perfGatewayMetrics) bool {
	return metrics.Goroutines != nil && metrics.HeapMB != nil && metrics.P95MS != nil &&
		metrics.ErrorRate != nil && metrics.Inflight != nil
}

// ---------------------------------------------------------------------------
// 数值辅助
// ---------------------------------------------------------------------------

// latestFinite 返回序列里最后一个有限值（NaN/±Inf 跳过）。时间是升序的，
// 因此从尾部找即可。
func latestFinite(samples []perfquery.Sample) (float64, bool) {
	for i := len(samples) - 1; i >= 0; i-- {
		value := samples[i].Value
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		return value, true
	}
	return 0, false
}

// sparkline 把样本转成 [unix 秒, 值] 的数组，并丢弃 NaN/±Inf。prometheus
// query_range 的端点是闭区间，按 span/points 当 step 通常返回 points+1 个
// 点；这里保留最新的 points 个，让响应里的 sparkline 点数与 windows.points
// 恒等（spec §7.2「固定 24 点」）。
// 返回空切片而不是 nil：JSON 里必须是 []，不是 null。
func sparkline(samples []perfquery.Sample, points int) [][2]float64 {
	finite := make([][2]float64, 0, len(samples))
	for _, sample := range samples {
		if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
			continue
		}
		finite = append(finite, [2]float64{float64(sample.At.Unix()), sample.Value})
	}
	if points > 0 && len(finite) > points {
		finite = finite[len(finite)-points:]
	}
	return finite
}

func perfFloatPtr(value float64) *float64 { return &value }

func formatFloat(value float64, digits int) string {
	return strconv.FormatFloat(value, 'f', digits, 64)
}
