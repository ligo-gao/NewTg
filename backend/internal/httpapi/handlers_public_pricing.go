// 公开定价目录端点（modules-09 F-09-14~F-09-22；M6c Task 7；R-PUB-01~05）。
//
// 端点面：
//
//	GET /api/pricing                     → PublicPricingList
//	GET /api/pricing/{model_id}          → PublicModelDetail（URL 编码的 `/`）
//	GET /api/pricing/{model_id}/metrics  → 404（P1，本任务只占位，不返回伪造数据）
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing/pricing"
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
}

// PublicPricingHandlers 持有公开目录端点依赖，可并发使用。
type PublicPricingHandlers struct {
	repo     store.PublicPricingRepo
	config   ConfigReader
	sessions *auth.SessionManager
	groups   PublicUserGroupLookup
	rdb      *redis.Client
	log      *slog.Logger
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
	}, nil
}

// RegisterPublicPricingRoutes 注册公开目录端点（精确模式，绕过 SessionAuth）。
func (h *PublicPricingHandlers) RegisterPublicPricingRoutes(rt *Router) {
	rt.HandlePublic("GET /api/pricing", http.HandlerFunc(h.handlePublicPricingList))
	rt.HandlePublic("GET /api/pricing/{model_id}", http.HandlerFunc(h.handlePublicPricingDetail))
	rt.HandlePublic("GET /api/pricing/{model_id}/metrics", http.HandlerFunc(h.handlePublicPricingMetrics))
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
	Endpoints       []string              `json:"endpoints"`
	Tags            []string              `json:"tags"`
	Description     string                `json:"description"`
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

// PublicFacets 与 F-09-15 的六组一一对应。capabilities/context/endpoints
// 在元数据落库前恒为空数组（R-PUB-01 的降级口径）。
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

// PublicModelParameter 是详情页参数表行（本波恒为空数组，元数据未落库）。
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
	Page              int
	PageSize          int
	Search            string
	Sort              string
	Vendors           []string // 已小写、去重
	Group             string
	QuotaType         string // all | token | request
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
	seen := make(map[string]struct{})
	for _, raw := range q["vendor"] {
		for _, part := range strings.Split(raw, ",") {
			value := strings.ToLower(strings.TrimSpace(part))
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			if len(query.Vendors) >= publicPricingMaxVendorFilters {
				break
			}
			seen[value] = struct{}{}
			query.Vendors = append(query.Vendors, value)
		}
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

// ---------------------------------------------------------------------------
// 目录索引
// ---------------------------------------------------------------------------

// publicCatalogModel 是内存过滤/排序的工作项。
type publicCatalogModel struct {
	rec     store.PublicModelPricing
	vendor  string // 原始大小写的前缀，无 `/` 时为空
	mode    string
	lowerID string
}

func buildPublicCatalogModels(rows []store.PublicModelPricing) []publicCatalogModel {
	models := make([]publicCatalogModel, 0, len(rows))
	for _, row := range rows {
		mode := publicPricingModeToken
		if row.Mode == 1 {
			mode = publicPricingModeRequest
		}
		models = append(models, publicCatalogModel{
			rec: row, vendor: vendorPrefix(row.Model), mode: mode,
			lowerID: strings.ToLower(row.Model),
		})
	}
	return models
}

// vendorPrefix 按 R-PUB-02 从 model_id 的首个 `/` 前派生供应商。
func vendorPrefix(model string) string {
	if index := strings.Index(model, "/"); index > 0 {
		return model[:index]
	}
	return ""
}

type publicCatalogIndex struct {
	models      []publicCatalogModel
	groups      []store.PublicGroup
	groupByName map[string]store.PublicGroup
	overrides   map[string]store.PublicGroupPricing // key: group\x00model
}

func newPublicCatalogIndex(catalog store.PublicPricingCatalog) publicCatalogIndex {
	index := publicCatalogIndex{
		models:      buildPublicCatalogModels(catalog.Models),
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

// filtered 应用 search/vendor/quotaType；skip 用来实现「本组自身不参与计数」。
type publicFilterSkip struct {
	vendor bool
	quota  bool
}

// knownVendors 过滤掉目录里不存在的 vendor 值（F-09-22 的「facet 值不存在则丢弃」）。
func (idx publicCatalogIndex) knownVendors(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(idx.models))
	for _, model := range idx.models {
		if model.vendor != "" {
			known[strings.ToLower(model.vendor)] = struct{}{}
		}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := known[value]; ok {
			out = append(out, value)
		}
	}
	return out
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
		out = append(out, model)
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
	// 全部值都无效时等于没有 vendor 筛选；只保留部分无效值时其余照常生效。
	query.Vendors = idx.knownVendors(query.Vendors)

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
		cards = append(cards, newPublicModelCard(model, groupPrice))
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
			Capabilities: []PublicFacetOption{},
			Context:      []PublicFacetOption{},
			Groups:       idx.groupFacets(total, canExposeGroupPrice),
			Endpoints:    []PublicFacetOption{},
			PricingTypes: idx.pricingTypeFacets(query),
		},
		Meta: h.meta(catalog),
	})
}

func newPublicModelCard(model publicCatalogModel, groupPrice *PublicGroupPriceView) PublicModelCard {
	card := PublicModelCard{
		ModelID: model.rec.Model,
		// 展示名元数据未落库（R-PUB-01）：回退为 model_id，不编造。
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
	if model.mode == publicPricingModeToken {
		card.CompletionPrice = nullableInt64String(model.rec.CompletionPrice)
		card.CacheReadPrice = nullableInt64String(model.rec.CacheReadPrice)
		card.CacheWritePrice = nullableInt64String(model.rec.CacheWritePrice)
	}
	// per_request 只展示每次价：输出/缓存价在管理面被禁止，DB 里即使残留
	// 旧值也不对外展示（与 F-09-18 的隐藏逻辑一致）。
	return card
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

	card := newPublicModelCard(*model, nil)
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

// handlePublicPricingMetrics 是 P1 指标的显式占位：只注册 404，绝不返回
// 伪造数据（brief 冻结）。返回独立 type，避免前端把「未实现」当成「模型下架」。
func (h *PublicPricingHandlers) handlePublicPricingMetrics(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, http.StatusNotFound, problemTypeNotImplemented, "Not Implemented")
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
