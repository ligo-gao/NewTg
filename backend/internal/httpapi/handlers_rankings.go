package httpapi

// GET /api/rankings —— 公开排行榜（D6b；modules-09 F-09-29～F-09-33 / §C1.7）。
//
// 【读路径】先读 Redis 共享缓存 rankings:public:<period>（worker 每次快照
// 成功后 DEL）；miss 时才按 period 读最新 run + 子表并回填。公开页可被爬虫
// 高频抓取，若每次在线聚合，year 周期会扫 12 个 usage_logs 月分区——缓存
// 把成本从「每次请求」降到「每个刷新周期一次」（§C1.7）。
//
// 【ETag】`"{period}:{computed_at_epoch}"` 只由快照时刻派生：同一 period 的
// 快照未变时响应体（除 freshness_seconds 外）不变，命中 If-None-Match 直接
// 304，省掉公开页的重复带宽。
//
// 【为什么 freshness_seconds 要重新计算】缓存值是完整 DTO JSON，若连同
// freshness_seconds 一起冻结，用户看到的「更新于 N 分钟前」会在整个 TTL 内
// 不动。快照的 computed_at 是不变量，新鲜度按请求时刻现算才是真实语义。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/rankings"
	"newtg/gateway/internal/store"
)

const (
	// rankingsCacheControl 是公开榜单的客户端缓存策略（D6b 冻结值）。
	// max-age 与 worker 的 today 刷新间隔（5m）相比更短：页面切周期/刷新时
	// 仍能较快看到新快照；stale-while-revalidate 让边缘在后台刷新的同时
	// 先回旧值，避免公开页出现空窗。
	rankingsCacheControl = "public, max-age=30, stale-while-revalidate=30"

	// rankingsModelLimit 是 models[] 的上限（F-09-30：接口 limit 上限 50）。
	// 榜单按 token 量降序截断；前端图形视图只取 Top 10，「显示更多」最多 50。
	rankingsModelLimit = 50

	// rankingsCacheTimeout 限制缓存回填的耗时：缓存写失败只影响下次回源，
	// 不应让公开请求无限等待 Redis。
	rankingsCacheTimeout = 2 * time.Second
)

// RankingsHandlerConfig 是公开排行榜端点的装配输入。
type RankingsHandlerConfig struct {
	// Repo 是快照只读接缝（生产 store.NewRankingsRepo(DB)）。
	Repo store.RankingsRepo
	// Redis 是共享缓存；nil 时每次回源（单测/无缓存的降级形态）。
	Redis  *redis.Client
	Config ConfigReader
	Logger *slog.Logger
	// Now 是时钟接缝（新鲜度/窗口断言）；nil 时使用 time.Now。
	Now func() time.Time
}

// RankingsHandlers 持有公开排行榜端点依赖，可并发使用。
type RankingsHandlers struct {
	repo   store.RankingsRepo
	rdb    *redis.Client
	config ConfigReader
	log    *slog.Logger
	now    func() time.Time
}

// NewRankingsHandlers 构造排行榜端点。Repo 必填：没有快照读取面时不能让
// 端点用空数据伪造 200（冷启动是 status=empty，而不是「查不到就装没数据」）。
func NewRankingsHandlers(cfg RankingsHandlerConfig) (*RankingsHandlers, error) {
	if cfg.Repo == nil {
		return nil, fmt.Errorf("httpapi: rankings repo is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &RankingsHandlers{
		repo:   cfg.Repo,
		rdb:    cfg.Redis,
		config: cfg.Config,
		log:    logger,
		now:    now,
	}, nil
}

// RegisterRankingsRoutes 注册公开排行榜端点（精确模式，绕过 SessionAuth）。
func (h *RankingsHandlers) RegisterRankingsRoutes(rt *Router) {
	rt.HandlePublic("GET /api/rankings", http.HandlerFunc(h.handleRankings))
}

// RankingsWindow 是统计窗口（站点时区的自然周期起点 → 快照时刻）。
type RankingsWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// RankingsModel 是模型榜的一行（F-09-30 数据字段表）。
//
// delta_tokens / delta_pct / prev_rank 保留可空语义：
//   - prev_rank = null：上一窗口样本不足或模型是新增的；
//   - delta_pct  = null：上一窗口该模型 token 为 0（前端显示「新增」）；
//   - delta_tokens 在 D6a 聚合中「无上一窗口」时等于当前值（把 prev 视为 0），
//     只有数据库里确为 NULL 时才下发 null。环比是否成立只看 delta_pct。
type RankingsModel struct {
	Rank        int      `json:"rank"`
	Model       string   `json:"model"`
	DisplayName string   `json:"display_name"`
	Author      string   `json:"author"`
	TotalTokens int64    `json:"total_tokens"`
	Requests    int64    `json:"requests"`
	DeltaTokens *int64   `json:"delta_tokens"`
	DeltaPct    *float64 `json:"delta_pct"`
	PrevRank    *int     `json:"prev_rank"`
}

// RankingsMover 是升降榜的一项（F-09-31）。
type RankingsMover struct {
	Model      string  `json:"model"`
	Author     string  `json:"author"`
	DeltaPct   float64 `json:"delta_pct"`
	CurTokens  int64   `json:"cur_tokens"`
	PrevTokens int64   `json:"prev_tokens"`
}

// RankingsMovers 是升降榜双向结果；方向为空时下发 []（不是 null）。
type RankingsMovers struct {
	Up   []RankingsMover `json:"up"`
	Down []RankingsMover `json:"down"`
}

// RankingsProvider 是作者/供应商排名的一行（F-09-33）。
//
// Series 是该作者当前周期的 token 桶，与 trend.buckets 一一对齐；只有作者榜
// Top 5 下发（B5a：worker 只写 Top 5 的历史序列），其余作者省略该字段
// （omitempty，前端以 100% 份额余量表达 Other）。Slot 沿用 rankings_entities
// 的稳定槽位；槽位耗尽时为 null（R-D6-06：不回绕、不重排）。
type RankingsProvider struct {
	Rank        int     `json:"rank"`
	Author      string  `json:"author"`
	TotalTokens int64   `json:"total_tokens"`
	SharePct    float64 `json:"share_pct"`
	Slot        *int    `json:"slot"`
	Series      []int64 `json:"series,omitempty"`
}

// RankingsTrendSeries 是趋势图的一条系列（F-09-32）。
type RankingsTrendSeries struct {
	Model  string  `json:"model"`
	Slot   *int    `json:"slot"`
	Tokens []int64 `json:"tokens"`
}

// RankingsTrend 是历史趋势（F-09-32）：粒度由 period 派生，
// buckets 与 series[].tokens 等长且按时间升序。
type RankingsTrend struct {
	Granularity string                `json:"granularity"`
	Buckets     []string              `json:"buckets"`
	Series      []RankingsTrendSeries `json:"series"`
}

// RankingsResponse 是 GET /api/rankings 的响应体（modules-09 §C1.7）。
//
// computed_at / freshness_seconds 在「从未生成过快照」的冷启动态为 null：
// 没有快照就没有真实的计算时刻与新鲜度，编造 now/0 会让前端把冷启动误判为
// 「刚刚更新」。status=empty 时前端渲染冷启动引导（F-09-29 空态）。
type RankingsResponse struct {
	Period           string             `json:"period"`
	Timezone         string             `json:"timezone"`
	Window           RankingsWindow     `json:"window"`
	ComputedAt       *string            `json:"computed_at"`
	FreshnessSeconds *int64             `json:"freshness_seconds"`
	SampleThreshold  int                `json:"sample_threshold"`
	Status           string             `json:"status"`
	Models           []RankingsModel    `json:"models"`
	Movers           RankingsMovers     `json:"movers"`
	Providers        []RankingsProvider `json:"providers"`
	Trend            RankingsTrend      `json:"trend"`
}

func (h *RankingsHandlers) handleRankings(w http.ResponseWriter, r *http.Request) {
	// B8 公开只读桶：60 次/分钟/IP+path（security-07 冻结值，fail-open）。
	if !h.allowPublicRead(w, r) {
		return
	}

	period, ok := parseRankingsPeriod(r.URL.Query().Get("period"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid period")
		return
	}

	now := h.now().UTC()
	loc := h.location()

	if cached, ok := h.readCache(r.Context(), period); ok {
		h.finalizeFreshness(&cached, now)
		h.writeResponse(w, r, cached)
		return
	}

	snap, found, err := h.repo.LatestRun(r.Context(), string(period))
	if err != nil {
		// 快照表不可用与「尚无快照」是两种语义：后者是合法冷启动（200 +
		// status=empty），前者必须 503，绝不伪装成空榜单。
		h.log.Error("rankings: load snapshot failed", "period", period, "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Rankings are temporarily unavailable")
		return
	}

	view := h.buildResponse(period, loc, snap, found, now)
	// 冷启动（无 run）不缓存：window.end 与 freshness 会随时间变化，且缓存
	// 一个空榜单没有收益；真正有 run 时快照内容在刷新间隔内是不变量。
	if found {
		h.writeCache(r.Context(), period, view)
	}
	h.writeResponse(w, r, view)
}

// parseRankingsPeriod 解析 period；缺省 week，非法值返回 false（400）。
//
// 注意与前端分工不同：前端对 URL 非法值静默回落 week（F-09-29 分支流程），
// 后端必须明确 400，避免把「拼错的周期」悄悄当成 week 掩盖客户端 bug。
func parseRankingsPeriod(raw string) (rankings.Period, bool) {
	if strings.TrimSpace(raw) == "" {
		return rankings.PeriodWeek, true
	}
	return rankings.ParsePeriod(strings.TrimSpace(raw))
}

// location 解析站点时区；零值装配（无配置）回退 UTC。
func (h *RankingsHandlers) location() *time.Location {
	if h.config == nil {
		return time.UTC
	}
	return rankings.ResolveLocation(h.config, h.log)
}

// minSamples 返回当前配置的样本阈值（冷启动响应也要给出 N）。
func (h *RankingsHandlers) minSamples() int64 {
	if h.config == nil {
		return 100 // 与 rankings.defaultMinSamples / settings 注册表一致
	}
	return rankings.ResolveMinSamples(h.config, h.log)
}

// buildResponse 把「run + 子表」投影为公开 DTO。
//
// 所有字段都用白名单构造（§C1.8）：不序列化 usage_logs 行，也不带
// user/token/channel 维度；author 只来自 model_pricing/model_meta 的公开
// 模型元数据（缺失回退 Unknown）。
func (h *RankingsHandlers) buildResponse(period rankings.Period, loc *time.Location,
	snap store.RankingsSnapshot, found bool, now time.Time) RankingsResponse {

	if !found {
		window := rankings.CurrentWindow(period, now, loc)
		return RankingsResponse{
			Period:          string(period),
			Timezone:        loc.String(),
			Window:          rankingsWindowView(window, loc),
			ComputedAt:      nil,
			SampleThreshold: int(h.minSamples()),
			Status:          "empty",
			Models:          []RankingsModel{},
			Movers:          RankingsMovers{Up: []RankingsMover{}, Down: []RankingsMover{}},
			Providers:       []RankingsProvider{},
			Trend: RankingsTrend{
				Granularity: string(period.Granularity()),
				Buckets:     []string{},
				Series:      []RankingsTrendSeries{},
			},
		}
	}

	run := snap.Run
	computed := run.SnapshotAt.UTC()
	computedText := formatRankingsTime(computed, loc)
	freshness := int64(now.Sub(computed).Seconds())
	if freshness < 0 {
		// 时钟回拨/快照来自未来：新鲜度不能为负（契约 ≥0）。
		freshness = 0
	}

	models := make([]RankingsModel, 0, len(snap.Models))
	slotByModel := make(map[string]*int, len(snap.Models))
	modelOrder := make([]string, 0, len(snap.Models))
	for i, m := range snap.Models {
		modelOrder = append(modelOrder, m.Model)
		slotByModel[m.Model] = intPtr(m.Slot)
		if i >= rankingsModelLimit {
			continue
		}
		rank := i + 1
		if m.Rank != nil && *m.Rank > 0 {
			rank = *m.Rank
		}
		display := m.Model
		if m.DisplayName != nil && strings.TrimSpace(*m.DisplayName) != "" {
			display = *m.DisplayName
		}
		author := "Unknown"
		if m.Author != nil && strings.TrimSpace(*m.Author) != "" {
			author = *m.Author
		}
		models = append(models, RankingsModel{
			Rank:        rank,
			Model:       m.Model,
			DisplayName: display,
			Author:      author,
			TotalTokens: m.TotalTokens,
			Requests:    m.Requests,
			DeltaTokens: m.DeltaTokens,
			DeltaPct:    m.DeltaPct,
			PrevRank:    m.PrevRank,
		})
		slotByModel[m.Model] = intPtr(m.Slot)
	}

	providers := make([]RankingsProvider, 0, len(snap.Providers))
	for i, p := range snap.Providers {
		rank := i + 1
		if p.Rank != nil && *p.Rank > 0 {
			rank = *p.Rank
		}
		share := 0.0
		if p.SharePct != nil {
			share = *p.SharePct
		}
		providers = append(providers, RankingsProvider{
			Rank:        rank,
			Author:      p.Author,
			TotalTokens: p.TotalTokens,
			SharePct:    share,
			Slot:        intPtr(p.Slot),
		})
	}

	movers := rankings.ComputeMovers(snap.Models)
	up := make([]RankingsMover, 0, len(movers.Up))
	for _, m := range movers.Up {
		up = append(up, RankingsMover{
			Model: m.Model, Author: m.Author, DeltaPct: m.DeltaPct,
			CurTokens: m.CurTokens, PrevTokens: m.PrevTokens,
		})
	}
	down := make([]RankingsMover, 0, len(movers.Down))
	for _, m := range movers.Down {
		down = append(down, RankingsMover{
			Model: m.Model, Author: m.Author, DeltaPct: m.DeltaPct,
			CurTokens: m.CurTokens, PrevTokens: m.PrevTokens,
		})
	}

	trend, bucketTimes := buildRankingsTrend(period, loc, snap.Timeseries, modelOrder, slotByModel)
	attachProviderSeries(providers, snap.AuthorSeries, bucketTimes)

	return RankingsResponse{
		Period:           string(run.Period),
		Timezone:         loc.String(),
		Window:           rankingsWindowView(rankings.Window{Start: run.WindowStart, End: run.WindowEnd}, loc),
		ComputedAt:       &computedText,
		FreshnessSeconds: &freshness,
		SampleThreshold:  run.SampleThreshold,
		Status:           run.Status,
		Models:           models,
		Movers:           RankingsMovers{Up: up, Down: down},
		Providers:        providers,
		Trend:            trend,
	}
}

// providerSeriesLimit 是下发 series 的作者数（B5a 冻结值：Top 5）。
const providerSeriesLimit = 5

// attachProviderSeries 把作者桶填入对应 provider 行，并与 trend 的时间桶对齐。
//
// 桶集合以 trend.buckets 为准（buildRankingsTrend 从模型桶并集得出）：作者
// 桶是模型桶的派生聚合，模型桶集合是超集。缺失点补 0（面积图上是 0 段而
// 不是断线）。只填前 5 行；其余作者与 Other 不下发 series。
func attachProviderSeries(providers []RankingsProvider, points []store.RankingsAuthorTimeseries, buckets []time.Time) {
	if len(buckets) == 0 || len(points) == 0 {
		return
	}
	byAuthor := make(map[string]map[time.Time]int64, len(points))
	for _, point := range points {
		byBucket, ok := byAuthor[point.Author]
		if !ok {
			byBucket = make(map[time.Time]int64)
			byAuthor[point.Author] = byBucket
		}
		byBucket[point.BucketStart.UTC()] += point.Tokens
	}
	for i := range providers {
		if i >= providerSeriesLimit {
			break
		}
		byBucket, ok := byAuthor[providers[i].Author]
		if !ok {
			continue
		}
		row := make([]int64, len(buckets))
		for j, bucket := range buckets {
			row[j] = byBucket[bucket]
		}
		providers[i].Series = row
	}
}

// buildRankingsTrend 把跨 run 累积的桶整理为「Top 6 + Other」的堆叠序列。
//
// 【为什么只取 6 + Other】§C1.9 系列上限：每图最多 6 个实体 + Other；超出
// 归入中性灰 Other，绝不循环使用第 9 色或重排已有实体的槽位（颜色跟随实体）。
// 桶集合取所有模型桶的并集，缺失点补 0（图表上是 0 段而不是断线）。
func buildRankingsTrend(period rankings.Period, loc *time.Location,
	points []store.RankingsTimeseries, modelOrder []string,
	slotByModel map[string]*int) (RankingsTrend, []time.Time) {

	granularity := string(period.Granularity())
	bucketSet := make(map[time.Time]struct{})
	tokensByModel := make(map[string]map[time.Time]int64)
	for _, point := range points {
		if point.Granularity != granularity {
			continue
		}
		bucket := point.BucketStart.UTC()
		bucketSet[bucket] = struct{}{}
		byBucket, ok := tokensByModel[point.Model]
		if !ok {
			byBucket = make(map[time.Time]int64)
			tokensByModel[point.Model] = byBucket
		}
		byBucket[bucket] += point.Tokens
	}

	buckets := make([]time.Time, 0, len(bucketSet))
	for bucket := range bucketSet {
		buckets = append(buckets, bucket)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Before(buckets[j]) })

	// 模型顺序沿用 modelOrder（模型榜 rank 顺序），保证「Top」与模型榜一致；
	// 不能遍历 slotByModel（Go map 顺序随机，会让颜色/系列顺序每次请求漂移）。
	// timeseries 里若有榜外模型（理论上不会），按名字稳定排序放到末尾。
	order := make([]string, 0, len(tokensByModel))
	seen := make(map[string]struct{}, len(tokensByModel))
	for _, model := range modelOrder {
		if _, ok := tokensByModel[model]; ok {
			order = append(order, model)
			seen[model] = struct{}{}
		}
	}
	extra := make([]string, 0, len(tokensByModel))
	for model := range tokensByModel {
		if _, ok := seen[model]; !ok {
			extra = append(extra, model)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	selected := order
	other := []string(nil)
	if len(order) > 6 {
		selected = order[:6]
		other = order[6:]
	}

	bucketLabels := make([]string, len(buckets))
	for i, bucket := range buckets {
		bucketLabels[i] = formatRankingsTime(bucket, loc)
	}

	series := make([]RankingsTrendSeries, 0, len(selected)+1)
	for _, model := range selected {
		row := make([]int64, len(buckets))
		for i, bucket := range buckets {
			row[i] = tokensByModel[model][bucket]
		}
		series = append(series, RankingsTrendSeries{
			Model: model, Slot: slotByModel[model], Tokens: row,
		})
	}
	if len(other) > 0 {
		row := make([]int64, len(buckets))
		for _, model := range other {
			for i, bucket := range buckets {
				row[i] += tokensByModel[model][bucket]
			}
		}
		// Other 没有实体槽位（用中性灰），slot=null。
		series = append(series, RankingsTrendSeries{Model: "Other", Slot: nil, Tokens: row})
	}
	return RankingsTrend{Granularity: granularity, Buckets: bucketLabels, Series: series}, buckets
}

func rankingsWindowView(window rankings.Window, loc *time.Location) RankingsWindow {
	return RankingsWindow{
		Start: formatRankingsTime(window.Start, loc),
		End:   formatRankingsTime(window.End, loc),
	}
}

// formatRankingsTime 以站点时区输出 RFC3339（例如 2026-10-20T00:00:00+08:00）。
func formatRankingsTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(time.RFC3339)
}

// intPtr 把可空 smallint 槽位转成 DTO 的 *int（NULL 槽位保持 null）。
func intPtr(v *int16) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// ---------------------------------------------------------------------------
// Redis 共享缓存
// ---------------------------------------------------------------------------

// readCache 读取 rankings:public:<period>；未命中/损坏/Redis 不可用都返回
// false（回源）。缓存只是加速面，任何故障都不应让公开榜单不可用。
func (h *RankingsHandlers) readCache(ctx context.Context, period rankings.Period) (RankingsResponse, bool) {
	if h.rdb == nil {
		return RankingsResponse{}, false
	}
	raw, err := h.rdb.Get(ctx, rankings.CacheKey(period)).Bytes()
	if err != nil {
		if err != redis.Nil {
			h.log.Warn("rankings: read cache failed; falling back to snapshot",
				"period", period, "error", err)
		}
		return RankingsResponse{}, false
	}
	var view RankingsResponse
	if err := json.Unmarshal(raw, &view); err != nil {
		// 结构变更/人工写坏：当作 miss 回源，不能把损坏 JSON 返回给客户端。
		h.log.Warn("rankings: cached payload is invalid; falling back to snapshot",
			"period", period, "error", err)
		return RankingsResponse{}, false
	}
	if reason := rankingsCacheMismatch(view, period); reason != "" {
		// 结构可解析但语义不可信（错 period、缺数组、computed_at 坏）同样
		// 回源：把旧周期/半截缓存当作当前榜单会静默展示错误数据。
		h.log.Warn("rankings: cached payload failed validation; falling back to snapshot",
			"period", period, "reason", reason)
		return RankingsResponse{}, false
	}
	return view, true
}

// rankingsCacheMismatch 校验缓存值的语义完整性，返回空串表示可用。
//
// 【为什么必须逐项校验】缓存键只有 period，值是上一次写入时的完整 DTO。
// 若部署跨越结构变更、或有人手工写坏、或 worker 未来写入 partial 但字段
// 不全，单看 json.Unmarshal 成功并不能证明这份数据能安全下发：
//   - period 必须是本次请求的周期（否则会拿 week 的榜单响应 month）；
//   - status 必须是契约枚举（ok/partial/empty），未知值前端无法渲染；
//   - 四个数组字段必须存在（JSON null 反序列化为 nil），否则响应会把
//     「空」与「损坏」混为一谈，且破坏前端「总是数组」的契约；
//   - computed_at 必须可解析：ETag 由它派生，解析失败会退化成固定 ETag。
func rankingsCacheMismatch(view RankingsResponse, period rankings.Period) string {
	if view.Period != string(period) {
		return "period mismatch"
	}
	switch view.Status {
	case "ok", "partial", "empty":
	default:
		return "invalid status"
	}
	if view.Models == nil || view.Providers == nil ||
		view.Movers.Up == nil || view.Movers.Down == nil ||
		view.Trend.Buckets == nil || view.Trend.Series == nil {
		return "missing array field"
	}
	if view.ComputedAt == nil {
		return "missing computed_at"
	}
	if _, err := time.Parse(time.RFC3339, *view.ComputedAt); err != nil {
		return "unparseable computed_at"
	}
	return ""
}

// writeCache 回填缓存，TTL = 该 period 的 worker 刷新间隔（§C1.7）。
// 写失败只记日志：本次请求仍用刚读出的快照正常响应。
func (h *RankingsHandlers) writeCache(ctx context.Context, period rankings.Period, view RankingsResponse) {
	if h.rdb == nil {
		return
	}
	payload, err := json.Marshal(view)
	if err != nil {
		h.log.Warn("rankings: marshal cache payload failed", "period", period, "error", err)
		return
	}
	cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rankingsCacheTimeout)
	defer cancel()
	if err := h.rdb.Set(cacheCtx, rankings.CacheKey(period), payload,
		period.RefreshInterval()).Err(); err != nil {
		h.log.Warn("rankings: write cache failed", "period", period, "error", err)
	}
}

// finalizeFreshness 用请求时刻重算新鲜度（缓存值里的 freshness 会随 TTL 变旧）。
func (h *RankingsHandlers) finalizeFreshness(view *RankingsResponse, now time.Time) {
	if view.ComputedAt == nil {
		return
	}
	computed, err := time.Parse(time.RFC3339, *view.ComputedAt)
	if err != nil {
		return
	}
	freshness := int64(now.Sub(computed).Seconds())
	if freshness < 0 {
		freshness = 0
	}
	view.FreshnessSeconds = &freshness
}

// writeResponse 写 ETag / Cache-Control；命 304 时直接返回。
//
// 【冷启动不发 ETag】computed_at=null 时没有稳定的快照时刻可派生 ETag；
// 早先实现退化为 `"{period}:0"`，于是跨天/跨窗口的两次冷启动拿到同一个
// ETag，客户端带 If-None-Match 会 304 复用旧窗口（window.end 已经变了）。
// 没有快照就不提供验证器，让客户端每次都拿到当前的空态。
func (h *RankingsHandlers) writeResponse(w http.ResponseWriter, r *http.Request, view RankingsResponse) {
	w.Header().Set("Cache-Control", rankingsCacheControl)
	if view.ComputedAt != nil {
		etag := rankingsETag(view.Period, view.ComputedAt)
		w.Header().Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// rankingsETag 由 period + 快照时刻（epoch 秒）派生。
//
// 只含这两个不变量：同一快照的 body 中唯一会变的是 freshness_seconds，而
// 304 命中时客户端复用本地表示、由前端自己按 computed_at 算相对时间。
// 调用方只在 computed_at 非空时使用（冷启动没有验证器，见 writeResponse）。
func rankingsETag(period string, computedAt *string) string {
	epoch := int64(0)
	if computedAt != nil {
		if t, err := time.Parse(time.RFC3339, *computedAt); err == nil {
			epoch = t.Unix()
		}
	}
	return fmt.Sprintf("%q", fmt.Sprintf("%s:%d", period, epoch))
}

// allowPublicRead 消耗 B8 桶；Redis 故障 fail-open（security-07 §2.3）。
func (h *RankingsHandlers) allowPublicRead(w http.ResponseWriter, r *http.Request) bool {
	if h.rdb == nil {
		return true
	}
	over, retry, err := allowRequestRate(r.Context(), h.rdb,
		PublicReadRateLimitKey(clientIP(r), r.URL.Path), PublicReadRateLimit, PublicReadRateWindow)
	if err != nil {
		h.log.Warn("rankings rate limiter unavailable; failing open", "error", err)
		return true
	}
	if over {
		writeRateLimited(w, retry, "Too many requests")
		return false
	}
	return true
}
