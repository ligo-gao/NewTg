// 定价管理面端点（modules-06 F-06-13/14/16；M3 Task 7）。
//
// 端点面（路径以 spec 为准：管理面在 /api/model-pricing、/api/group-pricing；
// /api/pricing 是模块 09 的公开目录，不得复用——见 modules-06 G-5 命名空间裁决）：
//
//	GET    /api/model-pricing                      offset 分页（pricing:read）
//	POST   /api/model-pricing                      Idempotency-Key（pricing:manage）
//	GET    /api/model-pricing/models               模型选择器（pricing:read）
//	POST   /api/model-pricing:preview              spec + 样例变量求值（pricing:read）
//	PATCH  /api/model-pricing/{model}              updated_at 乐观锁（pricing:manage）
//	DELETE /api/model-pricing/{model}              updated_at 乐观锁（pricing:manage）
//	POST   /api/model-pricing/{model}:enable|:disable
//
//	GET    /api/group-pricing                      offset 分页（pricing:read）
//	POST   /api/group-pricing                      Idempotency-Key（pricing:manage）
//	PATCH  /api/group-pricing/{group}/{model}      updated_at 乐观锁
//	DELETE /api/group-pricing/{group}/{model}      updated_at 乐观锁
//	POST   /api/group-pricing/{group}/{model}:enable|:disable
//
// 设计要点：
//   - 价格列是**整数积分**（R9）；NULL=未设置，0=显式免费。出参不做 float 金额，
//     展示换算（quota_per_unit，默认 100，元 = 积分/100 四舍五入到分）只在
//     preview 的 amount_yuan 中体现，避免把展示精度写回存储。
//   - 表达式校验复用 pricing.ParsePricing（编译 + 变量白名单），非法即 400；
//     预览求值复用 pricing.ComputeQuota，不做第二套计价实现。
//   - 所有写操作都登记 FieldPolicy：M1 的 RBAC 对未登记动作 fail-closed 403。
//
// Go 1.22 ServeMux 不允许 `{model}` 与字面量 `:enable` 共存于同一段，故动作端点
// 用 `{rest...}` 子树在 handler 内解析 `<model>:<action>`（与令牌端点同构）。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const (
	pricingModePerToken   = "per_token"
	pricingModePerRequest = "per_request"
	pricingModeTieredExpr = "tiered_expr"

	// maxPricingPrice 与 pricing.Source 的 exactPrice 上界一致（2^53−1）。
	//
	// 【为什么不用 bigint 的 9.2e18】PriceData 的价格字段是 float64（契约冻结），
	// 超过 2^53−1 会在进入计费快照时丢精度，Source 会拒绝该行。管理面若允许
	// 保存这种值，模型会在结算热路径才失败；保存期拒绝是 fail-closed。
	maxPricingPrice = int64(1)<<53 - 1

	pricingModelMaxRunes = 128
	pricingTextMaxRunes  = 64
	pricingModelLimitMax = 1000
	pricingModelLimitDef = 200
)

// pricingModelPattern 与 modules-06 F-06-14 的模型名字符集一致。
var pricingModelPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// PricingHandlerConfig 是定价端点的装配输入。Repo 必填（生产由
// store.NewPricingRepo(DB) 构造）；Redis 用于创建幂等；Authz 缺省时构造空中间件
// （字段策略仍是端点契约的一部分，不能因装配方未启用 RBAC 而失效）。
type PricingHandlerConfig struct {
	Repo   store.PricingRepo
	Redis  *redis.Client
	Authz  *rbac.AuthzMiddleware
	Logger *slog.Logger
	// OnChange 在定价写成功后触发（进程内缓存失效接缝）。M3 的 DBSource
	// 以 30s TTL 兜底；T9 装配引擎后可把它接到 Source.Invalidate。
	OnChange func()
}

// PricingHandlers 持有定价端点依赖，可并发使用。
type PricingHandlers struct {
	repo     store.PricingRepo
	rdb      *redis.Client
	authz    *rbac.AuthzMiddleware
	log      *slog.Logger
	onChange func()
}

var (
	pricingListAction    = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "list"}
	pricingModelsAction  = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "models"}
	pricingPreviewAction = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "preview"}
	pricingCreateAction  = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "create"}
	pricingUpdateAction  = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "update"}
	pricingDeleteAction  = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "delete"}
	pricingStatusAction  = rbac.AuthzAction{ResourceKind: "pricing", ActionName: "status"}

	groupPricingListAction   = rbac.AuthzAction{ResourceKind: "group_pricing", ActionName: "list"}
	groupPricingCreateAction = rbac.AuthzAction{ResourceKind: "group_pricing", ActionName: "create"}
	groupPricingUpdateAction = rbac.AuthzAction{ResourceKind: "group_pricing", ActionName: "update"}
	groupPricingDeleteAction = rbac.AuthzAction{ResourceKind: "group_pricing", ActionName: "delete"}
	groupPricingStatusAction = rbac.AuthzAction{ResourceKind: "group_pricing", ActionName: "status"}
)

// NewPricingHandlers 构造定价端点。
func NewPricingHandlers(cfg PricingHandlerConfig) (*PricingHandlers, error) {
	if cfg.Repo == nil {
		return nil, errors.New("httpapi: PricingHandlerConfig.Repo is required")
	}
	if cfg.Redis == nil {
		// 幂等键依赖 Redis；缺失时创建端点会失去重放保护。装配期拒绝，
		// 而不是静默退化成「每次新建」。
		return nil, errors.New("httpapi: PricingHandlerConfig.Redis is required (idempotency)")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &PricingHandlers{
		repo: cfg.Repo, rdb: cfg.Redis, authz: authz, log: logger, onChange: cfg.OnChange,
	}
	h.registerPolicies()
	return h, nil
}

// registerPolicies 登记每个写动作的字段白名单。字段层是端点契约的一部分：
// 未登记动作对任何写请求都 403（security-02 §2.6.1 规则 6）。
func (h *PricingHandlers) registerPolicies() {
	priceFields := func(extra ...string) *rbac.FieldPolicy {
		fields := map[string]*rbac.FieldPolicy{
			"model":             nil,
			"mode":              nil,
			"billing_expr":      nil,
			"prompt_price":      nil,
			"completion_price":  nil,
			"cache_read_price":  nil,
			"cache_write_price": nil,
			"author":            nil,
			"vendor":            nil,
			"status":            nil,
		}
		for _, name := range extra {
			fields[name] = nil
		}
		return &rbac.FieldPolicy{Fields: fields}
	}
	h.authz.SetWritePolicy(pricingCreateAction, priceFields())
	// 更新不允许改 model（自然键）；字段层提前 403，而不是让 handler 静默忽略。
	updatePolicy := priceFields("updated_at")
	delete(updatePolicy.Fields, "model")
	h.authz.SetWritePolicy(pricingUpdateAction, updatePolicy)
	h.authz.SetWritePolicy(pricingDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"updated_at": nil,
	}})
	h.authz.SetWritePolicy(pricingStatusAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"updated_at": nil,
	}})
	// preview 是 POST（写方法）但语义只读：仍必须登记策略，否则字段层会
	// 以 policy_missing 拒绝一个合法的只读预览。
	h.authz.SetWritePolicy(pricingPreviewAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"spec":        nil,
		"model":       nil,
		"group_ratio": nil,
		"params":      nil,
	}})

	groupFields := func(extra ...string) *rbac.FieldPolicy {
		fields := map[string]*rbac.FieldPolicy{
			"group":             nil,
			"model":             nil,
			"prompt_price":      nil,
			"completion_price":  nil,
			"cache_read_price":  nil,
			"cache_write_price": nil,
			"status":            nil,
		}
		for _, name := range extra {
			fields[name] = nil
		}
		return &rbac.FieldPolicy{Fields: fields}
	}
	h.authz.SetWritePolicy(groupPricingCreateAction, groupFields())
	// 更新不允许改 group/model（联合自然键）。
	groupUpdatePolicy := groupFields("updated_at")
	delete(groupUpdatePolicy.Fields, "group")
	delete(groupUpdatePolicy.Fields, "model")
	h.authz.SetWritePolicy(groupPricingUpdateAction, groupUpdatePolicy)
	h.authz.SetWritePolicy(groupPricingDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"updated_at": nil,
	}})
	h.authz.SetWritePolicy(groupPricingStatusAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"updated_at": nil,
	}})
}

// RegisterPricingRoutes 注册全部定价端点。wrap 语义同 RegisterUserRoutes。
func (h *PricingHandlers) RegisterPricingRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	fixed := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(required, action)(next)
	}

	mount("GET /api/model-pricing", fixed(rbac.PermPricingRead, pricingListAction, h.handleListModelPricing))
	mount("POST /api/model-pricing", fixed(rbac.PermPricingManage, pricingCreateAction,
		h.idempotentCreate("model", h.handleCreateModelPricing)))
	mount("GET /api/model-pricing/models", fixed(rbac.PermPricingRead, pricingModelsAction, h.handleListPricingModels))
	mount("POST /api/model-pricing:preview", fixed(rbac.PermPricingRead, pricingPreviewAction, h.handlePreviewPricing))
	mount("PATCH /api/model-pricing/{model}", fixed(rbac.PermPricingManage, pricingUpdateAction, h.handleUpdateModelPricing))
	mount("DELETE /api/model-pricing/{model}", fixed(rbac.PermPricingManage, pricingDeleteAction, h.handleDeleteModelPricing))
	// 动作端点：{model}:enable / {model}:disable（Go 1.22 不允许同段混排）。
	mount("POST /api/model-pricing/{rest...}", fixed(rbac.PermPricingManage, pricingStatusAction, h.handleModelPricingAction))

	mount("GET /api/group-pricing", fixed(rbac.PermPricingRead, groupPricingListAction, h.handleListGroupPricing))
	mount("POST /api/group-pricing", fixed(rbac.PermPricingManage, groupPricingCreateAction,
		h.idempotentCreate("group", h.handleCreateGroupPricing)))
	mount("PATCH /api/group-pricing/{rest...}", fixed(rbac.PermPricingManage, groupPricingUpdateAction, h.handleUpdateGroupPricing))
	mount("DELETE /api/group-pricing/{rest...}", fixed(rbac.PermPricingManage, groupPricingDeleteAction, h.handleDeleteGroupPricing))
	mount("POST /api/group-pricing/{rest...}", fixed(rbac.PermPricingManage, groupPricingStatusAction, h.handleGroupPricingAction))
}

// notifyChange 在写成功后调用缓存失效接缝；失败不影响响应（缓存有 TTL 兜底）。
func (h *PricingHandlers) notifyChange() {
	if h.onChange != nil {
		h.onChange()
	}
}

// ---------------------------------------------------------------------------
// 响应 DTO
// ---------------------------------------------------------------------------

type modelPricingView struct {
	ID              int64     `json:"id"`
	Model           string    `json:"model"`
	Mode            string    `json:"mode"`
	BillingExpr     *string   `json:"billing_expr"`
	PromptPrice     *int64    `json:"prompt_price"`
	CompletionPrice *int64    `json:"completion_price"`
	CacheReadPrice  *int64    `json:"cache_read_price"`
	CacheWritePrice *int64    `json:"cache_write_price"`
	Author          string    `json:"author"`
	Vendor          *string   `json:"vendor"`
	Status          int16     `json:"status"`
	Free            bool      `json:"free"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type modelPricingListView struct {
	Items        []modelPricingView `json:"items"`
	Total        int64              `json:"total"`
	Page         int                `json:"page"`
	PageSize     int                `json:"page_size"`
	QuotaPerUnit int64              `json:"quota_per_unit"`
}

type pricingModelOptionView struct {
	Model  string `json:"model"`
	Mode   string `json:"mode"`
	Status int16  `json:"status"`
}

type pricingModelListView struct {
	Items []pricingModelOptionView `json:"items"`
}

type groupPricingView struct {
	ID              int64     `json:"id"`
	Group           string    `json:"group"`
	Model           string    `json:"model"`
	Mode            string    `json:"mode"`
	PromptPrice     int64     `json:"prompt_price"`
	CompletionPrice int64     `json:"completion_price"`
	CacheReadPrice  int64     `json:"cache_read_price"`
	CacheWritePrice int64     `json:"cache_write_price"`
	Status          int16     `json:"status"`
	SourceStatus    int16     `json:"source_status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type groupPricingListView struct {
	Items        []groupPricingView `json:"items"`
	Total        int64              `json:"total"`
	Page         int                `json:"page"`
	PageSize     int                `json:"page_size"`
	QuotaPerUnit int64              `json:"quota_per_unit"`
}

type pricingPreviewClampView struct {
	Op       string `json:"op"`
	Kind     string `json:"kind"`
	Original any    `json:"original"`
	Clamped  int64  `json:"clamped"`
}

type pricingPreviewView struct {
	Quota        int64                    `json:"quota"`
	QuotaPerUnit int64                    `json:"quota_per_unit"`
	AmountYuan   string                   `json:"amount_yuan"`
	Mode         string                   `json:"mode"`
	FreeModel    bool                     `json:"free_model"`
	Clamped      bool                     `json:"clamped"`
	Clamp        *pricingPreviewClampView `json:"clamp"`
}

func newModelPricingView(rec store.ModelPricingRecord) modelPricingView {
	mode := pricingModePerToken
	switch {
	case rec.BillingExpr != nil:
		mode = pricingModeTieredExpr
	case rec.Mode == 1:
		mode = pricingModePerRequest
	}
	return modelPricingView{
		ID: rec.ID, Model: rec.Model, Mode: mode, BillingExpr: rec.BillingExpr,
		PromptPrice: rec.PromptPrice, CompletionPrice: rec.CompletionPrice,
		CacheReadPrice: rec.CacheReadPrice, CacheWritePrice: rec.CacheWritePrice,
		Author: rec.Author, Vendor: rec.Vendor, Status: rec.Status,
		Free: modelPricingFree(rec), CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}
}

// modelPricingFree 复刻 modules-06 F-06-13 的「免费」判定：仅当四项有效价
// 全为 0 时才为 true；单项 0 仍渲染数字 0（不能挂免费 Tag）。
//
// 【为什么用有效价而非原始列】NULL 列按 billing-02 §3.2 兜底（输入价默认
// 7500、补全按模型族、缓存读/写按输入价派生）。输入价 NULL 时兜底价 >0，
// 因此只有「输入价显式 0」才可能是全免费；其余 NULL 列的兜底值也随之为 0。
func modelPricingFree(rec store.ModelPricingRecord) bool {
	if rec.BillingExpr != nil {
		// 表达式自包含计价；M3 的表达式子集没有可靠的静态免费判定，保守
		// 返回 false（避免把「按用量计费」误标成免费）。
		return false
	}
	if rec.PromptPrice == nil || *rec.PromptPrice != 0 {
		return false
	}
	if rec.Mode == 1 {
		return true
	}
	for _, price := range []*int64{rec.CompletionPrice, rec.CacheReadPrice, rec.CacheWritePrice} {
		if price != nil && *price != 0 {
			return false
		}
	}
	return true
}

func newGroupPricingView(rec store.GroupPricingRecord) groupPricingView {
	return groupPricingView{
		ID: rec.ID, Group: rec.Group, Model: rec.Model, Mode: rec.SourceMode,
		PromptPrice: rec.PromptPrice, CompletionPrice: rec.CompletionPrice,
		CacheReadPrice: rec.CacheReadPrice, CacheWritePrice: rec.CacheWritePrice,
		Status: rec.Status, SourceStatus: rec.SourceStatus,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}
}

// quotaToYuan 按 quota_per_unit 把整数积分换算为元，HALF_UP 到分。
//
// 用 decimal 而非 float64：0.1 分这类二进制误差会让「12.345 → 12.35」在
// 边界上抖动。返回固定两位字符串，避免 JSON 数字再被前端二次舍入。
func quotaToYuan(quota int64) string {
	qpu := pricing.CurrentQuotaPerUnit()
	if qpu <= 0 {
		qpu = pricing.DefaultQuotaPerUnit
	}
	return decimal.NewFromInt(quota).
		Div(decimal.NewFromInt(qpu)).
		Round(2). // shopspring 的 Round 即 HALF_UP（远离零）
		StringFixed(2)
}

// ---------------------------------------------------------------------------
// 请求解析
// ---------------------------------------------------------------------------

// modelPricingPayload 是创建/更新的原始字段集合。Set 标记承载「未提交 vs
// 显式 null」：price 列的三值语义（NULL/0/正数）不允许把两者折叠。
type modelPricingPayload struct {
	Model string

	ModeSet bool
	Mode    string

	ExprSet bool
	Expr    *string

	PromptPrice        *int64
	PromptPriceSet     bool
	CompletionPrice    *int64
	CompletionPriceSet bool
	CacheReadPrice     *int64
	CacheReadPriceSet  bool
	CacheWritePrice    *int64
	CacheWritePriceSet bool

	Author    *string
	AuthorSet bool
	Vendor    *string
	VendorSet bool

	Status *int16
}

func decodeNullableInt64(raw json.RawMessage) (*int64, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("必须是非负整数积分")
	}
	if value < 0 {
		return nil, errors.New("必须是非负整数积分")
	}
	if value > maxPricingPrice {
		return nil, fmt.Errorf("超出可精确表示的上界 %d", maxPricingPrice)
	}
	return &value, nil
}

func decodeNullablePricingText(raw json.RawMessage, maxRunes int) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("必须是字符串或 null")
	}
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > maxRunes {
		return nil, fmt.Errorf("最多 %d 个字符", maxRunes)
	}
	if value == "" {
		return nil, nil
	}
	return &value, nil
}

// parseModelPricingPayload 从 body 提取字段；model 是否必填由调用方决定。
func parseModelPricingPayload(body map[string]json.RawMessage) (modelPricingPayload, error) {
	var payload modelPricingPayload
	if raw, ok := body["model"]; ok {
		var model string
		if err := json.Unmarshal(raw, &model); err != nil {
			return payload, errors.New("model 必须是字符串")
		}
		payload.Model = strings.TrimSpace(model)
	}
	if raw, ok := body["mode"]; ok {
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			return payload, errors.New("mode 必须是字符串")
		}
		payload.ModeSet = true
		payload.Mode = strings.TrimSpace(mode)
	}
	if raw, ok := body["billing_expr"]; ok {
		payload.ExprSet = true
		// 表达式不能套用「空白→NULL」的文本归一化：R-37 规定非 NULL 的空白
		// 表达式是配置错误。保留空串进入 validatePricingExpression 才能 400；
		// 真正的「清除」只能由显式 null 表达。
		if string(raw) == "null" {
			payload.Expr = nil
		} else {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return payload, errors.New("billing_expr 必须是字符串或 null")
			}
			value = strings.TrimSpace(value)
			payload.Expr = &value
		}
	}
	priceFields := []struct {
		name  string
		value **int64
		set   *bool
	}{
		{"prompt_price", &payload.PromptPrice, &payload.PromptPriceSet},
		{"completion_price", &payload.CompletionPrice, &payload.CompletionPriceSet},
		{"cache_read_price", &payload.CacheReadPrice, &payload.CacheReadPriceSet},
		{"cache_write_price", &payload.CacheWritePrice, &payload.CacheWritePriceSet},
	}
	for _, field := range priceFields {
		raw, ok := body[field.name]
		if !ok {
			continue
		}
		value, err := decodeNullableInt64(raw)
		if err != nil {
			// 价格取值错误按 spec/OpenAPI 是 422 字段错误（不是 400）：
			// pricingFieldError 让调用方输出 problem.fields=[<field>]。
			return payload, &pricingFieldError{
				message: fmt.Sprintf("%s %v", field.name, err),
				fields:  []string{field.name},
			}
		}
		*field.value = value
		*field.set = true
	}
	if raw, ok := body["author"]; ok {
		payload.AuthorSet = true
		value, err := decodeNullablePricingText(raw, pricingTextMaxRunes)
		if err != nil {
			return payload, fmt.Errorf("author %w", err)
		}
		payload.Author = value
	}
	if raw, ok := body["vendor"]; ok {
		payload.VendorSet = true
		value, err := decodeNullablePricingText(raw, pricingTextMaxRunes)
		if err != nil {
			return payload, fmt.Errorf("vendor %w", err)
		}
		payload.Vendor = value
	}
	if raw, ok := body["status"]; ok {
		var status int16
		if err := json.Unmarshal(raw, &status); err != nil || (status != 1 && status != 2) {
			return payload, errors.New("status 必须是 1（启用）或 2（停用）")
		}
		payload.Status = &status
	}
	return payload, nil
}

func parsePricingMode(raw string) (int16, string, error) {
	switch raw {
	case pricingModePerToken:
		return 0, pricingModePerToken, nil
	case pricingModePerRequest:
		return 1, pricingModePerRequest, nil
	case pricingModeTieredExpr:
		return 0, pricingModeTieredExpr, nil
	default:
		return 0, "", errors.New("mode 必须是 per_token、per_request 或 tiered_expr")
	}
}

// validatePricingExpression 复用 pricing.ParsePricing 做保存期校验。
//
// 传入的是标准 spec JSON（mode=tiered_expr + expr），这样编译错误、未知
// 函数/变量白名单（billing-01 §2.1）与未经校验的裸字符串走同一实现。
func validatePricingExpression(expr string) error {
	if strings.TrimSpace(expr) == "" {
		// R-37：非 NULL 但空白的表达式属配置错误，不得静默回退倍率。
		return errors.New("billing_expr 不能为空；清除表达式请显式提交 null")
	}
	spec, err := json.Marshal(map[string]string{"mode": pricingModeTieredExpr, "expr": expr})
	if err != nil {
		return err
	}
	if _, err := pricing.ParsePricing(string(spec)); err != nil {
		return err
	}
	return nil
}

func parseUpdatedAt(raw json.RawMessage) (time.Time, error) {
	var token string
	if err := json.Unmarshal(raw, &token); err != nil || strings.TrimSpace(token) == "" {
		return time.Time{}, errors.New("updated_at 必须是 RFC 3339 时间戳")
	}
	parsed, err := time.Parse(time.RFC3339Nano, token)
	if err != nil {
		return time.Time{}, errors.New("updated_at 必须是 RFC 3339 时间戳")
	}
	return parsed, nil
}

// validatePricingModelParam 校验路径中的模型名。
//
// 路径参数不受请求体字段策略保护：若不校验，`%00`/空格等值会直接进入 pgx
// 参数（返回 500 或驱动错误），违反「非法输入 → 400/404」的契约。
func validatePricingModelParam(w http.ResponseWriter, model string) bool {
	if pricingModelPattern.MatchString(model) {
		return true
	}
	writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest,
		"Invalid request", "model path parameter must be 1-128 characters of [A-Za-z0-9._:-]", []string{"model"})
	return false
}

// validatePricingGroupModelParams 校验覆盖价路径中的分组名与模型名。
func validatePricingGroupModelParams(w http.ResponseWriter, group, model string) bool {
	if !groupNamePattern.MatchString(group) {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "group path parameter must match ^[a-z0-9_-]{2,32}$", []string{"group"})
		return false
	}
	return validatePricingModelParam(w, model)
}

// updatedAtFromRequest 读取 DELETE 的乐观锁版本：优先查询参数（DELETE 常无
// body），其次可选 JSON body。两者都缺失时 400。
func updatedAtFromRequest(r *http.Request) (time.Time, error) {
	if raw := strings.TrimSpace(r.URL.Query().Get("updated_at")); raw != "" {
		return time.Parse(time.RFC3339Nano, raw)
	}
	if r.ContentLength > 0 {
		body, err := readRawObject(r)
		if err != nil {
			return time.Time{}, errors.New("updated_at 必须是 RFC 3339 时间戳")
		}
		raw, ok := body["updated_at"]
		if !ok {
			return time.Time{}, errors.New("updated_at is required for optimistic locking")
		}
		return parseUpdatedAt(raw)
	}
	return time.Time{}, errors.New("updated_at is required for optimistic locking")
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// writePricingProblemWithFields 输出带 fields 扩展成员的 RFC 7807。
// problem.yaml 允许 additionalProperties，因此 fields 是契约内扩展。
func writePricingProblemWithFields(w http.ResponseWriter, status int, problemType, title, detail string, fields []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := map[string]any{"type": problemType, "title": title, "status": status}
	if detail != "" {
		problem["detail"] = detail
	}
	if len(fields) > 0 {
		problem["fields"] = fields
	}
	_ = json.NewEncoder(w).Encode(problem)
}

// writePricingParseError 把请求体解析错误映射为 400/422。
//
// 价格列（负值/超界/非整数/字符串）按 spec 是字段校验错误 → 422 +
// problem.fields；其余结构/枚举错误仍是 400。表达式校验在 create 上保持
// 400（计划验收 (a)），PATCH 的模式歧义走 422。
func writePricingParseError(w http.ResponseWriter, err error) {
	var fieldErr *pricingFieldError
	if errors.As(err, &fieldErr) {
		writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", fieldErr.message, fieldErr.fields)
		return
	}
	writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest,
		"Invalid request", err.Error(), nil)
}

func (h *PricingHandlers) writePricingRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrPricingNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, store.ErrPricingConflict):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the pricing row was modified by someone else; refresh and retry")
	case errors.Is(err, store.ErrPricingDuplicate):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the pricing row already exists")
	case errors.Is(err, store.ErrPricingInvalidGroup):
		writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", "group does not exist or is disabled", []string{"group"})
	case errors.Is(err, store.ErrPricingInvalidModel):
		writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", "source model is not priced", []string{"model"})
	case errors.Is(err, store.ErrPricingSourceIsExpression):
		writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed",
			"source model uses tiered_expr pricing; group overrides are not applied in expression mode",
			[]string{"model"})
	case errors.Is(err, store.ErrPricingPerRequestFields):
		writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed",
			"per_request source ignores completion/cache override prices",
			[]string{"completion_price", "cache_read_price", "cache_write_price"})
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}

// ---------------------------------------------------------------------------
// 模型定价：列表 / 模型选择器
// ---------------------------------------------------------------------------

func (h *PricingHandlers) handleListModelPricing(w http.ResponseWriter, r *http.Request) {
	filter, err := parseModelPricingFilter(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	items, total, err := h.repo.ListModelPricing(r.Context(), filter)
	if err != nil {
		h.writePricingRepoError(w, "list model pricing", err)
		return
	}
	views := make([]modelPricingView, 0, len(items))
	for _, item := range items {
		views = append(views, newModelPricingView(item))
	}
	writeJSON(w, http.StatusOK, modelPricingListView{
		Items: views, Total: total, Page: filter.Page, PageSize: filter.PageSize,
		QuotaPerUnit: pricing.CurrentQuotaPerUnit(),
	})
}

func parseModelPricingFilter(r *http.Request) (store.ModelPricingFilter, error) {
	q := r.URL.Query()
	filter := store.ModelPricingFilter{Page: 1, PageSize: adminPageSizeDefault, Sort: "model"}
	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 || page > adminPageMax {
			return filter, fmt.Errorf("p must be between 1 and %d", adminPageMax)
		}
		filter.Page = page
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > adminPageSizeMax {
			return filter, fmt.Errorf("page_size must be between 1 and %d", adminPageSizeMax)
		}
		filter.PageSize = size
	}
	filter.Keyword = strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(filter.Keyword) > pricingModelMaxRunes {
		return filter, errors.New("keyword is too long")
	}
	filter.Mode = strings.TrimSpace(q.Get("mode"))
	switch filter.Mode {
	case "", pricingModePerToken, pricingModePerRequest, pricingModeTieredExpr:
	default:
		return filter, errors.New("mode must be per_token, per_request or tiered_expr")
	}
	status, err := parsePricingStatusFilter(q.Get("status"))
	if err != nil {
		return filter, err
	}
	filter.Status = status
	switch strings.TrimSpace(q.Get("sort")) {
	case "", "model":
		filter.Sort = "model"
	case "-model", "prompt_price", "-prompt_price", "updated_at", "-updated_at":
		filter.Sort = strings.TrimSpace(q.Get("sort"))
	default:
		return filter, errors.New("sort must be one of model, -model, prompt_price, -prompt_price, updated_at, -updated_at")
	}
	return filter, nil
}

// parsePricingStatusFilter 接受 1/2 与 enabled/disabled；空串/空表示不筛选。
func parsePricingStatusFilter(raw string) (*int16, error) {
	var status int16
	switch strings.TrimSpace(raw) {
	case "":
		return nil, nil
	case "1", "enabled":
		status = 1
	case "2", "disabled":
		status = 2
	default:
		return nil, errors.New("status must be 1/2 or enabled/disabled")
	}
	return &status, nil
}

func (h *PricingHandlers) handleListPricingModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.PricingModelFilter{Limit: pricingModelLimitDef}
	filter.Keyword = strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(filter.Keyword) > pricingModelMaxRunes {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "keyword is too long")
		return
	}
	switch strings.TrimSpace(q.Get("status")) {
	case "", "1", "enabled":
		status := int16(1)
		filter.Status = &status
	case "all":
		filter.Status = nil
	case "2", "disabled":
		status := int16(2)
		filter.Status = &status
	default:
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"status must be 1/2, enabled/disabled or all")
		return
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > pricingModelLimitMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("limit must be between 1 and %d", pricingModelLimitMax))
			return
		}
		filter.Limit = limit
	}
	items, err := h.repo.ListPricingModels(r.Context(), filter)
	if err != nil {
		h.writePricingRepoError(w, "list pricing models", err)
		return
	}
	views := make([]pricingModelOptionView, 0, len(items))
	for _, item := range items {
		views = append(views, pricingModelOptionView{Model: item.Model, Mode: item.Mode, Status: item.Status})
	}
	writeJSON(w, http.StatusOK, pricingModelListView{Items: views})
}

// ---------------------------------------------------------------------------
// 模型定价：创建
// ---------------------------------------------------------------------------

func (h *PricingHandlers) handleCreateModelPricing(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	payload, err := parseModelPricingPayload(body)
	if err != nil {
		writePricingParseError(w, err)
		return
	}
	model := payload.Model
	if !pricingModelPattern.MatchString(model) {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"model must be 1-128 characters of [A-Za-z0-9._:-]", []string{"model"})
		return
	}
	// author 是 NOT NULL 列：创建时显式 null 与更新同样 422，而不是静默用默认值。
	if payload.AuthorSet && payload.Author == nil {
		writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", `author must not be null or empty; omit it for "Unknown" or submit "Unknown"`,
			[]string{"author"})
		return
	}
	if payload.ExprSet && payload.Expr != nil {
		if payload.ModeSet && payload.Mode != pricingModeTieredExpr {
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"billing_expr requires mode=tiered_expr", []string{"mode"})
			return
		}
		if payload.PromptPrice != nil || payload.CompletionPrice != nil ||
			payload.CacheReadPrice != nil || payload.CacheWritePrice != nil {
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"tiered_expr mode must not mix numeric price fields", nil)
			return
		}
		if err := validatePricingExpression(*payload.Expr); err != nil {
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				err.Error(), []string{"billing_expr"})
			return
		}
	}
	if payload.ExprSet && payload.Expr == nil && payload.ModeSet && payload.Mode == pricingModeTieredExpr {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"mode=tiered_expr requires a non-empty billing_expr", []string{"billing_expr"})
		return
	}
	if !payload.ModeSet {
		// 只给 billing_expr 而不给 mode 是合法简写：表达式本身就是模式判别。
		if payload.Expr != nil {
			payload.Mode = pricingModeTieredExpr
		} else {
			payload.Mode = pricingModePerToken
		}
	}
	mode, modeName, err := parsePricingMode(payload.Mode)
	if err != nil {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			err.Error(), []string{"mode"})
		return
	}
	if modeName == pricingModeTieredExpr {
		if payload.Expr == nil {
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"mode=tiered_expr requires a non-empty billing_expr", []string{"billing_expr"})
			return
		}
	} else if payload.Expr != nil {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"billing_expr requires mode=tiered_expr", []string{"mode"})
		return
	}

	input := store.ModelPricingInput{
		Model: model, Mode: mode, Author: "Unknown", Status: 1,
	}
	if modeName == pricingModeTieredExpr {
		input.BillingExpr = payload.Expr
		input.Mode = 0 // 表达式模式以 billing_expr 判别；mode 列保留 0 以免误导。
	} else {
		input.PromptPrice = payload.PromptPrice
		input.CompletionPrice = payload.CompletionPrice
		input.CacheReadPrice = payload.CacheReadPrice
		input.CacheWritePrice = payload.CacheWritePrice
		if modeName == pricingModePerRequest {
			if payload.CompletionPrice != nil || payload.CacheReadPrice != nil || payload.CacheWritePrice != nil {
				writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
					"Validation failed", "per_request mode ignores completion/cache prices",
					[]string{"completion_price", "cache_read_price", "cache_write_price"})
				return
			}
		}
	}
	if payload.Author != nil {
		input.Author = *payload.Author
	}
	input.Vendor = payload.Vendor
	if payload.Status != nil {
		input.Status = *payload.Status
	}

	rec, err := h.repo.CreateModelPricing(r.Context(), input)
	if err != nil {
		h.writePricingRepoError(w, "create model pricing", err)
		return
	}
	h.notifyChange()
	writeJSON(w, http.StatusCreated, newModelPricingView(rec))
}

// ---------------------------------------------------------------------------
// 模型定价：更新 / 删除 / 启停
// ---------------------------------------------------------------------------

func (h *PricingHandlers) handleUpdateModelPricing(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.PathValue("model"))
	if model == "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if !validatePricingModelParam(w, model) {
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	rawUpdatedAt, ok := body["updated_at"]
	if !ok {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking")
		return
	}
	expectUpdatedAt, err := parseUpdatedAt(rawUpdatedAt)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	payload, err := parseModelPricingPayload(body)
	if err != nil {
		writePricingParseError(w, err)
		return
	}
	if payload.Model != "" && payload.Model != model {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"model is immutable; it cannot be renamed", []string{"model"})
		return
	}

	current, err := h.repo.GetModelPricing(r.Context(), model)
	if err != nil {
		h.writePricingRepoError(w, "read model pricing before update", err)
		return
	}
	patch, err := buildModelPricingPatch(current, payload)
	if err != nil {
		var fieldErr *pricingFieldError
		if errors.As(err, &fieldErr) {
			writePricingProblemWithFields(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
				"Validation failed", fieldErr.message, fieldErr.fields)
			return
		}
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", err.Error(), nil)
		return
	}
	rec, err := h.repo.UpdateModelPricing(r.Context(), model, patch, expectUpdatedAt)
	if err != nil {
		h.writePricingRepoError(w, "update model pricing", err)
		return
	}
	h.notifyChange()
	writeJSON(w, http.StatusOK, newModelPricingView(rec))
}

// pricingFieldError 让字段级校验错误携带 fields（problem.fields）。
type pricingFieldError struct {
	message string
	fields  []string
}

func (e *pricingFieldError) Error() string { return e.message }

func buildModelPricingPatch(current store.ModelPricingRecord, payload modelPricingPayload) (store.ModelPricingPatch, error) {
	patch := store.ModelPricingPatch{}
	hasChange := false

	// 表达式状态：nil = 非表达式；非 nil = 表达式。
	exprAfter := current.BillingExpr
	if payload.ExprSet {
		hasChange = true
		if payload.Expr == nil {
			exprAfter = nil
		} else {
			if err := validatePricingExpression(*payload.Expr); err != nil {
				return patch, &pricingFieldError{message: err.Error(), fields: []string{"billing_expr"}}
			}
			exprAfter = payload.Expr
		}
	}
	modeAfterName := ""
	if payload.ModeSet {
		hasChange = true
		if payload.Mode != pricingModePerToken && payload.Mode != pricingModePerRequest && payload.Mode != pricingModeTieredExpr {
			return patch, &pricingFieldError{message: "mode 必须是 per_token、per_request 或 tiered_expr", fields: []string{"mode"}}
		}
		modeAfterName = payload.Mode
	}

	switch {
	case modeAfterName == pricingModeTieredExpr:
		if exprAfter == nil {
			return patch, &pricingFieldError{message: "mode=tiered_expr requires a non-empty billing_expr", fields: []string{"billing_expr"}}
		}
		if payload.PromptPrice != nil || payload.CompletionPrice != nil ||
			payload.CacheReadPrice != nil || payload.CacheWritePrice != nil {
			return patch, &pricingFieldError{message: "tiered_expr mode must not mix numeric price fields"}
		}
		// 表达式是唯一价格配置：切到表达式时清空四个价列，避免旧价在切回
		// 时「复活」成管理员从未确认过的价格。
		mode := int16(0)
		patch.Mode = &mode
		patch.BillingExpr = exprAfter
		patch.BillingExprSet = true
		patch.PromptPrice = nil
		patch.PromptPriceSet = true
		patch.CompletionPrice = nil
		patch.CompletionPriceSet = true
		patch.CacheReadPrice = nil
		patch.CacheReadPriceSet = true
		patch.CacheWritePrice = nil
		patch.CacheWritePriceSet = true
		hasChange = true
	case exprAfter != nil:
		if payload.ModeSet {
			return patch, &pricingFieldError{message: "billing_expr requires mode=tiered_expr", fields: []string{"mode"}}
		}
		// 表达式自包含全部计价：在表达式行上写数值价列会被 Source 忽略，
		// 静默接受会让管理员误以为价列生效（fail-closed）。
		if payload.PromptPriceSet || payload.CompletionPriceSet ||
			payload.CacheReadPriceSet || payload.CacheWritePriceSet {
			return patch, &pricingFieldError{message: "tiered_expr mode must not mix numeric price fields"}
		}
		if payload.ExprSet {
			patch.BillingExpr = exprAfter
			patch.BillingExprSet = true
			// 表达式是唯一价格配置；切到表达式时清空四个价列，mode 列归零保持
			// 判别列与 billing_expr 一致。旧价留在行里会在切回时「复活」。
			mode := int16(0)
			patch.Mode = &mode
			patch.PromptPrice = nil
			patch.PromptPriceSet = true
			patch.CompletionPrice = nil
			patch.CompletionPriceSet = true
			patch.CacheReadPrice = nil
			patch.CacheReadPriceSet = true
			patch.CacheWritePrice = nil
			patch.CacheWritePriceSet = true
		}
	default:
		// 当前是表达式、请求未显式清空却改成 ratio/per_request：拒绝歧义。
		if current.BillingExpr != nil && payload.ModeSet && !payload.ExprSet {
			return patch, &pricingFieldError{
				message: "leaving tiered_expr mode requires billing_expr: null",
				fields:  []string{"billing_expr"},
			}
		}
		if payload.ExprSet {
			patch.BillingExpr = nil
			patch.BillingExprSet = true
		}
		if payload.ModeSet {
			mode, _, _ := parsePricingMode(modeAfterName)
			patch.Mode = &mode
		}
	}

	// 有效模式 = 本次提交的 mode（若有）否则当前行模式。per_request 只读
	// prompt_price；即使请求没带 mode，在当前 per_request 行上写隐藏价同样是
	// 静默 no-op，必须 422（与 create 的显式 0 也拒绝的口径一致）。
	effectiveMode := current.Mode
	if payload.ModeSet && modeAfterName != pricingModeTieredExpr {
		effectiveMode, _, _ = parsePricingMode(modeAfterName)
	}
	if exprAfter == nil && effectiveMode == 1 &&
		(payload.CompletionPrice != nil || payload.CacheReadPrice != nil || payload.CacheWritePrice != nil) {
		return patch, &pricingFieldError{
			message: "per_request mode ignores completion/cache prices",
			fields:  []string{"completion_price", "cache_read_price", "cache_write_price"},
		}
	}

	if payload.PromptPriceSet {
		patch.PromptPrice = payload.PromptPrice
		patch.PromptPriceSet = true
		hasChange = true
	}
	if payload.CompletionPriceSet {
		patch.CompletionPrice = payload.CompletionPrice
		patch.CompletionPriceSet = true
		hasChange = true
	}
	if payload.CacheReadPriceSet {
		patch.CacheReadPrice = payload.CacheReadPrice
		patch.CacheReadPriceSet = true
		hasChange = true
	}
	if payload.CacheWritePriceSet {
		patch.CacheWritePrice = payload.CacheWritePrice
		patch.CacheWritePriceSet = true
		hasChange = true
	}
	if payload.AuthorSet {
		// model_pricing.author 是 NOT NULL DEFAULT 'Unknown'：显式 null 没有
		// 可落的语义。按 fail-closed 拒绝，管理员可用 "Unknown" 复位。
		if payload.Author == nil {
			return patch, &pricingFieldError{
				message: `author must not be null or empty; submit "Unknown" to reset it`,
				fields:  []string{"author"},
			}
		}
		patch.Author = payload.Author
		hasChange = true
	}
	if payload.VendorSet {
		patch.Vendor = payload.Vendor
		patch.VendorSet = true
		hasChange = true
	}
	if payload.Status != nil {
		patch.Status = payload.Status
		hasChange = true
	}
	if !hasChange {
		return patch, errors.New("no fields to update")
	}
	return patch, nil
}

func (h *PricingHandlers) handleDeleteModelPricing(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.PathValue("model"))
	if model == "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if !validatePricingModelParam(w, model) {
		return
	}
	expectUpdatedAt, err := updatedAtFromRequest(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	if err := h.repo.DeleteModelPricing(r.Context(), model, expectUpdatedAt); err != nil {
		h.writePricingRepoError(w, "delete model pricing", err)
		return
	}
	h.notifyChange()
	w.WriteHeader(http.StatusNoContent)
}

func (h *PricingHandlers) handleModelPricingAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSpace(r.PathValue("rest"))
	model, action, ok := splitPricingAction(rest)
	if !ok || model == "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if !validatePricingModelParam(w, model) {
		return
	}
	var status int16
	switch action {
	case "enable":
		status = 1
	case "disable":
		status = 2
	default:
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	raw, ok := body["updated_at"]
	if !ok {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking")
		return
	}
	expectUpdatedAt, err := parseUpdatedAt(raw)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	rec, err := h.repo.SetModelPricingStatus(r.Context(), model, status, expectUpdatedAt)
	if err != nil {
		h.writePricingRepoError(w, "set model pricing status", err)
		return
	}
	h.notifyChange()
	writeJSON(w, http.StatusOK, newModelPricingView(rec))
}

// splitPricingAction 从 "<target>:<action>" 解析目标与动作。
// 只在最后一个冒号处切分，允许模型名自身含冒号（modules-06 F-06-14）。
func splitPricingAction(rest string) (target, action string, ok bool) {
	index := strings.LastIndex(rest, ":")
	if index <= 0 || index == len(rest)-1 {
		return rest, "", false
	}
	return strings.TrimSpace(rest[:index]), rest[index+1:], true
}

// ---------------------------------------------------------------------------
// 预览
// ---------------------------------------------------------------------------

func (h *PricingHandlers) handlePreviewPricing(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	rawSpec, ok := body["spec"]
	if !ok {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"spec is required", []string{"spec"})
		return
	}
	spec, err := normalizePricingSpec(rawSpec)
	if err != nil {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			err.Error(), []string{"spec"})
		return
	}
	model := ""
	if raw, ok := body["model"]; ok {
		if err := json.Unmarshal(raw, &model); err != nil {
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"model must be a string", []string{"model"})
			return
		}
		model = strings.TrimSpace(model)
	}
	groupRatio := 1.0
	if raw, ok := body["group_ratio"]; ok {
		if err := json.Unmarshal(raw, &groupRatio); err != nil {
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"group_ratio must be a number", []string{"group_ratio"})
			return
		}
	}
	if groupRatio <= 0 || groupRatio != groupRatio || groupRatio > 1e9 {
		// NaN != NaN；显式拒绝 NaN/Inf/≤0，避免预览给出一条永远不可能结算的价。
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"group_ratio must be a finite positive number", []string{"group_ratio"})
		return
	}
	params, err := parsePricingSampleParams(body["params"])
	if err != nil {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			err.Error(), []string{"params"})
		return
	}

	parsed, err := pricing.ParsePricingSpec(spec)
	if err != nil {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			err.Error(), []string{"spec"})
		return
	}
	price := parsed.Price
	price.GroupRatioInfo = contract.GroupRatioInfo{Group: "preview", GroupRatio: groupRatio}
	snapshot, err := pricing.ResolveExpression(&price, model, parsed.Mode, parsed.Expr)
	if err != nil {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			err.Error(), []string{"spec"})
		return
	}
	quota, computeErr := pricing.ComputeQuota(snapshot, params, nil)
	var clamp *contract.QuotaClamp
	if computeErr != nil {
		var ok bool
		if clamp, ok = pricing.AsClamp(computeErr); !ok {
			fields := []string{"spec"}
			if errors.Is(computeErr, pricing.ErrInvalidParams) {
				fields = []string{"params"}
			}
			writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				computeErr.Error(), fields)
			return
		}
	}
	modeName := pricingModeName(snapshot.Mode)
	view := pricingPreviewView{
		Quota: quota, QuotaPerUnit: pricing.CurrentQuotaPerUnit(),
		AmountYuan: quotaToYuan(quota), Mode: modeName,
		FreeModel: snapshot.Price.FreeModel, Clamped: clamp != nil,
	}
	if clamp != nil {
		audit := pricing.ClampAuditMap(clamp)
		view.Clamp = &pricingPreviewClampView{
			Op: clamp.Op, Kind: audit["kind"].(string),
			Original: audit["original"], Clamped: clamp.Clamped,
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// normalizePricingSpec 接受 JSON 对象或字符串两种线格式。
// 对象是正常形态；字符串便于前端复用已有的「表达式即 JSON」编辑器状态。
func normalizePricingSpec(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", errors.New("spec is required")
	}
	if strings.HasPrefix(trimmed, `"`) {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", errors.New("spec must be a JSON object or string")
		}
		return value, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", errors.New("spec must be a JSON object or string")
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", errors.New("spec must be a JSON object or string")
	}
	return string(encoded), nil
}

func parsePricingSampleParams(raw json.RawMessage) (contract.TokenParams, error) {
	params := contract.TokenParams{ImageCount: pricing.ImageCountUnset}
	if len(raw) == 0 || string(raw) == "null" {
		// 未提供样例用量时按「无 token、未设置张数」预览；表达式常量价仍可求值。
		return params, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return params, errors.New("params must be a JSON object")
	}
	decoders := map[string]*int64{
		"p": &params.P, "c": &params.C, "len": &params.Len, "cr": &params.CR,
		"cc5m": &params.CC5m, "cc1h": &params.CC1h, "img": &params.Img,
		"img_cr": &params.ImgCR, "img_o": &params.ImgO, "ai": &params.AI, "ao": &params.AO,
	}
	for name, dst := range decoders {
		value, ok := body[name]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, dst); err != nil || *dst < 0 {
			return params, fmt.Errorf("params.%s must be a non-negative integer", name)
		}
	}
	if rawCount, ok := body["image_count"]; ok {
		var count int
		if err := json.Unmarshal(rawCount, &count); err != nil || count < pricing.ImageCountUnset {
			return params, errors.New("params.image_count must be -1 (unset) or a non-negative integer")
		}
		params.ImageCount = count
	}
	return params, nil
}

func pricingModeName(mode contract.BillingMode) string {
	switch mode {
	case contract.BillingModeTieredExpr:
		return pricingModeTieredExpr
	case contract.BillingModePerCall:
		return pricingModePerRequest
	default:
		return pricingModePerToken
	}
}

// ---------------------------------------------------------------------------
// 分组覆盖价
// ---------------------------------------------------------------------------

func (h *PricingHandlers) handleListGroupPricing(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.GroupPricingFilter{Page: 1, PageSize: adminPageSizeDefault, Sort: "group"}
	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 || page > adminPageMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("p must be between 1 and %d", adminPageMax))
			return
		}
		filter.Page = page
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > adminPageSizeMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("page_size must be between 1 and %d", adminPageSizeMax))
			return
		}
		filter.PageSize = size
	}
	filter.Group = strings.TrimSpace(q.Get("group"))
	if filter.Group != "" && !groupNamePattern.MatchString(filter.Group) {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"group must match ^[a-z0-9_-]{2,32}$")
		return
	}
	filter.Keyword = strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(filter.Keyword) > pricingModelMaxRunes {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "keyword is too long")
		return
	}
	status, err := parsePricingStatusFilter(q.Get("status"))
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	filter.Status = status
	switch q.Get("sort") {
	case "", "group", "-group", "model", "-model", "updated_at", "-updated_at":
		filter.Sort = q.Get("sort")
	default:
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"sort must be one of group, -group, model, -model, updated_at, -updated_at")
		return
	}
	items, total, err := h.repo.ListGroupPricing(r.Context(), filter)
	if err != nil {
		h.writePricingRepoError(w, "list group pricing", err)
		return
	}
	views := make([]groupPricingView, 0, len(items))
	for _, item := range items {
		views = append(views, newGroupPricingView(item))
	}
	writeJSON(w, http.StatusOK, groupPricingListView{
		Items: views, Total: total, Page: filter.Page, PageSize: filter.PageSize,
		QuotaPerUnit: pricing.CurrentQuotaPerUnit(),
	})
}

// parseGroupPricingPayload 解析覆盖价字段；create 时价列缺省为 0（NOT NULL）。
func parseGroupPricingPayload(body map[string]json.RawMessage, create bool) (store.GroupPricingInput, store.GroupPricingPatch, error) {
	var (
		input store.GroupPricingInput
		patch store.GroupPricingPatch
	)
	if raw, ok := body["group"]; ok {
		if err := json.Unmarshal(raw, &input.Group); err != nil {
			return input, patch, errors.New("group must be a string")
		}
		input.Group = strings.TrimSpace(input.Group)
	}
	if raw, ok := body["model"]; ok {
		if err := json.Unmarshal(raw, &input.Model); err != nil {
			return input, patch, errors.New("model must be a string")
		}
		input.Model = strings.TrimSpace(input.Model)
	}
	priceFields := []struct {
		name string
		dst  *int64
	}{
		{"prompt_price", &input.PromptPrice},
		{"completion_price", &input.CompletionPrice},
		{"cache_read_price", &input.CacheReadPrice},
		{"cache_write_price", &input.CacheWritePrice},
	}
	patchDst := map[string]**int64{
		"prompt_price":      &patch.PromptPrice,
		"completion_price":  &patch.CompletionPrice,
		"cache_read_price":  &patch.CacheReadPrice,
		"cache_write_price": &patch.CacheWritePrice,
	}
	for _, field := range priceFields {
		raw, ok := body[field.name]
		if !ok {
			continue
		}
		// group_pricing 的价列是 NOT NULL：JSON null 不能被静默当 0（那会变成
		// 「显式免费」）。缺省值只能通过省略键获得（服务端填 0）。
		if string(raw) == "null" {
			return input, patch, &pricingFieldError{
				message: fmt.Sprintf("%s must be an integer; null is not allowed", field.name),
				fields:  []string{field.name},
			}
		}
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
			return input, patch, &pricingFieldError{
				message: fmt.Sprintf("%s must be a non-negative integer", field.name),
				fields:  []string{field.name},
			}
		}
		if value > maxPricingPrice {
			return input, patch, &pricingFieldError{
				message: fmt.Sprintf("%s exceeds the precise upper bound %d", field.name, maxPricingPrice),
				fields:  []string{field.name},
			}
		}
		*field.dst = value
		valueCopy := value
		*patchDst[field.name] = &valueCopy
	}
	if raw, ok := body["status"]; ok {
		var status int16
		if err := json.Unmarshal(raw, &status); err != nil || (status != 1 && status != 2) {
			return input, patch, errors.New("status must be 1 (enabled) or 2 (disabled)")
		}
		input.Status = status
		patch.Status = &status
	}
	if create {
		input.Status = 1
		if raw, ok := body["status"]; ok {
			var status int16
			if err := json.Unmarshal(raw, &status); err != nil || (status != 1 && status != 2) {
				return input, patch, errors.New("status must be 1 (enabled) or 2 (disabled)")
			}
			input.Status = status
		}
	}
	return input, patch, nil
}

func (h *PricingHandlers) handleCreateGroupPricing(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	input, _, err := parseGroupPricingPayload(body, true)
	if err != nil {
		writePricingParseError(w, err)
		return
	}
	if !groupNamePattern.MatchString(input.Group) {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"group must match ^[a-z0-9_-]{2,32}$", []string{"group"})
		return
	}
	if !pricingModelPattern.MatchString(input.Model) {
		writePricingProblemWithFields(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"model must be 1-128 characters of [A-Za-z0-9._:-]", []string{"model"})
		return
	}
	rec, err := h.repo.CreateGroupPricing(r.Context(), input)
	if err != nil {
		h.writePricingRepoError(w, "create group pricing", err)
		return
	}
	h.notifyChange()
	writeJSON(w, http.StatusCreated, newGroupPricingView(rec))
}

// parseGroupPricingKey 解析 {rest...} 为 group/model（动作端点可带 :action）。
//
// allowAction=false 用于 PATCH/DELETE：模型名允许含冒号（modules-06 F-06-14），
// 此时整段 tail 都是模型名，不能把 `model:xxx` 误当动作。
func parseGroupPricingKey(rest string, allowAction bool) (group, model, action string, ok bool) {
	rest = strings.TrimSpace(rest)
	slash := strings.Index(rest, "/")
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", "", false
	}
	group = rest[:slash]
	tail := rest[slash+1:]
	if allowAction {
		if target, act, hasAction := splitPricingAction(tail); hasAction {
			return group, target, act, true
		}
	}
	return group, tail, "", true
}

func (h *PricingHandlers) handleUpdateGroupPricing(w http.ResponseWriter, r *http.Request) {
	group, model, action, ok := parseGroupPricingKey(r.PathValue("rest"), false)
	if !ok || action != "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if !validatePricingGroupModelParams(w, group, model) {
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	rawUpdatedAt, ok := body["updated_at"]
	if !ok {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking")
		return
	}
	expectUpdatedAt, err := parseUpdatedAt(rawUpdatedAt)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	_, patch, err := parseGroupPricingPayload(body, false)
	if err != nil {
		writePricingParseError(w, err)
		return
	}
	if patch.PromptPrice == nil && patch.CompletionPrice == nil && patch.CacheReadPrice == nil &&
		patch.CacheWritePrice == nil && patch.Status == nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "no fields to update")
		return
	}
	rec, err := h.repo.UpdateGroupPricing(r.Context(), group, model, patch, expectUpdatedAt)
	if err != nil {
		h.writePricingRepoError(w, "update group pricing", err)
		return
	}
	h.notifyChange()
	writeJSON(w, http.StatusOK, newGroupPricingView(rec))
}

func (h *PricingHandlers) handleDeleteGroupPricing(w http.ResponseWriter, r *http.Request) {
	group, model, action, ok := parseGroupPricingKey(r.PathValue("rest"), false)
	if !ok || action != "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if !validatePricingGroupModelParams(w, group, model) {
		return
	}
	expectUpdatedAt, err := updatedAtFromRequest(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	if err := h.repo.DeleteGroupPricing(r.Context(), group, model, expectUpdatedAt); err != nil {
		h.writePricingRepoError(w, "delete group pricing", err)
		return
	}
	h.notifyChange()
	w.WriteHeader(http.StatusNoContent)
}

func (h *PricingHandlers) handleGroupPricingAction(w http.ResponseWriter, r *http.Request) {
	group, model, action, ok := parseGroupPricingKey(r.PathValue("rest"), true)
	if !ok || action == "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if !validatePricingGroupModelParams(w, group, model) {
		return
	}
	var status int16
	switch action {
	case "enable":
		status = 1
	case "disable":
		status = 2
	default:
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	raw, ok := body["updated_at"]
	if !ok {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking")
		return
	}
	expectUpdatedAt, err := parseUpdatedAt(raw)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	rec, err := h.repo.SetGroupPricingStatus(r.Context(), group, model, status, expectUpdatedAt)
	if err != nil {
		h.writePricingRepoError(w, "set group pricing status", err)
		return
	}
	h.notifyChange()
	writeJSON(w, http.StatusOK, newGroupPricingView(rec))
}

// ---------------------------------------------------------------------------
// 创建幂等（Idempotency-Key）
// ---------------------------------------------------------------------------

// idempotentCreate 为两个创建端点提供 24h 幂等重放，与用户/令牌端点同构：
// 同 key + 同 body 返回首次响应；同 key + 不同 body → 422。
// scope 区分资源，键里带操作者 ID，避免跨管理员共享幂等空间。
func (h *PricingHandlers) idempotentCreate(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil || p.UserID == 0 {
			writeAdminProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > adminIdempotencyKeyHdrMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"Idempotency-Key is too long")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"unable to read request body")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		fingerprint := tokenBodyFingerprint(body)

		ctx := r.Context()
		cacheKey := fmt.Sprintf("newtg:idem:pricing:%s:create:%d:%s", scope, p.UserID, key)
		if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
			replayAdminIdempotent(w, cached, fingerprint)
			return
		}
		lockKey := cacheKey + ":lock"
		acquired, lockErr := h.rdb.SetNX(ctx, lockKey, "1", adminIdempotencyLease).Result()
		if lockErr == nil && !acquired {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
						"request cancelled before the idempotent result was ready")
					return
				case <-time.After(25 * time.Millisecond):
				}
				if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
					replayAdminIdempotent(w, cached, fingerprint)
					return
				}
			}
			writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
				"a request with the same Idempotency-Key is still in progress; retry shortly")
			return
		}

		buffer := newAdminBufferedResponse()
		next(buffer, r)
		if buffer.status >= 200 && buffer.status < 300 {
			h.saveIdempotent(ctx, cacheKey, adminCachedHTTPResponse{
				Status: buffer.status, ContentType: buffer.header.Get("Content-Type"),
				Body: buffer.body.String(), Fingerprint: fingerprint,
			})
		}
		if lockErr == nil {
			_ = h.rdb.Del(ctx, lockKey).Err()
		}
		buffer.copyTo(w)
	}
}

func (h *PricingHandlers) loadIdempotent(ctx context.Context, key string) (adminCachedHTTPResponse, bool) {
	raw, err := h.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return adminCachedHTTPResponse{}, false
	}
	var cached adminCachedHTTPResponse
	if err := json.Unmarshal(raw, &cached); err != nil {
		return adminCachedHTTPResponse{}, false
	}
	return cached, true
}

func (h *PricingHandlers) saveIdempotent(ctx context.Context, key string, cached adminCachedHTTPResponse) {
	raw, err := json.Marshal(cached)
	if err != nil {
		return
	}
	_ = h.rdb.Set(ctx, key, raw, adminIdempotencyTTL).Err()
}
