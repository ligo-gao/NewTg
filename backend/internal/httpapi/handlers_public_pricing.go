// 公开定价目录端点（modules-09 F-09-14~F-09-22；M6c Task 7；R-PUB-01~05）。
//
// 端点面：
//
//	GET /api/pricing                     → PublicPricingList
//	GET /api/pricing/metrics             → PublicModelMetricsList（目录内批量指标）
//	GET /api/pricing/{model_id}          → PublicModelDetail（URL 编码的 `/`）
//	GET /api/pricing/{model_id}/metrics  → PublicModelMetrics（单模型指标）
//
// 与前缀 /api/model-pricing 的管理面严格分离（modules-06 G-5 命名空间裁决）：
// 公开面只读、只返回展示字段，绝不出现 id/author/status/billing_expr/channel
// 等内部字段。
//
// 关键口径：
//   - 金额一律 int64 的 JSON 字符串（防前端 Number 精度丢失）；
//   - NULL 价列保持 JSON null（R-PUB-03），不伪造兜底价，0 才是显式免费；
//   - 非法参数逐项降级返回 200（R-PUB-04），空集是 200 + items:[]；
//   - facet 计数忽略「本组自身」的筛选（标准 faceted navigation）；
//   - 分组价只在登录或显式 group 时返回；匿名且 expose_group_ratios=false 时
//     连倍率都不下发（R-PUB-05，覆盖 F-09-15 的匿名展示倍率）。

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/pricingstrategy"
	"newtg/gateway/internal/settings"
	"newtg/gateway/internal/store"
)

const (
	publicPricingDefaultPageSize = 24
	publicPricingMaxPageSize     = 100
	publicPricingSearchMaxRunes  = 128
	// publicPricingMaxVendorFilters 限制单次请求的 vendor 取值数量：query 数组
	// 没有天然上界，放任增长会让内存过滤的谓词数量随 URL 长度线性放大。
	publicPricingMaxVendorFilters = 50

	publicPricingSortName      = "name"
	publicPricingSortPriceAsc  = "price_asc"
	publicPricingSortPriceDesc = "price_desc"

	publicPricingModeToken   = "per_token"
	publicPricingModeRequest = "per_request"

	// problemTypeModelNotFound 是详情 404 的冻结 type（modules-09 F-09-22/B3）。
	problemTypeModelNotFound = "https://newtg.dev/problems/model-not-found"
	// problemTypeNotImplemented 用于 P1 指标端点的显式占位：不返回伪造数据，
	// 也不把「未实现」伪装成「模型不存在」。
	problemTypeNotImplemented = "https://newtg.dev/problems/not-implemented"

	// publicPricingCacheControl 是 F-09-14 冻结的缓存策略（R-PUB-06）。
	publicPricingCacheControl = "public, max-age=30, stale-while-revalidate=30"
	// publicPricingPrivateCacheControl 用于带会话/显式分组的请求。响应体含
	// 该用户的实付价与（expose=true 时）分组倍率，不能进共享缓存；除 max-age
	// 外与公开策略一致，前端行为不变。
	publicPricingPrivateCacheControl = "private, max-age=30, stale-while-revalidate=30"
)

// errPublicPricingRepoRequired 是公开读面的装配错误（生产缺 DB 属于装配错误）。
var errPublicPricingRepoRequired = errors.New("httpapi: PublicPricingHandlerConfig.Repo is required")
var errHomeStatsRepoRequired = errors.New("httpapi: HomeStatsHandlerConfig.Repo is required")

// PublicPricingHandlerConfig 是公开目录端点的装配输入。
type PublicPricingHandlerConfig struct {
	Repo store.PublicPricingRepo
	// Config 读取 billing.expose_group_ratios、billing.quota_per_unit、
	// billing.currency_symbol 与分组限速（rl.*）。nil 时全部取冻结默认值。
	Config ConfigReader
	// Sessions 用于软鉴权；nil 时一切请求按匿名处理。
	Sessions *auth.SessionManager
	// UserGroups 是会话用户分组的读取接缝；nil 时登录用户也拿不到 group_price
	// （公开数据仍正常）。
	UserGroups PublicUserGroupLookup
	// Redis 用于 B8 公开只读限流桶；nil 时跳过限流（仅测试装配）。
	Redis  *redis.Client
	Logger *slog.Logger
	// Metrics 是公开性能接口的数据接缝（读日志库 usage_logs）。nil 时
	// 接口返回 503，而不是伪造 200 空数据。
	Metrics store.PublicModelMetricsSource
	// Perf 提供最近一次主动测速结果（主库 model_performance_tests）。nil 或
	// 查询失败时 latest_test 为 null，指标响应仍为 200（fail-open）。
	Perf store.ModelPerformanceStore
}

// PublicPricingHandlers 持有公开目录端点依赖，可并发使用。
type PublicPricingHandlers struct {
	repo     store.PublicPricingRepo
	config   ConfigReader
	sessions *auth.SessionManager
	groups   PublicUserGroupLookup
	rdb      *redis.Client
	log      *slog.Logger
	metrics  store.PublicModelMetricsSource
	perf     store.ModelPerformanceStore
}

// NewPublicPricingHandlers 构造公开目录端点。Repo 必填。
func NewPublicPricingHandlers(cfg PublicPricingHandlerConfig) (*PublicPricingHandlers, error) {
	if cfg.Repo == nil {
		return nil, errPublicPricingRepoRequired
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &PublicPricingHandlers{
		repo:     cfg.Repo,
		config:   cfg.Config,
		sessions: cfg.Sessions,
		groups:   cfg.UserGroups,
		rdb:      cfg.Redis,
		log:      logger,
		metrics:  cfg.Metrics,
		perf:     cfg.Perf,
	}, nil
}

// RegisterPublicPricingRoutes 注册公开目录端点（精确模式，绕过 SessionAuth）。
func (h *PublicPricingHandlers) RegisterPublicPricingRoutes(rt *Router) {
	rt.HandlePublic("GET /api/pricing", http.HandlerFunc(h.handlePublicPricingList))
	// 批量指标必须注册在 `{model_id}` 之前：Go 1.22 的字面量模式本来就优先于
	// 通配段，但相邻书写能让「/metrics 是列表子路径而不是模型名」一目了然。
	rt.HandlePublic("GET /api/pricing/metrics", http.HandlerFunc(h.handlePublicPricingMetricsList))
	rt.HandlePublic("GET /api/pricing/{model_id}", http.HandlerFunc(h.handlePublicPricingDetail))
	rt.HandlePublic("GET /api/pricing/{model_id}/metrics", http.HandlerFunc(h.handlePublicPricingMetrics))
	// 未匹配的定价子路径兜底（公开 404，而不是落进受保护 /api/ 子树）。
	//
	// 【为什么需要这条兜底】Go 1.22 的 `{model_id}` 只匹配单段：`/api/pricing/`
	// 或未编码的 `/api/pricing/openai/gpt-5` 不匹配任何公开精确模式，会落到
	// `/api/` 子树 → SessionAuth 401。对匿名访客来说这是错误语义：定价是公开
	// 数据，「路径形态不对/模型不存在」应当是 404 model-not-found，而不是
	// 「请先登录」。`{rest...}` 是尾部通配，优先级低于上述所有更具体的模式，
	// 因此不会截获已编码的单段详情或 /metrics。
	rt.HandlePublic("GET /api/pricing/{rest...}", http.HandlerFunc(h.handlePublicPricingFallback))
}

// handlePublicPricingFallback 统一返回 404 model-not-found（不区分「多段路径」
// 与「不存在」：两者都不对应任何可公示模型）。
func (h *PublicPricingHandlers) handlePublicPricingFallback(w http.ResponseWriter, r *http.Request) {
	h.writeModelNotFound(w)
}

// ---------------------------------------------------------------------------
// 响应 DTO
// ---------------------------------------------------------------------------

// PublicModelCard 是列表项。字段集合由 F-09-18 冻结；元数据缺口（R-PUB-01）
// 以空数组/null 表达，绝不编造。
type PublicModelCard struct {
	ModelID         string                `json:"model_id"`
	DisplayName     string                `json:"display_name"`
	Vendor          string                `json:"vendor"`
	Capabilities    []string              `json:"capabilities"`
	ContextLength   *int64                `json:"context_length"`
	MaxOutput       *int64                `json:"max_output"`
	Mode            string                `json:"mode"`
	PromptPrice     *string               `json:"prompt_price"`
	CompletionPrice *string               `json:"completion_price"`
	CacheReadPrice  *string               `json:"cache_read_price"`
	CacheWritePrice *string               `json:"cache_write_price"`
	IsFree          bool                  `json:"is_free"`
	GroupPrice      *PublicGroupPriceView `json:"group_price"`
	// PricingSchedule 是时段计价表（spec §5.4）：strategy.time.enabled=true
	// 时非 null，否则为 null——未启用时响应与旧版逐字段一致。
	PricingSchedule *PricingScheduleView `json:"pricing_schedule"`
	Endpoints       []string             `json:"endpoints"`
	Tags            []string             `json:"tags"`
	Description     string               `json:"description"`
}

// PublicGroupPriceView 是分组实付价（服务端整数运算，前端只展示）。
type PublicGroupPriceView struct {
	Group      string  `json:"group"`
	Ratio      *int32  `json:"ratio"`
	Prompt     *string `json:"prompt"`
	Completion *string `json:"completion"`
	CacheRead  *string `json:"cache_read"`
	CacheWrite *string `json:"cache_write"`
	Source     string  `json:"source"` // override | ratio
}

// PublicFacetOption 是通用 facet 项（vendor/capability/context/endpoint）。
// label 在元数据缺失时等于 value（R-PUB-01：不伪造展示名）。
type PublicFacetOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// PublicGroupFacet 是分组 facet；ratio 在不可展示时为 null（R-PUB-05）。
type PublicGroupFacet struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Ratio *int32 `json:"ratio"`
	Count int64  `json:"count"`
}

// PublicPricingTypeFacet 是计费类型 facet（token | request）。
type PublicPricingTypeFacet struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// PublicFacets 与 F-09-15 的六组一一对应。元数据缺失的模型不产生
// capabilities/context/endpoints 计数（D1-18），但数组本身恒为非 null。
type PublicFacets struct {
	Vendors      []PublicFacetOption      `json:"vendors"`
	Capabilities []PublicFacetOption      `json:"capabilities"`
	Context      []PublicFacetOption      `json:"context"`
	Groups       []PublicGroupFacet       `json:"groups"`
	Endpoints    []PublicFacetOption      `json:"endpoints"`
	PricingTypes []PublicPricingTypeFacet `json:"pricing_types"`
}

// PublicPricingMeta 是展示换算所需的元数据（F-09-19）。
type PublicPricingMeta struct {
	CreditSymbol    string  `json:"credit_symbol"`
	QuotaPerUnit    int64   `json:"quota_per_unit"`
	USDExchangeRate float64 `json:"usd_exchange_rate"`
	Version         string  `json:"version"`
	GeneratedAt     string  `json:"generated_at"`
}

// PublicPricingList 是 GET /api/pricing 的响应信封。
type PublicPricingList struct {
	Items    []PublicModelCard `json:"items"`
	Total    string            `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Facets   PublicFacets      `json:"facets"`
	Meta     PublicPricingMeta `json:"meta"`
}

// PublicModelParameter 是详情页参数表行（F-09-24）；无档案时为空数组。
type PublicModelParameter struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Type        string `json:"type"`
	Default     any    `json:"default"`
	Description string `json:"description"`
}

// PublicRateLimit 是详情页「速率限制（按分组）」行（F-09-27）。
type PublicRateLimit struct {
	Group        string `json:"group"`
	UserModelRPM int64  `json:"user_model_rpm"`
}

// PublicModelDetail 是 ModelCard 的超集（F-09-24）。
type PublicModelDetail struct {
	PublicModelCard
	Modalities      []string               `json:"modalities"`
	KnowledgeCutoff *string                `json:"knowledge_cutoff"`
	ReleasedAt      *string                `json:"released_at"`
	GroupPrices     []PublicGroupPriceView `json:"group_prices"`
	Parameters      []PublicModelParameter `json:"parameters"`
	RateLimits      []PublicRateLimit      `json:"rate_limits"`
	AuthStyle       string                 `json:"auth_style"`
}

// ---------------------------------------------------------------------------
// 请求解析（R-PUB-04：逐参数降级，永不因查询参数返回 4xx）
// ---------------------------------------------------------------------------

type publicPricingQuery struct {
	Page       int
	PageSize   int
	Search     string
	Sort       string
	Vendors    []string // 已小写、去重
	Capability []string // 已小写、去重（D1-21）
	Context    []string // 已小写、去重（分档值，如 128k）
	Endpoints  []string // 已小写、去重（endpointType，如 chat.completions）
	Group      string
	QuotaType  string // all | token | request
	// IncludeGroupPrice 保留原有外部契约。
	IncludeGroupPrice bool
}

func parsePublicPricingQuery(r *http.Request) publicPricingQuery {
	q := r.URL.Query()
	query := publicPricingQuery{
		Page: 1, PageSize: publicPricingDefaultPageSize,
		Sort: publicPricingSortName, QuotaType: "all", IncludeGroupPrice: true,
	}

	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		// 非整数/≤0 回退首屏；上界在分页阶段按最后一页收敛（F-09-21 越界回退）。
		if page, err := strconv.Atoi(raw); err == nil && page > 0 {
			query.Page = page
		}
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		if size, err := strconv.Atoi(raw); err == nil && size > 0 {
			query.PageSize = size
		}
		if query.PageSize > publicPricingMaxPageSize {
			// 上限是硬约束：降级为上限而不是丢弃整个参数。
			query.PageSize = publicPricingMaxPageSize
		}
	}

	query.Search = truncateRunes(strings.TrimSpace(q.Get("search")), publicPricingSearchMaxRunes)

	switch strings.TrimSpace(q.Get("sort")) {
	case publicPricingSortPriceAsc:
		query.Sort = publicPricingSortPriceAsc
	case publicPricingSortPriceDesc:
		query.Sort = publicPricingSortPriceDesc
	default:
		query.Sort = publicPricingSortName
	}

	// vendor 既接受重复键（vendor=a&vendor=b）也接受逗号分隔（vendor=a,b）。
	query.Vendors = parseFacetValues(q, "vendor", publicPricingMaxVendorFilters)
	// D1-21 的三个新增多值参数；同时接受 `name[]` 形态（表单数组序列化），
	// 与 vendor 一致地容忍重复键与逗号分隔。
	query.Capability = parseFacetValues(q, "capability", publicPricingMaxVendorFilters)
	query.Context = parseFacetValues(q, "context", publicPricingMaxVendorFilters)
	query.Endpoints = parseFacetValues(q, "endpointType", publicPricingMaxVendorFilters)
	if len(query.Endpoints) == 0 {
		// 兼容 snake_case 的手写 URL；文档冻结名是 endpointType。
		query.Endpoints = parseFacetValues(q, "endpoint_type", publicPricingMaxVendorFilters)
	}

	query.Group = strings.TrimSpace(q.Get("group"))

	// 文档冻结名是 quotaType；同时接受 snake_case 形态以容忍手写 URL。
	quotaType := strings.TrimSpace(q.Get("quotaType"))
	if quotaType == "" {
		quotaType = strings.TrimSpace(q.Get("quota_type"))
	}
	switch quotaType {
	case "token", "request":
		query.QuotaType = quotaType
	default:
		query.QuotaType = "all"
	}

	switch strings.ToLower(strings.TrimSpace(q.Get("include_group_price"))) {
	case "0", "false":
		query.IncludeGroupPrice = false
	case "1", "true":
		query.IncludeGroupPrice = true
	default:
		query.IncludeGroupPrice = true
	}
	return query
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}

// parseFacetValues 读取一个多值 facet 参数（重复键 + 逗号分隔），统一小写
// 并去重。上界按项而不是按键：query 数组没有天然上界，放任增长会让内存
// 过滤的谓词数量随 URL 长度线性放大（原 vendor 上限的同一条理由）。
func parseFacetValues(q url.Values, key string, limit int) []string {
	raw := make([]string, 0, len(q[key])+len(q[key+"[]"]))
	raw = append(raw, q[key]...)
	raw = append(raw, q[key+"[]"]...)
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			value := strings.ToLower(strings.TrimSpace(part))
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			if len(out) >= limit {
				return out
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 目录索引
// ---------------------------------------------------------------------------

// publicCatalogModel 是内存过滤/排序的工作项。
type publicCatalogModel struct {
	rec store.PublicModelPricing
	// meta 是解析到的**启用**档案；nil 表示无档案，卡片按 D1-15 降级。
	meta    *store.ModelMeta
	vendor  string // 三级回退后的供应商展示名（D1-16）
	mode    string
	lowerID string
	// 以下是从档案一次性派生的匹配值：filtered 每次请求要跑 6 遍 facet
	// 子集，重复解析 JSON 会把公开目录的 CPU 成本按 facet 数放大。
	capabilities  []string // 去重后保留首个原值（按大小写折叠）
	contextBucket string   // "" = 无档案或 context_length=null（不参与计数/筛选）
	endpointTypes []string // D1-19 派生枚举（规范大小写）
}

// buildPublicCatalogModels 把定价行与档案快照装配成工作项。
//
// 可见集合仍由 model_pricing 驱动（D1-13）；档案只做挂载。解析用全量档案
// （含 status=2）：命中停用档案 ⇒ 该模型整条剔除（items/facets/详情 404），
// 否则一条 exact 停用档案会被更宽的前缀档案穿透，模型带着错误元数据上架。
func buildPublicCatalogModels(rows []store.PublicModelPricing,
	meta []store.ModelMeta, vendors []store.PublicVendor) []publicCatalogModel {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Model)
	}
	resolved := store.ResolveModelMeta(meta, names)
	vendorNames := make(map[int64]string, len(vendors))
	for _, vendor := range vendors {
		vendorNames[vendor.ID] = vendor.Name
	}

	models := make([]publicCatalogModel, 0, len(rows))
	for _, row := range rows {
		record, ok := resolved[row.Model]
		if ok && record.Status != 1 {
			// 停用档案对公开面不存在（D1-13）；用 !=1 而不是 ==2 取默认拒绝，
			// 未来若新增状态值也不会把未知状态误当成启用。
			continue
		}
		var attached *store.ModelMeta
		if ok {
			copied := record
			attached = &copied
		}
		mode := publicPricingModeToken
		if row.Mode == 1 {
			mode = publicPricingModeRequest
		}
		model := publicCatalogModel{
			rec: row, meta: attached, mode: mode,
			vendor:  publicModelVendor(row, attached, vendorNames),
			lowerID: strings.ToLower(row.Model),
		}
		if attached != nil {
			// 档案允许重复/大小写变体（["text","text"]、["text","Text"]）。去重
			// 在装配期完成：facet 计数与筛选 total 必须同口径，否则一个重复
			// 能力会在 facet 里显示 2、筛选结果却只有 1 个模型。
			model.capabilities = dedupeFoldPreserveFirst(decodeStringArray(attached.Capabilities))
			model.contextBucket = publicContextBucket(attached.ContextLength)
			model.endpointTypes = store.DeriveEndpointTypes(attached.Endpoints)
		}
		models = append(models, model)
	}
	return models
}

// publicModelVendor 实现 D1-16 的三级回退：
// 档案关联的 vendors.name → model_pricing.vendor → model_id 首段前缀。
//
// 空的/仅空白的值继续向下一级回退：展示空供应商与缺数据无法区分，留白
// 比显示一条看不见的字符串更差。
func publicModelVendor(rec store.PublicModelPricing, meta *store.ModelMeta, vendorNames map[int64]string) string {
	if meta != nil && meta.VendorID != nil {
		if name, ok := vendorNames[*meta.VendorID]; ok {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				return trimmed
			}
		}
	}
	if rec.Vendor != nil {
		if trimmed := strings.TrimSpace(*rec.Vendor); trimmed != "" {
			return trimmed
		}
	}
	return vendorPrefix(rec.Model)
}

// vendorPrefix 按 R-PUB-02 从 model_id 的首个 `/` 前派生供应商。
func vendorPrefix(model string) string {
	if index := strings.Index(model, "/"); index > 0 {
		return model[:index]
	}
	return ""
}

// ---------------------------------------------------------------------------
// 档案派生值（D1-18/19/20）
// ---------------------------------------------------------------------------

// 上下文分档的冻结边界（D1-20）：
//
//	8K   ≤ 8,192
//	32K  8,193 – 32,768
//	128K 32,769 – 131,072
//	200K+ > 131,072
//
// 边界按「档位上界」闭区间定义，因此 8,192 属于 8K、32,768 属于 32K，
// 8,193 属于 32K、32,769 属于 128K。null 不计数（返回值 ""）。
const (
	publicContext8K   = "8K"
	publicContext32K  = "32K"
	publicContext128K = "128K"
	publicContext200K = "200K+"
)

func publicContextBucket(length *int64) string {
	if length == nil {
		return ""
	}
	switch value := *length; {
	case value <= 8_192:
		return publicContext8K
	case value <= 32_768:
		return publicContext32K
	case value <= 131_072:
		return publicContext128K
	default:
		return publicContext200K
	}
}

// publicContextBuckets 是分档的展示顺序（固定，避免字符串序把 128K 排在
// 32K 前）。
var publicContextBuckets = []string{publicContext8K, publicContext32K, publicContext128K, publicContext200K}

// decodeStringArray 把 jsonb 字符串数组解成 []string；恒返回非 nil（降级
// 契约要求空数组而不是 null）。
func decodeStringArray(raw []byte) []string {
	out := make([]string, 0)
	if len(raw) == 0 {
		return out
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return out
	}
	for _, item := range items {
		if value, ok := item.(string); ok {
			out = append(out, value)
		}
	}
	return out
}

// decodeModelParameters 解析 parameters jsonb（元素形如
// `{name, required, type, default, description}`）。缺名/非法的元素跳过，
// 不让一条脏数据把整张参数表变成空。
//
// 逐元素解析成 json.RawMessage 而不是整体反序列化成 []map[string]any：
// 后者遇到一个非对象元素（数字、字符串、坏 JSON）会整列失败，与注释承诺的
// 「跳过非法元素」不符。
func decodeModelParameters(raw []byte) []PublicModelParameter {
	out := make([]PublicModelParameter, 0)
	if len(raw) == 0 {
		return out
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		// 整列不是数组：没有可解析的元素。
		return out
	}
	for _, item := range items {
		var fields map[string]any
		if err := json.Unmarshal(item, &fields); err != nil {
			continue
		}
		name, _ := fields["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		param := PublicModelParameter{Name: name, Default: fields["default"]}
		param.Required, _ = fields["required"].(bool)
		param.Type, _ = fields["type"].(string)
		param.Description, _ = fields["description"].(string)
		out = append(out, param)
	}
	return out
}

// dedupeFoldPreserveFirst 按大小写折叠去重，保留首次出现的原值作为展示值
// （label/输出）；匹配侧本来就用大小写不敏感比较，因此折叠不改变命中语义。
func dedupeFoldPreserveFirst(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		folded := strings.ToLower(value)
		if _, ok := seen[folded]; ok {
			continue
		}
		seen[folded] = struct{}{}
		out = append(out, value)
	}
	return out
}

type publicCatalogIndex struct {
	models      []publicCatalogModel
	groups      []store.PublicGroup
	groupByName map[string]store.PublicGroup
	overrides   map[string]store.PublicGroupPricing // key: group\x00model
}

func newPublicCatalogIndex(catalog store.PublicPricingCatalog) publicCatalogIndex {
	index := publicCatalogIndex{
		models:      buildPublicCatalogModels(catalog.Models, catalog.Meta, catalog.Vendors),
		groups:      catalog.Groups,
		groupByName: make(map[string]store.PublicGroup, len(catalog.Groups)),
		overrides:   make(map[string]store.PublicGroupPricing, len(catalog.Overrides)),
	}
	for _, group := range catalog.Groups {
		index.groupByName[group.Name] = group
	}
	for _, override := range catalog.Overrides {
		index.overrides[override.Group+"\x00"+override.Model] = override
	}
	return index
}

// filtered 应用 search 与全部 facet 筛选；skip 用来实现「本组自身不参与计数」。
//
// 组内 OR、跨组 AND（D1-18）：每个谓词只要求命中该组已选的任一值，不同组
// 之间顺序 continue，等价于取交集。
type publicFilterSkip struct {
	vendor     bool
	quota      bool
	capability bool
	context    bool
	endpoint   bool
}

func (idx publicCatalogIndex) filtered(query publicPricingQuery, skip publicFilterSkip) []publicCatalogModel {
	out := make([]publicCatalogModel, 0, len(idx.models))
	for _, model := range idx.models {
		if query.Search != "" && !strings.Contains(model.lowerID, strings.ToLower(query.Search)) {
			continue
		}
		if !skip.vendor && len(query.Vendors) > 0 {
			vendor := strings.ToLower(model.vendor)
			matched := false
			for _, selected := range query.Vendors {
				if vendor != "" && vendor == selected {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if !skip.quota && query.QuotaType != "all" {
			if (query.QuotaType == "token" && model.mode != publicPricingModeToken) ||
				(query.QuotaType == "request" && model.mode != publicPricingModeRequest) {
				continue
			}
		}
		if !skip.capability && len(query.Capability) > 0 && !model.hasAnyCapability(query.Capability) {
			continue
		}
		if !skip.context && len(query.Context) > 0 && !model.hasContextBucket(query.Context) {
			continue
		}
		if !skip.endpoint && len(query.Endpoints) > 0 && !model.hasAnyEndpoint(query.Endpoints) {
			continue
		}
		out = append(out, model)
	}
	return out
}

func (m publicCatalogModel) hasAnyCapability(selected []string) bool {
	for _, capability := range m.capabilities {
		for _, want := range selected {
			if strings.EqualFold(capability, want) {
				return true
			}
		}
	}
	return false
}

func (m publicCatalogModel) hasContextBucket(selected []string) bool {
	if m.contextBucket == "" {
		return false
	}
	for _, want := range selected {
		if strings.EqualFold(m.contextBucket, want) {
			return true
		}
	}
	return false
}

func (m publicCatalogModel) hasAnyEndpoint(selected []string) bool {
	for _, endpoint := range m.endpointTypes {
		for _, want := range selected {
			if strings.EqualFold(endpoint, want) {
				return true
			}
		}
	}
	return false
}

// knownVendors 过滤掉目录里不存在的 vendor 值（F-09-22 的「facet 值不存在则丢弃」）。
func (idx publicCatalogIndex) knownVendors(values []string) []string {
	known := make(map[string]struct{}, len(idx.models))
	for _, model := range idx.models {
		if model.vendor != "" {
			known[strings.ToLower(model.vendor)] = struct{}{}
		}
	}
	return filterKnown(values, known)
}

// knownCapabilities / knownContexts / knownEndpoints 同 knownVendors 的
// 「不存在的值丢弃」，但 context/endpoint 的合法取值域是**冻结枚举**而不是
// 目录里当前出现的值：某个分档/端点在目录里暂无命中（如没有 8K 模型、没有
// embeddings 模型）仍是合法筛选，必须返回命中 0 条；只有真正不在冻结枚举
// 里的值才丢弃（否则拼错的值会静默变成「无筛选」返回全集）。
// 资产缺失的模型不贡献已知值，因此筛选未知能力不会误命中降级卡片。
func (idx publicCatalogIndex) knownCapabilities(values []string) []string {
	known := make(map[string]struct{})
	for _, model := range idx.models {
		for _, capability := range model.capabilities {
			if capability != "" {
				known[strings.ToLower(capability)] = struct{}{}
			}
		}
	}
	return filterKnown(values, known)
}

func (idx publicCatalogIndex) knownContexts(values []string) []string {
	known := make(map[string]struct{}, len(publicContextBuckets))
	for _, bucket := range publicContextBuckets {
		known[strings.ToLower(bucket)] = struct{}{}
	}
	return filterKnown(values, known)
}

func (idx publicCatalogIndex) knownEndpoints(values []string) []string {
	known := make(map[string]struct{}, len(store.PublicEndpointMapping))
	for _, entry := range store.PublicEndpointMapping {
		known[strings.ToLower(entry.Value)] = struct{}{}
	}
	return filterKnown(values, known)
}

func filterKnown(values []string, known map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := known[value]; ok {
			out = append(out, value)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// facet 计算
// ---------------------------------------------------------------------------

func (idx publicCatalogIndex) vendorFacets(query publicPricingQuery) []PublicFacetOption {
	// 计费类型筛选仍然生效，唯独忽略 vendor 自身的选择：否则选中 openai 后，
	// 其他供应商的计数会全部变成 0，多选就失去意义。
	models := idx.filtered(query, publicFilterSkip{vendor: true})
	counts := make(map[string]int64)
	labels := make(map[string]string)
	for _, model := range models {
		if model.vendor == "" {
			// 无前缀的模型没有可读供应商；不生成空值 facet（前端无法渲染）。
			continue
		}
		value := strings.ToLower(model.vendor)
		counts[value]++
		if _, ok := labels[value]; !ok {
			labels[value] = model.vendor
		}
	}
	values := make([]string, 0, len(counts))
	for value := range counts {
		values = append(values, value)
	}
	sort.Strings(values)
	out := make([]PublicFacetOption, 0, len(values))
	for _, value := range values {
		out = append(out, PublicFacetOption{Value: value, Label: labels[value], Count: counts[value]})
	}
	return out
}

// capabilityFacets/contextFacets/endpointFacets 遵循同一套 faceted navigation
// 规则（D1-18）：计数只忽略**本组自身**的选择，其它筛选照常生效；无档案
// （或 context_length=null）的模型不产生计数。value 一律小写（与 vendor
// facet 一致，便于 URL 往返），label 保留可读大小写。

func (idx publicCatalogIndex) capabilityFacets(query publicPricingQuery) []PublicFacetOption {
	models := idx.filtered(query, publicFilterSkip{capability: true})
	counts := make(map[string]int64)
	labels := make(map[string]string)
	for _, model := range models {
		for _, capability := range model.capabilities {
			value := strings.ToLower(capability)
			if value == "" {
				continue
			}
			counts[value]++
			// 同一 capability 的多个大小写变体只保留首个原值作为 label，
			// 与装配期的保留首个原值口径一致。
			if _, ok := labels[value]; !ok {
				labels[value] = capability
			}
		}
	}
	values := make([]string, 0, len(counts))
	for value := range counts {
		values = append(values, value)
	}
	sort.Strings(values)
	out := make([]PublicFacetOption, 0, len(values))
	for _, value := range values {
		out = append(out, PublicFacetOption{Value: value, Label: labels[value], Count: counts[value]})
	}
	return out
}

func (idx publicCatalogIndex) contextFacets(query publicPricingQuery) []PublicFacetOption {
	models := idx.filtered(query, publicFilterSkip{context: true})
	counts := make(map[string]int64)
	for _, model := range models {
		if model.contextBucket != "" {
			counts[model.contextBucket]++
		}
	}
	// 固定分档顺序（8K → 32K → 128K → 200K+），只输出有命中的档位。
	out := make([]PublicFacetOption, 0, len(publicContextBuckets))
	for _, bucket := range publicContextBuckets {
		if count := counts[bucket]; count > 0 {
			out = append(out, PublicFacetOption{Value: strings.ToLower(bucket), Label: bucket, Count: count})
		}
	}
	return out
}

func (idx publicCatalogIndex) endpointFacets(query publicPricingQuery) []PublicFacetOption {
	models := idx.filtered(query, publicFilterSkip{endpoint: true})
	counts := make(map[string]int64)
	for _, model := range models {
		for _, endpoint := range model.endpointTypes {
			counts[endpoint]++
		}
	}
	// 固定枚举顺序（store.PublicEndpointMapping），只输出有命中的端点。
	out := make([]PublicFacetOption, 0, len(store.PublicEndpointMapping))
	for _, entry := range store.PublicEndpointMapping {
		if count := counts[entry.Value]; count > 0 {
			out = append(out, PublicFacetOption{
				Value: strings.ToLower(entry.Value), Label: entry.Value, Count: count,
			})
		}
	}
	return out
}

func (idx publicCatalogIndex) pricingTypeFacets(query publicPricingQuery) []PublicPricingTypeFacet {
	// 忽略 quotaType 自身的选择，其它条件（search/vendor）照常生效。
	models := idx.filtered(query, publicFilterSkip{quota: true})
	var token, request int64
	for _, model := range models {
		switch model.mode {
		case publicPricingModeRequest:
			request++
		default:
			token++
		}
	}
	// 只为存在的模式生成候选项；顺序冻结为 token → request（F-09-16 的
	// 「per_token 在 per_request 前」）。
	out := make([]PublicPricingTypeFacet, 0, 2)
	if token > 0 {
		out = append(out, PublicPricingTypeFacet{Value: "token", Count: token})
	}
	if request > 0 {
		out = append(out, PublicPricingTypeFacet{Value: "request", Count: request})
	}
	return out
}

func (idx publicCatalogIndex) groupFacets(filteredCount int64, canExposeRatio bool) []PublicGroupFacet {
	out := make([]PublicGroupFacet, 0, len(idx.groups))
	for _, group := range idx.groups {
		ratio := int32(group.Ratio)
		facet := PublicGroupFacet{
			Value: group.Name, Label: group.Name, Count: filteredCount,
		}
		if canExposeRatio {
			facet.Ratio = &ratio
		}
		out = append(out, facet)
	}
	// user_groups 查询已按 name 排序，这里保持确定性（假实现可能乱序）。
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

// ---------------------------------------------------------------------------
// 列表
// ---------------------------------------------------------------------------

func (h *PublicPricingHandlers) handlePublicPricingList(w http.ResponseWriter, r *http.Request) {
	if !h.allowPublicRead(w, r) {
		return
	}
	query := parsePublicPricingQuery(r)
	catalog, err := h.repo.LoadPublicPricingCatalog(r.Context())
	if err != nil {
		h.log.Error("public pricing: load catalog failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	identity := h.resolver().resolve(r.Context(), r)
	exposeRatios := readConfigBool(h.config, "billing.expose_group_ratios", false)
	idx := newPublicCatalogIndex(catalog)
	// F-09-22 / R-PUB-04：不存在的 facet 值直接丢弃（不是「匹配 0 条」）。
	// 全部值都无效时等于没有该组筛选；只保留部分无效值时其余照常生效。
	query.Vendors = idx.knownVendors(query.Vendors)
	query.Capability = idx.knownCapabilities(query.Capability)
	query.Context = idx.knownContexts(query.Context)
	query.Endpoints = idx.knownEndpoints(query.Endpoints)

	// 显式 group 优先（预览），否则用会话用户分组；不存在的分组被丢弃
	// （R-PUB-04 的「facet 值不存在则丢弃」）。
	targetGroup := ""
	if query.Group != "" {
		if _, ok := idx.groupByName[query.Group]; ok {
			targetGroup = query.Group
		}
	}
	if targetGroup == "" && identity.Authenticated {
		if _, ok := idx.groupByName[identity.Group]; ok {
			targetGroup = identity.Group
		}
	}
	// 只有登录用户或 expose=true 的匿名请求才能看到分组价/倍率（R-PUB-05）。
	canExposeGroupPrice := identity.Authenticated || exposeRatios
	personalized := identity.Authenticated || targetGroup != ""

	matched := idx.filtered(query, publicFilterSkip{})
	sortPublicModels(matched, query.Sort)

	total := int64(len(matched))
	lastPage := int((total + int64(query.PageSize) - 1) / int64(query.PageSize))
	if lastPage < 1 {
		lastPage = 1
	}
	if query.Page > lastPage {
		// 越界页回退最后一页（total=0 时停在 1）；设计 F-09-21 的异常流程。
		query.Page = lastPage
	}
	start := (query.Page - 1) * query.PageSize
	end := start + query.PageSize
	if start > len(matched) {
		start = len(matched)
	}
	if end > len(matched) {
		end = len(matched)
	}

	cards := make([]PublicModelCard, 0, end-start)
	for _, model := range matched[start:end] {
		var groupPrice *PublicGroupPriceView
		if query.IncludeGroupPrice && canExposeGroupPrice && targetGroup != "" {
			groupPrice = idx.groupPrice(targetGroup, model)
		}
		cards = append(cards, h.newPublicModelCard(model, groupPrice))
	}

	etag := publicPricingETag(catalog.Version, "")
	if h.writeConditionalHeaders(w, r, etag, personalized) {
		return
	}
	writeJSON(w, http.StatusOK, PublicPricingList{
		Items:    cards,
		Total:    strconv.FormatInt(total, 10),
		Page:     query.Page,
		PageSize: query.PageSize,
		Facets: PublicFacets{
			Vendors:      idx.vendorFacets(query),
			Capabilities: idx.capabilityFacets(query),
			Context:      idx.contextFacets(query),
			Groups:       idx.groupFacets(total, canExposeGroupPrice),
			Endpoints:    idx.endpointFacets(query),
			PricingTypes: idx.pricingTypeFacets(query),
		},
		Meta: h.meta(catalog),
	})
}

func (h *PublicPricingHandlers) newPublicModelCard(model publicCatalogModel, groupPrice *PublicGroupPriceView) PublicModelCard {
	card := PublicModelCard{
		ModelID: model.rec.Model,
		// 无档案时按 D1-15 降级：display_name=model_id，其余元数据字段为空。
		DisplayName:  model.rec.Model,
		Vendor:       model.vendor,
		Capabilities: []string{},
		Mode:         model.mode,
		PromptPrice:  nullableInt64String(model.rec.PromptPrice),
		IsFree:       publicModelIsFree(model.rec),
		GroupPrice:   groupPrice,
		Endpoints:    []string{},
		Tags:         []string{},
		Description:  "",
	}
	card.PricingSchedule = h.publicPricingSchedule(model.rec)
	if meta := model.meta; meta != nil {
		// display_name 允许空串：空串回退 model_id（D1-16），避免卡片标题行
		// 变成一条不可见/不可点击的空白。下发 TrimSpace 后的值，与判空
		// 口径一致（首尾空白不进入响应）。
		if meta.DisplayName != nil {
			if name := strings.TrimSpace(*meta.DisplayName); name != "" {
				card.DisplayName = name
			}
		}
		// 直接复用装配期已去重的大小写折叠结果，保证卡片输出与 facet
		// 计数/筛选口径一致。
		card.Capabilities = model.capabilities
		card.ContextLength = meta.ContextLength
		card.MaxOutput = meta.MaxOutput
		card.Endpoints = store.DeriveEndpointTypes(meta.Endpoints)
		card.Tags = decodeStringArray(meta.Tags)
		if meta.Description != nil {
			card.Description = *meta.Description
		}
	}
	if model.mode == publicPricingModeToken {
		card.CompletionPrice = nullableInt64String(model.rec.CompletionPrice)
		card.CacheReadPrice = nullableInt64String(model.rec.CacheReadPrice)
		card.CacheWritePrice = nullableInt64String(model.rec.CacheWritePrice)
	}
	// per_request 只展示每次价：输出/缓存价在管理面被禁止，DB 里即使残留
	// 旧值也不对外展示（与 F-09-18 的隐藏逻辑一致）。
	return card
}

// publicPricingSchedule 把 model_pricing.strategy 转成公开时段表。
//
// 解析失败按 null 下发并告警：公开目录是展示面，一行脏策略不应让整个定价
// 目录 500；真正的计费路径仍由 DBSource fail-closed（请求直接失败），所以
// 「展示为无时段」不会把错价伪装成正确价格。
func (h *PublicPricingHandlers) publicPricingSchedule(rec store.PublicModelPricing) *PricingScheduleView {
	if len(rec.Strategy) == 0 {
		return nil
	}
	strategy, err := pricingstrategy.Parse(rec.Strategy)
	if err != nil {
		h.log.Warn("public pricing: invalid time strategy; omitting schedule",
			"model", rec.Model, "error", err)
		return nil
	}
	return newPricingScheduleView(strategy)
}

// publicModelIsFree 实现 R-PUB-03：四项有效价全部显式 0 才是免费，NULL 不算 0。
//
// per_request 只有 prompt_price 一项有效价（其余价列在保存期被拒绝），因此
// 该模式下的「四项有效价」退化为一项——与管理面 modelPricingFree 同口径。
func publicModelIsFree(rec store.PublicModelPricing) bool {
	if rec.PromptPrice == nil || *rec.PromptPrice != 0 {
		return false
	}
	if rec.Mode == 1 {
		return true
	}
	for _, price := range []*int64{rec.CompletionPrice, rec.CacheReadPrice, rec.CacheWritePrice} {
		if price == nil || *price != 0 {
			return false
		}
	}
	return true
}

func nullableInt64String(value *int64) *string {
	if value == nil {
		return nil
	}
	out := strconv.FormatInt(*value, 10)
	return &out
}

// sortPublicModels 实现 F-09-16 的排序口径：
//   - name：名称升序（大小写不敏感），tie-breaker 为原始 model_id；
//   - price_asc/desc：输入价（prompt_price）；
//   - 价格模式下 per_token 恒在 per_request 前（两种单位不可直接比较），
//     组内再按价格排序，NULL 价恒排在本组末尾（R-PUB-03：未知价不能冒充最便宜）；
//   - 最终 tie-breaker 是 model_id，保证 offset 翻页稳定。
func sortPublicModels(models []publicCatalogModel, sortKey string) {
	lessName := func(a, b publicCatalogModel) bool {
		if a.lowerID != b.lowerID {
			return a.lowerID < b.lowerID
		}
		return a.rec.Model < b.rec.Model
	}
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if sortKey == publicPricingSortName {
			return lessName(a, b)
		}
		ra, rb := publicModeRank(a.mode), publicModeRank(b.mode)
		if ra != rb {
			return ra < rb
		}
		pa, pb := a.rec.PromptPrice, b.rec.PromptPrice
		if (pa == nil) != (pb == nil) {
			return pa != nil // NULL 恒在组末
		}
		if pa != nil && *pa != *pb {
			if sortKey == publicPricingSortPriceDesc {
				return *pa > *pb
			}
			return *pa < *pb
		}
		return lessName(a, b)
	})
}

func publicModeRank(mode string) int {
	if mode == publicPricingModeRequest {
		return 1
	}
	return 0
}

// groupPrice 计算分组实付价：覆盖价优先，否则 基准价 × ratio ÷ 100。
//
// 整数运算在服务端完成（F-09-18「前端不做倍率乘法」）。除法按四舍五入
// （+50 后整除）：截断会让每个非整分价格系统性偏低，而四舍五入与既有
// quotaToYuan 的 HALF_UP 展示口径一致。ratio 上界 1000、价上界 2^53−1，
// 乘积仍在 int64 内。
func (idx publicCatalogIndex) groupPrice(group string, model publicCatalogModel) *PublicGroupPriceView {
	rec, ok := idx.groupByName[group]
	if !ok {
		return nil
	}
	if override, exists := idx.overrides[group+"\x00"+model.rec.Model]; exists {
		view := &PublicGroupPriceView{
			Group:      group,
			Ratio:      &rec.Ratio,
			Prompt:     int64StringPtr(override.PromptPrice),
			Completion: int64StringPtr(override.CompletionPrice),
			CacheRead:  int64StringPtr(override.CacheReadPrice),
			CacheWrite: int64StringPtr(override.CacheWritePrice),
			Source:     "override",
		}
		if model.mode == publicPricingModeRequest {
			// per_request 源只读 prompt：覆盖行里残留的输出/缓存价不展示。
			view.Completion, view.CacheRead, view.CacheWrite = nil, nil, nil
		}
		return view
	}
	return &PublicGroupPriceView{
		Group:      group,
		Ratio:      &rec.Ratio,
		Prompt:     ratioPrice(model.rec.PromptPrice, rec.Ratio),
		Completion: ratioPriceForMode(model, model.rec.CompletionPrice, rec.Ratio),
		CacheRead:  ratioPriceForMode(model, model.rec.CacheReadPrice, rec.Ratio),
		CacheWrite: ratioPriceForMode(model, model.rec.CacheWritePrice, rec.Ratio),
		Source:     "ratio",
	}
}

func ratioPriceForMode(model publicCatalogModel, base *int64, ratio int32) *string {
	if model.mode == publicPricingModeRequest {
		return nil
	}
	return ratioPrice(base, ratio)
}

func ratioPrice(base *int64, ratio int32) *string {
	if base == nil {
		return nil
	}
	// half-up：非负价 × 非负倍率，+50 后整除 100。
	applied := (*base*int64(ratio) + 50) / 100
	return int64StringPtr(applied)
}

func int64StringPtr(value int64) *string {
	out := strconv.FormatInt(value, 10)
	return &out
}

// ---------------------------------------------------------------------------
// 详情
// ---------------------------------------------------------------------------

func (h *PublicPricingHandlers) handlePublicPricingDetail(w http.ResponseWriter, r *http.Request) {
	if !h.allowPublicRead(w, r) {
		return
	}
	modelID := r.PathValue("model_id")
	catalog, err := h.repo.LoadPublicPricingCatalog(r.Context())
	if err != nil {
		h.log.Error("public pricing detail: load catalog failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	idx := newPublicCatalogIndex(catalog)
	var model *publicCatalogModel
	for i := range idx.models {
		if idx.models[i].rec.Model == modelID {
			model = &idx.models[i]
			break
		}
	}
	if model == nil {
		// 不存在/停用/无定价/表达式计价统一 404（R-PUB-01/F-09-22 口径 3）：
		// 区分这些分支会让公开面成为「模型是否配置」的探测信道。
		h.writeModelNotFound(w)
		return
	}

	identity := h.resolver().resolve(r.Context(), r)
	exposeRatios := readConfigBool(h.config, "billing.expose_group_ratios", false)
	canExpose := identity.Authenticated || exposeRatios
	personalized := identity.Authenticated

	card := h.newPublicModelCard(*model, nil)
	detail := PublicModelDetail{
		PublicModelCard: card,
		Modalities:      []string{},
		KnowledgeCutoff: nil,
		ReleasedAt:      nil,
		GroupPrices:     []PublicGroupPriceView{},
		Parameters:      []PublicModelParameter{},
		RateLimits:      h.rateLimits(idx.groups),
		// OpenAI 兼容网关的公开认证方式冻结为 Bearer（F-09-27 的接入说明）。
		AuthStyle: "bearer",
	}
	if meta := model.meta; meta != nil {
		// D1-17：详情在卡片字段之外追加档案字段；缺档时保持 D1-15 的空
		// 数组/null，而不是省略键（前端按固定网格渲染）。
		detail.Modalities = decodeStringArray(meta.Modalities)
		detail.KnowledgeCutoff = meta.KnowledgeCutoff
		if meta.ReleasedAt != nil {
			// 契约是 date（YYYY-MM-DD）；DB 列是 date，时区归一后格式化成
			// 日期本身，不带时间部分。
			released := meta.ReleasedAt.UTC().Format("2006-01-02")
			detail.ReleasedAt = &released
		}
		detail.Parameters = decodeModelParameters(meta.Parameters)
	}
	if canExpose {
		for _, group := range idx.groups {
			detail.GroupPrices = append(detail.GroupPrices,
				*idx.groupPrice(group.Name, *model))
		}
		sort.Slice(detail.GroupPrices, func(i, j int) bool {
			return detail.GroupPrices[i].Group < detail.GroupPrices[j].Group
		})
	}

	etag := publicPricingETag(catalog.Version, modelID)
	if h.writeConditionalHeaders(w, r, etag, personalized) {
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// PublicModelMetricsSummary 是公开性能接口的模型级汇总。
//
// 延迟 / TTFT 为毫秒平均值，无样本时为 null；TPS 为
// completion_tokens / (Σ latency_ms / 1000) 的近似值；success_rate 为
// status=1 的占比；failed_24h 固定统计最近 24 小时。
type PublicModelMetricsSummary struct {
	AvgLatencyMs *float64 `json:"avg_latency_ms"`
	AvgTTFTMs    *float64 `json:"avg_ttft_ms"`
	SuccessRate  *float64 `json:"success_rate"`
	TPS          *float64 `json:"tps"`
	Failed24h    *int64   `json:"failed_24h"`
	SampleSize   int64    `json:"sample_size"`
}

type PublicModelMetricsWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type PublicGroupMetricsView struct {
	Group        string   `json:"group"`
	AvgTTFTMs    *float64 `json:"avg_ttft_ms"`
	AvgLatencyMs *float64 `json:"avg_latency_ms"`
	SuccessRate  *float64 `json:"success_rate"`
}

// PublicModelLatestTest 是最近一次管理员主动测速的汇总（契约 latest_test）。
//
// 每个可空统计列保持 JSON null：主动测速的某个分位数在样本不足时可能缺失，
// 用 0 冒充「很快」比留空更有误导性。success_rate/sample_size 由汇总行保证
// 有值（tested_at 来自 created_at）。
type PublicModelLatestTest struct {
	TTFTMSP50    *int     `json:"ttft_ms_p50"`
	TTFTMSP95    *int     `json:"ttft_ms_p95"`
	LatencyMSP50 *int     `json:"latency_ms_p50"`
	LatencyMSP95 *int     `json:"latency_ms_p95"`
	TPS          *float64 `json:"tps"`
	SuccessRate  float64  `json:"success_rate"`
	SampleSize   int      `json:"sample_size"`
	TestedAt     string   `json:"tested_at"`
}

type PublicModelMetricsResponse struct {
	Range    string                    `json:"range"`
	Window   PublicModelMetricsWindow  `json:"window"`
	Summary  PublicModelMetricsSummary `json:"summary"`
	PerGroup []PublicGroupMetricsView  `json:"per_group"`
	// LatestTest 为最近一次主动测速；没有记录/接缝未装配/查询失败时为 null。
	LatestTest *PublicModelLatestTest `json:"latest_test"`
}

// PublicModelMetricsItem 是模型库列表页的单模型指标行。
//
// Summary 恒填（无样本时 sample_size=0，其余统计列为 null）；LatestTest 没有
// 主动测速记录、测速库未装配或读取失败时为 null。
type PublicModelMetricsItem struct {
	ModelID    string                    `json:"model_id"`
	Summary    PublicModelMetricsSummary `json:"summary"`
	LatestTest *PublicModelLatestTest    `json:"latest_test"`
}

// PublicModelMetricsList 是 GET /api/pricing/metrics 的响应信封。
// Items 恒为数组：空目录或窗口内没有任何样本/测速时输出 []，不是 null。
type PublicModelMetricsList struct {
	Range  string                   `json:"range"`
	Window PublicModelMetricsWindow `json:"window"`
	Items  []PublicModelMetricsItem `json:"items"`
}

const (
	publicMetricsCacheControl = "public, max-age=60"
	publicMetricsRange24h     = "24h"
	publicMetricsRange7d      = "7d"

	problemTypeMetricsUnavailable = "https://newtg.dev/problems/metrics-unavailable"
)

func parsePublicMetricsRange(raw string) (time.Duration, string, bool) {
	switch raw {
	case "", publicMetricsRange24h:
		return 24 * time.Hour, publicMetricsRange24h, true
	case publicMetricsRange7d:
		return 7 * 24 * time.Hour, publicMetricsRange7d, true
	default:
		return 0, "", false
	}
}

func roundMillisPtr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	rounded := math.Round(*value)
	return &rounded
}

// publicMetricsSummary 把一次 usage_logs 聚合转成公开汇总视图。
//
// 单模型与批量端点共用同一口径：延迟/TTFT 四舍五入到毫秒；无样本时所有
// 统计列为 null（不是 0）；success_rate/failed_24h 只在有样本时下发；
// TPS 用 completion_tokens / (latency_sum_ms/1000) 的近似值。
func publicMetricsSummary(data store.PublicModelMetrics) PublicModelMetricsSummary {
	summary := PublicModelMetricsSummary{
		AvgLatencyMs: roundMillisPtr(data.AvgLatencyMs),
		AvgTTFTMs:    roundMillisPtr(data.AvgTTFTMs),
		SampleSize:   data.SampleSize,
	}
	if data.SampleSize > 0 {
		successRate := float64(data.SuccessCount) / float64(data.SampleSize)
		summary.SuccessRate = &successRate
		failed := data.Failed24h
		summary.Failed24h = &failed
	}
	if data.LatencySumMs > 0 {
		tps := float64(data.CompletionTokens) / (float64(data.LatencySumMs) / 1000)
		summary.TPS = &tps
	}
	return summary
}

// publicLatestTestView 把主动测速汇总行投影成公开视图（tested_at 归一为 UTC
// RFC3339）。调用方只在确认存在记录时调用，因此返回非 nil。
func publicLatestTestView(test store.ModelPerformanceTest) *PublicModelLatestTest {
	return &PublicModelLatestTest{
		TTFTMSP50:    test.TTFTMSP50,
		TTFTMSP95:    test.TTFTMSP95,
		LatencyMSP50: test.LatencyMSP50,
		LatencyMSP95: test.LatencyMSP95,
		TPS:          test.TPSAvg,
		SuccessRate:  test.SuccessRate,
		SampleSize:   test.Runs,
		TestedAt:     test.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// publicMetricsETag 由模型、窗口、分钟桶与缓存身份派生。
//
// 身份（cacheIdentity）有两类来源，都会改变响应实体，必须折进缓存键：
//   - 最近一次主动测速的 ID/时间：主动测速不进入 usage_logs 聚合，否则
//     管理员刚跑完一次测试，共享缓存仍会回放旧值；
//   - 降级标记（"|degraded"）：日志库故障时 summary 与真实零样本几乎相同，
//     没有标记就会让同一缓存实体在恢复后继续被 304 复用。
//
// 无测速且未降级时身份为空，行为与旧版一致。
func publicMetricsETag(modelID, window string, at time.Time, cacheIdentity string) string {
	sum := sha256.Sum256([]byte(modelID + "|" + window + "|" + cacheIdentity))
	return `"metrics-v1-` + hex.EncodeToString(sum[:])[:16] + `-` +
		strconv.FormatInt(at.Unix()/60, 10) + `"`
}

// handlePublicPricingMetrics 返回模型级真实调用平均值（modules-09 F-09-25）。
// 数据来自日志库 usage_logs；无样本时输出 null，由前端显示 "—"。
func (h *PublicPricingHandlers) handlePublicPricingMetrics(w http.ResponseWriter, r *http.Request) {
	if !h.allowPublicRead(w, r) {
		return
	}
	window, label, ok := parsePublicMetricsRange(r.URL.Query().Get("range"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid range")
		return
	}
	if h.metrics == nil {
		writeProblem(w, http.StatusServiceUnavailable, problemTypeMetricsUnavailable,
			"Performance data unavailable")
		return
	}

	modelID := r.PathValue("model_id")
	catalog, err := h.repo.LoadPublicPricingCatalog(r.Context())
	if err != nil {
		h.log.Error("public pricing metrics: load catalog failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	idx := newPublicCatalogIndex(catalog)
	visible := false
	for i := range idx.models {
		if idx.models[i].rec.Model == modelID {
			visible = true
			break
		}
	}
	if !visible {
		h.writeModelNotFound(w)
		return
	}

	end := time.Now().UTC()
	start := end.Add(-window)
	data, err := h.metrics.LoadPublicModelMetrics(r.Context(), modelID, start, end)
	degraded := false
	if err != nil {
		// 首页只消费 latest_test；日志库故障不应把整张表变成「暂不可用」。
		// summary 降级为零样本（各列为 null），latest_test 照常返回。
		h.log.Warn("public pricing metrics: load usage summary failed; degrading summary",
			"model", modelID, "error", err)
		data = store.PublicModelMetrics{}
		degraded = true
	}

	summary := publicMetricsSummary(data)

	groups := make([]PublicGroupMetricsView, 0, len(data.Groups))
	for _, group := range data.Groups {
		view := PublicGroupMetricsView{
			Group:        group.Group,
			AvgLatencyMs: roundMillisPtr(group.AvgLatencyMs),
			AvgTTFTMs:    roundMillisPtr(group.AvgTTFTMs),
		}
		if group.SampleSize > 0 {
			successRate := float64(group.SuccessCount) / float64(group.SampleSize)
			view.SuccessRate = &successRate
		}
		groups = append(groups, view)
	}

	// 目录过滤（store 层）已经排除没有任何可调度渠道的模型；本端点的目录快照
	// 也是每次请求重新读取的，所以「共享缓存里的旧目录副本」并不会把访客带到
	// 这里——那类访客会先在目录快照里查不到该模型，直接拿到 404（这是期望
	// 语义，不要为兼容旧副本而放宽）。这里的复查只覆盖一个很窄的竞态：本次
	// 请求刚读完目录快照、模型当时确实可路由，随后毫秒级内渠道被禁用；此时
	// 不应再回放一份已无渠道可服务的测速读数。
	//
	// 不可调度时不下发 latest_test（前端显示 —），测速身份同时归空——与「从未
	// 测过」走同一 ETag，避免客户端拿旧 ETag 命中 304 继续回放旧读数。
	routable := true
	if ok, channelErr := h.repo.HasRoutableChannel(r.Context(), modelID); channelErr != nil {
		// fail-open：渠道可用性查询失败不应把公开指标端点打成 503。保留
		// latest_test，与 perf 存储故障的降级风格一致（宁可多展示一次读数，
		// 也不能让一次辅助查询拖垮整张性能表）。
		h.log.Warn("public pricing metrics: routable channel check failed; failing open",
			"model", modelID, "error", channelErr)
	} else {
		routable = ok
	}

	// 主动测速是独立事实源（主库），失败不影响真实调用聚合：日志告警后
	// latest_test 留空，响应仍是 200（fail-open）。不可调度时整段跳过：
	// 读数不会下发，也不必再查一次主库。
	testIdentity := ""
	var latestTest *PublicModelLatestTest
	if routable {
		if h.perf == nil {
			h.log.Warn("public pricing metrics: performance store not configured; latest_test omitted",
				"model", modelID)
		} else if test, found, perfErr := h.perf.LatestByModel(r.Context(), modelID); perfErr != nil {
			h.log.Warn("public pricing metrics: latest performance test unavailable; failing open",
				"model", modelID, "error", perfErr)
		} else if found {
			latestTest = publicLatestTestView(test)
			testIdentity = strconv.FormatInt(test.ID, 10) + "@" +
				test.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
	}

	// 降级结果的 summary 与正常零样本几乎相同，必须让缓存键可区分，避免
	// 同一缓存实体被跨状态复用（例如日志库恢复后仍以 304 命中降级表示）。
	// 注意 Cache-Control 仍是 max-age=60：已有副本的最长存活时间不变。
	if degraded {
		testIdentity += "|degraded"
	}

	response := PublicModelMetricsResponse{
		Range: label,
		Window: PublicModelMetricsWindow{
			Start: start.Format(time.RFC3339),
			End:   end.Format(time.RFC3339),
		},
		Summary:    summary,
		PerGroup:   groups,
		LatestTest: latestTest,
	}

	etag := publicMetricsETag(modelID, label, end, testIdentity)
	w.Header().Set("Cache-Control", publicMetricsCacheControl)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// publicMetricsListETag 由可见模型集合、窗口、分钟桶与每个模型最近一次主动
// 测速的身份派生。
//
// 主动测速不进入 usage_logs 聚合，必须把它的 id/created_at 折进缓存键：否则
// 管理员刚跑完一次测试，共享缓存仍会在 60 秒内回放旧值。测试身份排序后参与
// 哈希，保证 map 迭代顺序不影响 ETag（同分钟同数据 → 同 ETag）。
func publicMetricsListETag(
	models []string,
	window string,
	at time.Time,
	tests map[string]store.ModelPerformanceTest,
) string {
	identities := make([]string, 0, len(tests))
	for model, test := range tests {
		identities = append(identities, model+"@"+strconv.FormatInt(test.ID, 10)+"@"+
			test.CreatedAt.UTC().Format(time.RFC3339Nano))
	}
	sort.Strings(identities)
	minute := strconv.FormatInt(at.Unix()/60, 10)
	material := strings.Join(models, "\x00") + "|" + window + "|" + minute + "|" +
		strings.Join(identities, "\x00")
	sum := sha256.Sum256([]byte(material))
	return `"metrics-list-v1-` + hex.EncodeToString(sum[:])[:16] + `-` + minute + `"`
}

// latestTestsByModels 批量读取每个可见模型最近一次主动测速。
//
// 主动测速是独立事实源（主库），读取失败只告警并返回空 map：端点保持 200、
// latest_test 全为 null（fail-open，与单模型端点同口径）。接缝未装配时同样
// 只告警；空模型集合不触发查询。
func (h *PublicPricingHandlers) latestTestsByModels(
	ctx context.Context,
	models []string,
) map[string]store.ModelPerformanceTest {
	if h.perf == nil {
		h.log.Warn("public pricing metrics list: performance store not configured; latest_test omitted")
		return nil
	}
	if len(models) == 0 {
		return nil
	}
	tests, err := h.perf.LatestByModels(ctx, models)
	if err != nil {
		h.log.Warn("public pricing metrics list: latest performance tests unavailable; failing open",
			"error", err)
		return nil
	}
	return tests
}

// handlePublicPricingMetricsList 返回目录内所有模型的窗口汇总（模型库列表页）。
//
// 只输出窗口内有真实调用样本（sample_size > 0）或存在主动测速记录的可见模型；
// 无数据的模型不出现在 items 中（定价列表仍能看到它们）。items 恒为数组。
// 与单模型端点共用可见性规则、窗口解析、summary/latest_test 视图与 fail-open
// 语义，并下发共享缓存头（public, max-age=60 + ETag）。
func (h *PublicPricingHandlers) handlePublicPricingMetricsList(w http.ResponseWriter, r *http.Request) {
	if !h.allowPublicRead(w, r) {
		return
	}
	window, label, ok := parsePublicMetricsRange(r.URL.Query().Get("range"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid range")
		return
	}
	if h.metrics == nil {
		writeProblem(w, http.StatusServiceUnavailable, problemTypeMetricsUnavailable,
			"Performance data unavailable")
		return
	}

	catalog, err := h.repo.LoadPublicPricingCatalog(r.Context())
	if err != nil {
		h.log.Error("public pricing metrics list: load catalog failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	// 与列表端点同一可见性规则（含 status=2 档案剔除）与目录顺序。
	idx := newPublicCatalogIndex(catalog)
	models := make([]string, 0, len(idx.models))
	for _, model := range idx.models {
		models = append(models, model.rec.Model)
	}

	end := time.Now().UTC()
	start := end.Add(-window)

	// 空目录不查日志库（批量方法本身也会短路，这里省掉一次跨库调用语义）。
	metrics := make(map[string]store.PublicModelMetrics)
	if len(models) > 0 {
		metrics, err = h.metrics.LoadPublicModelMetricsBatch(r.Context(), models, start, end)
		if err != nil {
			h.log.Error("public pricing metrics list: load metrics failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
	}
	tests := h.latestTestsByModels(r.Context(), models)

	items := make([]PublicModelMetricsItem, 0, len(models))
	for _, model := range idx.models {
		data := metrics[model.rec.Model]
		test, hasTest := tests[model.rec.Model]
		// 无样本且无测速 = 窗口内没有可展示事实；不输出「全 null」占位行。
		if data.SampleSize <= 0 && !hasTest {
			continue
		}
		item := PublicModelMetricsItem{
			ModelID: model.rec.Model,
			Summary: publicMetricsSummary(data),
		}
		if hasTest {
			item.LatestTest = publicLatestTestView(test)
		}
		items = append(items, item)
	}

	response := PublicModelMetricsList{
		Range: label,
		Window: PublicModelMetricsWindow{
			Start: start.Format(time.RFC3339),
			End:   end.Format(time.RFC3339),
		},
		Items: items,
	}

	etag := publicMetricsListETag(models, label, end, tests)
	w.Header().Set("Cache-Control", publicMetricsCacheControl)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *PublicPricingHandlers) writeModelNotFound(w http.ResponseWriter) {
	detail := "The requested model does not exist or is unavailable."
	writeProblemDetail(w, http.StatusNotFound, problemTypeModelNotFound, "Model not found", detail)
}

// rateLimits 从 rl.group_overrides / rl.user_model_rpm 构造详情页限速表。
//
// 配置缺失时返回空数组（「若可得」）：不编造 0 RPM，也不把「未配置」写成
// 一个看起来像限制的数字——0 在契约里的含义是「不限」。
func (h *PublicPricingHandlers) rateLimits(groups []store.PublicGroup) []PublicRateLimit {
	if h.config == nil || len(groups) == 0 {
		return []PublicRateLimit{}
	}
	defaultRPM, hasDefault := readConfigInt64(h.config, "rl.user_model_rpm")
	if !hasDefault {
		// 配置中心只返回「已落库」的键；未修改过的站点没有该行。这里回落到
		// 注册表默认值，保证公开详情页的限速表与模块 08 的管理默认一致，
		// 而不是凭空消失一列。
		if setting, ok := settings.Lookup("rl.user_model_rpm"); ok {
			if value, ok := settings.DefaultValue(setting).(int64); ok {
				defaultRPM, hasDefault = value, true
			}
		}
	}
	overrides := map[string]int64{}
	if raw, ok := h.config.Get("rl.group_overrides"); ok {
		var parsed map[string]int64
		if err := json.Unmarshal(raw, &parsed); err == nil {
			overrides = parsed
		}
	}
	out := make([]PublicRateLimit, 0, len(groups))
	for _, group := range groups {
		value, ok := overrides[group.Name]
		if !ok {
			if !hasDefault {
				continue
			}
			value = defaultRPM
		}
		out = append(out, PublicRateLimit{Group: group.Name, UserModelRPM: value})
	}
	return out
}

// ---------------------------------------------------------------------------
// 缓存头 / ETag / 元数据
// ---------------------------------------------------------------------------

// writeConditionalHeaders 写出 ETag 与缓存策略；命中 If-None-Match 时直接
// 返回 304（无 body）并返回 true。
//
// 【个性化响应不进共享缓存】带有效会话（或显式分组）的响应含该用户的实付价，
// 若仍标 public，中间缓存可能把 A 用户的分组价回给匿名访客。personalized 时
// 降级为 private，同时 Vary: Cookie 让任何忽略 private 的缓存也按会话分区。
func (h *PublicPricingHandlers) writeConditionalHeaders(w http.ResponseWriter,
	r *http.Request, etag string, personalized bool) bool {
	header := w.Header()
	header.Set("ETag", etag)
	vary := "Accept-Encoding"
	if personalized {
		header.Set("Cache-Control", publicPricingPrivateCacheControl)
		vary += ", Cookie"
	} else {
		header.Set("Cache-Control", publicPricingCacheControl)
	}
	header.Set("Vary", vary)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func etagMatches(headerValue, etag string) bool {
	for _, raw := range strings.Split(headerValue, ",") {
		candidate := strings.TrimSpace(raw)
		// RFC 9110 §13.1.2：`*` 匹配任意当前表示（客户端只想确认资源存在）。
		if candidate == "*" {
			return true
		}
		// 弱比较：`W/"x"` 与 `"x"` 在 GET 语义下等价（RFC 9110 §8.8.3.2）。
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// publicPricingETag 由定价域版本与（详情时的）模型名派生。
//
// 不把模型名直接拼进头部：model_id 允许 `/` 与冒号，虽然对 ETag 引号值合法，
// 但哈希后长度固定、也不受未来字符集变化影响。
func publicPricingETag(version, modelID string) string {
	if modelID == "" {
		return `"pricing-v` + version + `"`
	}
	sum := sha256.Sum256([]byte(modelID))
	return `"pricing-v` + version + `-` + hex.EncodeToString(sum[:])[:16] + `"`
}

// meta 构造展示元数据。
//
// 【契约观察项 O1（已知、未在本次修正）】ETag 只由定价域版本派生，不含筛选
// 条件或请求身份；`generated_at` 是本次快照的生成时刻，因此同一 ETag 下不同
// 请求的 body 可能只在 generated_at 上不同。304 响应没有 body，缓存语义仍然
// 正确（版本未变即可复用本地表示）；若未来需要逐请求内容摘要，应把筛选参数
// 纳入 ETag 派生，而不是依赖 generated_at。当前保留版本级 ETag 是为了让不同
// 筛选条件共享同一个「定价是否变化」的缓存键（F-09-14 的版本语义）。
func (h *PublicPricingHandlers) meta(catalog store.PublicPricingCatalog) PublicPricingMeta {
	symbol := readConfigString(h.config, "billing.currency_symbol")
	if symbol == "" {
		symbol = "¥"
	}
	quotaPerUnit, ok := readConfigInt64(h.config, "billing.quota_per_unit")
	if !ok || quotaPerUnit < 1 || quotaPerUnit > 1_000_000 {
		quotaPerUnit = pricing.DefaultQuotaPerUnit
	}
	generated := catalog.GeneratedAt
	if generated.IsZero() {
		generated = time.Now().UTC()
	}
	return PublicPricingMeta{
		CreditSymbol:    symbol,
		QuotaPerUnit:    quotaPerUnit,
		USDExchangeRate: usdExchangeRate,
		Version:         catalog.Version,
		GeneratedAt:     generated.UTC().Format(time.RFC3339),
	}
}

// allowPublicRead 消耗 B8 公开只读桶；Redis 故障 fail-open（security-07 §2.3）。
func (h *PublicPricingHandlers) allowPublicRead(w http.ResponseWriter, r *http.Request) bool {
	if h.rdb == nil {
		return true
	}
	over, retry, err := allowRequestRate(r.Context(), h.rdb,
		PublicReadRateLimitKey(clientIP(r), r.URL.Path), PublicReadRateLimit, PublicReadRateWindow)
	if err != nil {
		h.log.Warn("public pricing rate limiter unavailable; failing open", "error", err)
		return true
	}
	if over {
		writeRateLimited(w, retry, "Too many requests")
		return false
	}
	return true
}

func (h *PublicPricingHandlers) resolver() *publicSessionResolver {
	return &publicSessionResolver{sessions: h.sessions, groups: h.groups}
}

// readConfigInt64 读取整数配置；非整数/缺失时 ok=false。
func readConfigInt64(config ConfigReader, key string) (int64, bool) {
	if config == nil {
		return 0, false
	}
	raw, ok := config.Get(key)
	if !ok {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}
