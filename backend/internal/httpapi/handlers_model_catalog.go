// 模型目录与供应商目录管理端点（modules-06 F-06-22/23/24/25/26；D1b）。
//
// 端点面（权限矩阵冻结在任务书 §1）：
//
//	GET    /api/models                        channels:read
//	POST   /api/models                        channels:manage + Idempotency-Key
//	GET    /api/models/{id}                   channels:read
//	PUT    /api/models/{id}                   channels:manage（updated_at 乐观锁）
//	DELETE /api/models/{id}?updated_at=       channels:manage（updated_at 乐观锁）
//	GET    /api/models/search                 channels:read（limit ≤100）
//	POST   /api/models/batch-delete           channels:manage_sensitive
//	                                          （clear_pricing=true 额外 pricing:manage）
//	GET    /api/models/missing                channels:read
//	GET    /api/models/sync/preview           channels:manage_sensitive
//	POST   /api/models/sync                   channels:manage_sensitive
//
//	GET    /api/vendors                       channels:read
//	POST   /api/vendors                       channels:manage + Idempotency-Key
//	GET    /api/vendors/{id}                  channels:read
//	PUT    /api/vendors/{id}                  channels:manage（派生 version 乐观锁）
//	DELETE /api/vendors/{id}                  channels:manage
//	GET    /api/vendors/search                channels:read（limit ≤100）
//	POST   /api/vendors/operations/preview    channels:manage_sensitive
//	POST   /api/vendors/operations            channels:manage_sensitive
//
// 设计要点：
//   - 目录与定价是两个域（F-06-22 硬边界）：本文件只写 model_meta / vendors；
//     唯一触达定价的是 batch-delete 的 clear_pricing 可选项，且额外要求
//     pricing:manage——否则 channels 管理员可以绕过定价权限删价。
//   - 派生字段（square_state / configured_channel_count / enable_groups /
//     vendor_name）不落库，读取时计算。为避免 N+1，列表页只发**固定条数**的
//     聚合查询（用户分组 + 启用渠道 + 供应商名），匹配在内存完成；目录是百级
//     规模，这比「每行一条 SQL」更容易测试，也与 store.ResolveMeta 的口径一致。
//   - name_rule 匹配语义必须与 store.modelNameMatches 一致（0 精确 / 1 前缀 /
//     2 包含 / 3 后缀）。store 的该函数未导出，这里保留一份逐字对应的实现，
//     由 TestModelCatalogDerivedNameRuleMatch 对齐两边（D1b 边界不允许改 store）。
//   - 同步两段：preview 拉取上游清单并把 **sha256(source_version) → 清单** 缓存
//     到 Redis（TTL 1h）；apply 只接受该缓存里的条目，因此「版本过期 / 条目在
//     上游消失」都能在写库前 409 拒绝。apply 写入展开后的条目以缓存清单为准，
//     客户端提交的条目值只用于选择，不作为写入真源（防篡改）。
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// ModelCatalogHandlerConfig 是目录端点的装配输入。Repo 必填；Redis 用于创建
// 幂等与同步清单缓存；Derived 缺失时派生字段按「无渠道」的保守值计算
// （测试/纯内存装配），生产由 NewSQLCatalogDerived(DB) 提供。
type ModelCatalogHandlerConfig struct {
	Repo    store.ModelCatalogRepo
	Redis   *redis.Client
	Authz   *rbac.AuthzMiddleware
	Audit   *audit.Logger
	Logger  *slog.Logger
	Derived CatalogDerivedReader
	// SyncSource 拉取上游清单原文。nil 表示未配置 model_sync.source_url：
	// preview 返回 503，而不是伪造空清单（空清单会让管理员误以为「上游没有模型」）。
	SyncSource func(ctx context.Context) ([]byte, error)
}

// CatalogDerivedReader 是派生字段的读取接缝（生产实现走固定条数的聚合查询）。
type CatalogDerivedReader interface {
	// Derive 为一批模型计算广场态、启用渠道数与启用分组。
	Derive(ctx context.Context, models []store.ModelMeta) (map[int64]ModelDerived, error)
	// VendorNames 返回供应商 id → 名称（仅用于展示）。
	VendorNames(ctx context.Context, ids []int64) (map[int64]string, error)
}

// ModelDerived 是 model_meta 的派生字段（不落库）。
type ModelDerived struct {
	SquareState            string
	ConfiguredChannelCount int64
	EnableGroups           []string
}

// ModelCatalogHandlers 持有目录端点依赖，可并发使用。
type ModelCatalogHandlers struct {
	repo       store.ModelCatalogRepo
	rdb        *redis.Client
	authz      *rbac.AuthzMiddleware
	audit      *audit.Logger
	log        *slog.Logger
	derived    CatalogDerivedReader
	syncSource func(ctx context.Context) ([]byte, error)
	now        time.Time
}

// 目录端点的授权动作。ResourceKind 把 model 与 vendor 的字段策略分开登记；
// ActionName 与权限键不同名（权限键是功能级，动作名是字段级策略键）。
var (
	catalogModelListAction     = rbac.AuthzAction{ResourceKind: "model", ActionName: "list"}
	catalogModelSearchAction   = rbac.AuthzAction{ResourceKind: "model", ActionName: "search"}
	catalogModelGetAction      = rbac.AuthzAction{ResourceKind: "model", ActionName: "get"}
	catalogModelCreateAction   = rbac.AuthzAction{ResourceKind: "model", ActionName: "create"}
	catalogModelUpdateAction   = rbac.AuthzAction{ResourceKind: "model", ActionName: "update"}
	catalogModelDeleteAction   = rbac.AuthzAction{ResourceKind: "model", ActionName: "delete"}
	catalogModelBatchDelAction = rbac.AuthzAction{ResourceKind: "model", ActionName: "batch_delete"}
	catalogModelMissingAction  = rbac.AuthzAction{ResourceKind: "model", ActionName: "missing"}
	catalogModelSyncPrevAction = rbac.AuthzAction{ResourceKind: "model", ActionName: "sync_preview"}
	catalogModelSyncAction     = rbac.AuthzAction{ResourceKind: "model", ActionName: "sync"}

	catalogVendorListAction    = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "list"}
	catalogVendorSearchAction  = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "search"}
	catalogVendorGetAction     = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "get"}
	catalogVendorCreateAction  = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "create"}
	catalogVendorUpdateAction  = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "update"}
	catalogVendorDeleteAction  = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "delete"}
	catalogVendorOpsPrevAction = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "operations_preview"}
	catalogVendorOpsAction     = rbac.AuthzAction{ResourceKind: "vendor", ActionName: "operations"}
)

// 目录端点的问题类型（RFC 7807）。自定义类型用于让前端按稳定标识分支，
// 而不是解析 detail 文案。
const (
	catalogProblemVendorReferenced   = "https://newtg.dev/problems/vendor_referenced"
	catalogProblemVendorConflict     = "https://newtg.dev/problems/vendor_conflict"
	catalogProblemChannelModelsEmpty = "https://newtg.dev/problems/channel_models_empty"
	catalogProblemEmptySelection     = "https://newtg.dev/problems/empty_selection"
	catalogProblemTooLarge           = "https://newtg.dev/problems/selection_too_large"
	catalogProblemSyncVersion        = "https://newtg.dev/problems/sync_version_expired"
	catalogProblemSyncBlocked        = "https://newtg.dev/problems/sync_blocked_model"
	catalogProblemSyncSource         = "https://newtg.dev/problems/sync_source_unavailable"
	catalogProblemSyncUnconfigured   = "https://newtg.dev/problems/sync_source_unconfigured"
)

const (
	catalogModelNameMaxRunes  = 128
	catalogVendorNameMaxRunes = 128
	catalogIconMaxRunes       = 128
	catalogSearchLimitMax     = 100
	catalogKeywordMaxRunes    = 128
	// catalogPageSizeAll 是「需要全量装载后再派生/排序」时的内部页大小。
	// 只用固定值翻页，不把 100 的对外上限泄漏给内部循环。
	catalogPageSizeAll = 100
	// catalogMaxPages 是内部全量翻页的防死循环上限（10 万行）。
	catalogMaxPages = 1000
	// catalogSyncManifestTTL 是 preview → apply 的版本缓存窗口（F-06-25：
	// 缓存 source_version 到 Redis，TTL 1h）。
	catalogSyncManifestTTL = time.Hour
	// catalogSyncManifestMaxBytes 是上游清单原文上限；清单是元数据，4MB 足够
	// 覆盖数千模型，同时避免把一次 preview 变成内存放大面。
	catalogSyncManifestMaxBytes = 4 << 20
)

var (
	// ErrSyncSourceNotConfigured：model_sync.source_url 未配置。
	ErrSyncSourceNotConfigured = errors.New("httpapi: model_sync.source_url is not configured")
	// ErrSyncSourceUnavailable：拉取失败 / 非 2xx / 响应过大 / 目标被拒绝。
	ErrSyncSourceUnavailable = errors.New("httpapi: upstream model manifest is unavailable")
)

// NewModelCatalogHandlers 构造目录端点。Redis 必填：创建幂等与同步版本缓存
// 都依赖它；缺失时静默退化成「每次新建」会破坏 F-06-25 的版本校验。
func NewModelCatalogHandlers(cfg ModelCatalogHandlerConfig) (*ModelCatalogHandlers, error) {
	if cfg.Repo == nil {
		return nil, errors.New("httpapi: ModelCatalogHandlerConfig.Repo is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: ModelCatalogHandlerConfig.Redis is required (idempotency, sync version cache)")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &ModelCatalogHandlers{
		repo: cfg.Repo, rdb: cfg.Redis, authz: authz, audit: cfg.Audit, log: logger,
		derived: cfg.Derived, syncSource: cfg.SyncSource,
	}
	if h.derived == nil {
		// 无派生读取面时按「没有任何启用渠道」计算：广场态 unavailable、
		// 渠道数 0、分组空。生产装配必须传 NewSQLCatalogDerived，否则列表页
		// 会低估可见性——因此 routes.go 在 DB 可用时总是传实现。
		h.derived = staticCatalogDerived{}
	}
	h.registerPolicies()
	return h, nil
}

// registerPolicies 登记每个写动作的字段白名单。未登记的动作对任何写请求
// 都会 403（security-02 §2.6.1 规则 6），因此预览这类「POST 但只读」的
// 动作也必须登记。
func (h *ModelCatalogHandlers) registerPolicies() {
	modelFields := func(extra ...string) *rbac.FieldPolicy {
		fields := map[string]*rbac.FieldPolicy{
			"model_name": nil, "display_name": nil, "description": nil, "icon": nil,
			"tags": nil, "vendor_id": nil, "endpoints": nil, "capabilities": nil,
			"modalities": nil, "parameters": nil, "context_length": nil, "max_output": nil,
			"knowledge_cutoff": nil, "released_at": nil, "status": nil,
			"sync_official": nil, "name_rule": nil,
		}
		for _, name := range extra {
			fields[name] = nil
		}
		return &rbac.FieldPolicy{Fields: fields}
	}
	h.authz.SetWritePolicy(catalogModelCreateAction, modelFields())
	h.authz.SetWritePolicy(catalogModelUpdateAction, modelFields("updated_at"))
	h.authz.SetWritePolicy(catalogModelDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"updated_at": nil,
	}})
	h.authz.SetWritePolicy(catalogModelBatchDelAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"ids": nil, "remove_from_channels": nil, "clear_pricing": nil,
	}})
	h.authz.SetWritePolicy(catalogModelSyncPrevAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	h.authz.SetWritePolicy(catalogModelSyncAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"source_version": nil, "models": nil, "vendors": nil,
	}})

	vendorFields := func(extra ...string) *rbac.FieldPolicy {
		fields := map[string]*rbac.FieldPolicy{
			"name": nil, "description": nil, "icon": nil, "status": nil,
		}
		for _, name := range extra {
			fields[name] = nil
		}
		return &rbac.FieldPolicy{Fields: fields}
	}
	h.authz.SetWritePolicy(catalogVendorCreateAction, vendorFields())
	h.authz.SetWritePolicy(catalogVendorUpdateAction, vendorFields("version"))
	// DELETE 无请求体，但仍要登记：未登记的写动作无条件 403。
	h.authz.SetWritePolicy(catalogVendorDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	opFields := &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"action": nil, "model_ids": nil, "vendor_ids": nil,
		"target_vendor_id": nil, "expected_version": nil,
	}}
	h.authz.SetWritePolicy(catalogVendorOpsPrevAction, opFields)
	h.authz.SetWritePolicy(catalogVendorOpsAction, opFields)
}

// RegisterModelCatalogRoutes 注册全部目录端点。wrap 语义同 RegisterPricingRoutes。
func (h *ModelCatalogHandlers) RegisterModelCatalogRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	fixed := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(required, action)(next)
	}

	// 模型目录。静态子路径先于 /api/models/{id} 注册（ServeMux 字面量优先，
	// 显式顺序让契约在阅读时也成立）。
	mount("GET /api/models", fixed(rbac.PermChannelsRead, catalogModelListAction, h.handleListModels))
	mount("GET /api/models/search", fixed(rbac.PermChannelsRead, catalogModelSearchAction, h.handleSearchModels))
	mount("GET /api/models/missing", fixed(rbac.PermChannelsRead, catalogModelMissingAction, h.handleMissingModels))
	mount("GET /api/models/sync/preview", fixed(rbac.PermChannelsManageSensitive, catalogModelSyncPrevAction, h.handleSyncPreview))
	mount("POST /api/models/sync", fixed(rbac.PermChannelsManageSensitive, catalogModelSyncAction, h.handleSyncApply))
	mount("POST /api/models/batch-delete", fixed(rbac.PermChannelsManageSensitive, catalogModelBatchDelAction,
		h.idempotentCreate("model-batch-delete", h.handleBatchDeleteModels)))
	mount("POST /api/models", fixed(rbac.PermChannelsManage, catalogModelCreateAction,
		h.idempotentCreate("model", h.handleCreateModel)))
	mount("GET /api/models/{id}", fixed(rbac.PermChannelsRead, catalogModelGetAction, h.handleGetModel))
	mount("PUT /api/models/{id}", fixed(rbac.PermChannelsManage, catalogModelUpdateAction, h.handleUpdateModel))
	mount("DELETE /api/models/{id}", fixed(rbac.PermChannelsManage, catalogModelDeleteAction, h.handleDeleteModel))

	// 供应商目录。
	mount("GET /api/vendors", fixed(rbac.PermChannelsRead, catalogVendorListAction, h.handleListVendors))
	mount("GET /api/vendors/search", fixed(rbac.PermChannelsRead, catalogVendorSearchAction, h.handleSearchVendors))
	mount("POST /api/vendors/operations/preview", fixed(rbac.PermChannelsManageSensitive, catalogVendorOpsPrevAction, h.handlePreviewVendorOperations))
	mount("POST /api/vendors/operations", fixed(rbac.PermChannelsManageSensitive, catalogVendorOpsAction, h.handleApplyVendorOperations))
	mount("POST /api/vendors", fixed(rbac.PermChannelsManage, catalogVendorCreateAction,
		h.idempotentCreate("vendor", h.handleCreateVendor)))
	mount("GET /api/vendors/{id}", fixed(rbac.PermChannelsRead, catalogVendorGetAction, h.handleGetVendor))
	mount("PUT /api/vendors/{id}", fixed(rbac.PermChannelsManage, catalogVendorUpdateAction, h.handleUpdateVendor))
	mount("DELETE /api/vendors/{id}", fixed(rbac.PermChannelsManage, catalogVendorDeleteAction, h.handleDeleteVendor))
}

// ---------------------------------------------------------------------------
// 响应 DTO
// ---------------------------------------------------------------------------

// modelMetaView 是 model_meta 的 20 列 + 四个派生字段（任务书 §2）。
// jsonb 列用 RawMessage 原样回显，避免二次编码改变数组/对象形态。
type modelMetaView struct {
	ID              int64           `json:"id"`
	ModelName       string          `json:"model_name"`
	DisplayName     *string         `json:"display_name"`
	Description     *string         `json:"description"`
	Icon            *string         `json:"icon"`
	Tags            json.RawMessage `json:"tags"`
	VendorID        *int64          `json:"vendor_id"`
	Endpoints       json.RawMessage `json:"endpoints"`
	Capabilities    json.RawMessage `json:"capabilities"`
	Modalities      json.RawMessage `json:"modalities"`
	Parameters      json.RawMessage `json:"parameters"`
	ContextLength   *int64          `json:"context_length"`
	MaxOutput       *int64          `json:"max_output"`
	KnowledgeCutoff *string         `json:"knowledge_cutoff"`
	// released_at 的列类型是 date，契约（schemas/admin.yaml）是 format: date。
	// 用 *time.Time 输出会得到 RFC 3339 date-time，与契约不一致；这里显式
	// 格式化为 YYYY-MM-DD。
	ReleasedAt   *string   `json:"released_at"`
	Status       int16     `json:"status"`
	SyncOfficial bool      `json:"sync_official"`
	NameRule     int16     `json:"name_rule"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	VendorName             *string  `json:"vendor_name"`
	SquareState            string   `json:"square_state"`
	ConfiguredChannelCount int64    `json:"configured_channel_count"`
	EnableGroups           []string `json:"enable_groups"`
}

type modelListView struct {
	Items    []modelMetaView `json:"items"`
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

type modelSearchView struct {
	ID          int64   `json:"id"`
	ModelName   string  `json:"model_name"`
	DisplayName *string `json:"display_name"`
	Status      int16   `json:"status"`
}

type modelSearchListView struct {
	Items []modelSearchView `json:"items"`
}

type missingModelView struct {
	ModelName    string `json:"model_name"`
	ChannelCount int64  `json:"channel_count"`
	HasPricing   bool   `json:"has_pricing"`
}

type missingListView struct {
	Items           []missingModelView `json:"items"`
	SkippedChannels int64              `json:"skipped_channels"`
	Total           int64              `json:"total"`
}

type vendorView struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Icon        *string   `json:"icon"`
	Status      int16     `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ModelCount  int64     `json:"model_count"`
	Version     string    `json:"version"`
}

type vendorListView struct {
	Items    []vendorView `json:"items"`
	Total    int64        `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
}

type vendorSearchView struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status int16  `json:"status"`
}

type vendorSearchListView struct {
	Items []vendorSearchView `json:"items"`
}

type batchDeleteResponse struct {
	Requested           int64 `json:"requested"`
	Deleted             int64 `json:"deleted"`
	ChannelsUpdated     int64 `json:"channels_updated"`
	PricingDeleted      int64 `json:"pricing_deleted"`
	GroupPricingDeleted int64 `json:"group_pricing_deleted"`
}

type vendorVersionRefView struct {
	VendorID int64  `json:"vendor_id"`
	Version  string `json:"version"`
}

type vendorConflictView struct {
	VendorID       int64  `json:"vendor_id"`
	CurrentVersion string `json:"current_version"`
}

type vendorPreviewResponse struct {
	Action          string                 `json:"action"`
	Affected        int64                  `json:"affected"`
	Conflict        bool                   `json:"conflict"`
	Conflicts       []vendorConflictView   `json:"conflicts"`
	ExpectedVersion string                 `json:"expected_version"`
	Vendors         []vendorVersionRefView `json:"vendors"`
}

type vendorOperationResponse struct {
	Affected int64 `json:"affected"`
}

// formatCatalogDate 把 date 列（UTC 零点）输出成契约要求的 YYYY-MM-DD。
func formatCatalogDate(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02")
	return &formatted
}

func newModelMetaView(rec store.ModelMeta) modelMetaView {
	return modelMetaView{
		ID: rec.ID, ModelName: rec.ModelName, DisplayName: rec.DisplayName,
		Description: rec.Description, Icon: rec.Icon,
		Tags:          rawJSONOr(rec.Tags, "[]"),
		VendorID:      rec.VendorID,
		Endpoints:     rawJSONOr(rec.Endpoints, "{}"),
		Capabilities:  rawJSONOr(rec.Capabilities, "[]"),
		Modalities:    rawJSONOr(rec.Modalities, "[]"),
		Parameters:    rawJSONOr(rec.Parameters, "[]"),
		ContextLength: rec.ContextLength, MaxOutput: rec.MaxOutput,
		KnowledgeCutoff: rec.KnowledgeCutoff, ReleasedAt: formatCatalogDate(rec.ReleasedAt),
		Status: rec.Status, SyncOfficial: rec.SyncOfficial, NameRule: rec.NameRule,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
		// 派生字段的零值：调用方必须用 applyDerived 填充；零值不是合法广场态，
		// 但只在编程错误时可见（不会来自 DB 数据）。
		EnableGroups: []string{},
	}
}

// rawJSONOr 把 nil jsonb 字节替换为列默认容器；DB 的 NOT NULL 默认值不会
// 产生 nil，但 store 的 Create/Update 返回值在部分路径上可能未装载该列。
func rawJSONOr(raw []byte, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return json.RawMessage(raw)
}

func newVendorView(rec store.Vendor) vendorView {
	return vendorView{
		ID: rec.ID, Name: rec.Name, Description: rec.Description, Icon: rec.Icon,
		Status: rec.Status, CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
		ModelCount: rec.ModelCount, Version: rec.Version(),
	}
}

// ---------------------------------------------------------------------------
// 错误输出
// ---------------------------------------------------------------------------

// writeCatalogProblem 输出 RFC 7807，并允许附加扩展成员（reference_count /
// conflicts / channel_ids）。Problem 的 additionalProperties 为 true，
// 这些扩展在契约内。
func writeCatalogProblem(w http.ResponseWriter, status int, problemType, title, detail string, extra map[string]any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := map[string]any{"type": problemType, "title": title, "status": status}
	if detail != "" {
		problem["detail"] = detail
	}
	for key, value := range extra {
		problem[key] = value
	}
	_ = json.NewEncoder(w).Encode(problem)
}

// writeCatalogFieldsProblem 输出字段级校验失败（422 + fields）。
func writeCatalogFieldsProblem(w http.ResponseWriter, detail string, fields []string) {
	extra := map[string]any{}
	if len(fields) > 0 {
		extra["fields"] = fields
	}
	writeCatalogProblem(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
		"Validation failed", detail, extra)
}

// writeModelCatalogError 把模型目录仓储错误映射为 HTTP 语义（任务书 §2 冻结）。
func (h *ModelCatalogHandlers) writeModelCatalogError(w http.ResponseWriter, op string, err error) {
	var invalid store.ErrModelMetaInvalid
	var emptyChannels store.ErrChannelModelsEmpty
	switch {
	case errors.Is(err, store.ErrModelMetaNotFound):
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
	case errors.Is(err, store.ErrModelMetaDuplicate):
		writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"a model with this model_name already exists", nil)
	case errors.Is(err, store.ErrModelMetaConflict):
		writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the model metadata was modified by another request; refresh and retry", nil)
	case errors.As(err, &invalid):
		// ErrModelMetaInvalid 的 Field 是列名，直接回填 problem.fields。
		fields := []string{}
		if invalid.Field != "" {
			fields = []string{invalid.Field}
		}
		detail := "model metadata is invalid"
		if invalid.Reason != "" {
			detail = invalid.Field + ": " + invalid.Reason
		}
		writeCatalogFieldsProblem(w, detail, fields)
	case errors.Is(err, store.ErrVendorNotFound):
		// 模型档案上下文里的 vendor_id 是请求体字段：引用无效 → 422，
		// 不是「被寻址资源不存在」的 404（store ErrVendorNotFound 的 L-1 口径）。
		writeCatalogFieldsProblem(w, "vendor_id does not reference an existing vendor", []string{"vendor_id"})
	case errors.As(err, &emptyChannels):
		extra := map[string]any{"channel_ids": emptyChannels.ChannelIDs}
		writeCatalogProblem(w, http.StatusUnprocessableEntity, catalogProblemChannelModelsEmpty,
			"Channel models cannot be emptied",
			"removing these models would leave the listed channels without any supported model", extra)
	case errors.Is(err, store.ErrCatalogEmptySelection):
		writeCatalogProblem(w, http.StatusUnprocessableEntity, catalogProblemEmptySelection,
			"Empty selection", "the selection is empty after deduplication", nil)
	case errors.Is(err, store.ErrCatalogTooLarge):
		writeCatalogProblem(w, http.StatusUnprocessableEntity, catalogProblemTooLarge,
			"Selection too large", "at most 1000 rows may be selected per request", nil)
	default:
		h.log.Error(op+" failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "", nil)
	}
}

// writeVendorCatalogError 把供应商目录仓储错误映射为 HTTP 语义。
// 与模型上下文的区别：这里的 ErrVendorNotFound 是**被寻址资源不存在** → 404。
func (h *ModelCatalogHandlers) writeVendorCatalogError(w http.ResponseWriter, op string, err error) {
	var referenced store.ErrVendorReferenced
	var versionConflict store.ErrVendorVersionConflict
	switch {
	case errors.Is(err, store.ErrVendorNotFound):
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
	case errors.Is(err, store.ErrVendorDuplicate):
		writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"a vendor with this name already exists (case-insensitive)", nil)
	case errors.Is(err, store.ErrVendorConflict):
		writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the vendor was modified by another request; refresh and retry", nil)
	case errors.Is(err, store.ErrVendorInvalid):
		writeCatalogFieldsProblem(w, "vendor input is invalid", []string{"name"})
	case errors.Is(err, store.ErrVendorOperationInvalid):
		writeCatalogFieldsProblem(w, "vendor operation parameters are invalid", []string{"action"})
	case errors.As(err, &referenced):
		writeCatalogProblem(w, http.StatusConflict, catalogProblemVendorReferenced,
			"Vendor is referenced",
			"the vendor is referenced by model metadata; merge it into another vendor or reassign the models first",
			map[string]any{"reference_count": referenced.ReferenceCount})
	case errors.As(err, &versionConflict):
		conflicts := make([]vendorConflictView, 0, len(versionConflict.Conflicts))
		for _, c := range versionConflict.Conflicts {
			conflicts = append(conflicts, vendorConflictView{VendorID: c.VendorID, CurrentVersion: c.CurrentVersion})
		}
		writeCatalogProblem(w, http.StatusConflict, catalogProblemVendorConflict,
			"Vendor version conflict",
			"the operation fingerprint is missing, expired or no longer matches; preview again",
			map[string]any{"conflicts": conflicts})
	case errors.Is(err, store.ErrModelMetaNotFound):
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
	case errors.Is(err, store.ErrCatalogEmptySelection):
		writeCatalogProblem(w, http.StatusUnprocessableEntity, catalogProblemEmptySelection,
			"Empty selection", "the selection is empty after deduplication", nil)
	case errors.Is(err, store.ErrCatalogTooLarge):
		writeCatalogProblem(w, http.StatusUnprocessableEntity, catalogProblemTooLarge,
			"Selection too large", "at most 1000 rows may be selected per request", nil)
	default:
		h.log.Error(op+" failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "", nil)
	}
}

// ---------------------------------------------------------------------------
// 请求解析（模型档案）
// ---------------------------------------------------------------------------

func parseCatalogPage(q url.Values) (int, int, error) {
	page, pageSize := 1, adminPageSizeDefault
	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > adminPageMax {
			return page, pageSize, fmt.Errorf("p must be between 1 and %d", adminPageMax)
		}
		page = value
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > adminPageSizeMax {
			return page, pageSize, fmt.Errorf("page_size must be between 1 and %d", adminPageSizeMax)
		}
		pageSize = value
	}
	return page, pageSize, nil
}

// parseCatalogStatus 管理面不宽容降级：非法枚举一律 400（任务书 §2）。
func parseCatalogStatus(raw string) (*int16, error) {
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

// parseSquareState 校验广场态筛选枚举。
func parseSquareState(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return "", nil
	case squareStateHidden, squareStateVisible, squareStatePartial, squareStateUnavailable:
		return strings.TrimSpace(raw), nil
	default:
		return "", errors.New("square_state must be one of hidden, visible, partial, unavailable")
	}
}

// squareState 三态 + hidden。
const (
	squareStateHidden      = "hidden"
	squareStateVisible     = "visible"
	squareStatePartial     = "partial"
	squareStateUnavailable = "unavailable"
)

// decodeNullableString 解析「字符串或 null」。返回 (nil,true) 表示显式 null。
func decodeNullableString(raw json.RawMessage) (*string, bool, error) {
	if string(raw) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, errors.New("must be a string or null")
	}
	return &value, true, nil
}

func decodeNullableInt64Field(raw json.RawMessage) (*int64, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("must be an integer or null")
	}
	return &value, nil
}

// decodeRawJSONArray 要求字段是 JSON 数组（显式 null 也拒绝：列是 NOT NULL，
// 且「清空」应提交 []，避免 null 与空数组两种表示）。
func decodeRawJSONArray(field string, raw json.RawMessage) ([]byte, error) {
	if string(raw) == "null" {
		return nil, fmt.Errorf("%s must be a JSON array, not null", field)
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, fmt.Errorf("%s must be a JSON array", field)
	}
	return []byte(raw), nil
}

func decodeRawJSONObject(field string, raw json.RawMessage) ([]byte, error) {
	if string(raw) == "null" {
		return nil, fmt.Errorf("%s must be a JSON object, not null", field)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", field)
	}
	return []byte(raw), nil
}

func validateModelName(name string) error {
	if utf8.RuneCountInString(name) > catalogModelNameMaxRunes {
		return fmt.Errorf("model_name must be at most %d characters", catalogModelNameMaxRunes)
	}
	// 与 model_meta_model_name_chk 同口径；放行 `/`（公开模型名 openai/gpt-5，D1-03）。
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '.', '_', ':', '/', '-':
			continue
		}
		return errors.New("model_name may only contain letters, digits and . _ : / -")
	}
	return nil
}

func validateCatalogIcon(icon *string) error {
	if icon != nil && utf8.RuneCountInString(*icon) > catalogIconMaxRunes {
		return fmt.Errorf("must be at most %d characters", catalogIconMaxRunes)
	}
	return nil
}

// validateCatalogDescription 与 platform-01 §22.7.2 的 512 字符上限一致。
// 供应商行没有 DB CHECK，必须在应用层拦住。
func validateCatalogDescription(value *string) error {
	if value != nil && utf8.RuneCountInString(*value) > 512 {
		return errors.New("must be at most 512 characters")
	}
	return nil
}

func decodeModelStatus(raw json.RawMessage) (*int16, error) {
	var status int16
	if err := json.Unmarshal(raw, &status); err != nil || (status != 1 && status != 2) {
		return nil, errors.New("status must be 1 (enabled) or 2 (disabled)")
	}
	return &status, nil
}

func decodeNameRule(raw json.RawMessage) (*int16, error) {
	var rule int16
	if err := json.Unmarshal(raw, &rule); err != nil || rule < 0 || rule > 3 {
		return nil, errors.New("name_rule must be one of 0 (exact), 1 (prefix), 2 (contains), 3 (suffix)")
	}
	return &rule, nil
}

// decodeReleasedAt 接受 RFC 3339 或 YYYY-MM-DD（DB 列是 date）。
func decodeReleasedAt(raw json.RawMessage) (*time.Time, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, errors.New("released_at must be a date string or null")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("released_at must be a date string or null")
	}
	if parsed, err := time.Parse("2006-01-02", token); err == nil {
		return &parsed, nil
	}
	parsed, err := time.Parse(time.RFC3339, token)
	if err != nil {
		return nil, errors.New("released_at must be YYYY-MM-DD or RFC 3339")
	}
	// date 列只保留日期；截断到 UTC 零点，避免时区把日期挪一天。
	utc := parsed.UTC().Truncate(24 * time.Hour)
	return &utc, nil
}

// buildModelMetaInput 把创建请求体转成 store 写入值。
func buildModelMetaInput(body map[string]json.RawMessage) (store.ModelMetaInput, error) {
	var input store.ModelMetaInput
	rawName, ok := body["model_name"]
	if !ok {
		return input, errors.New("model_name is required")
	}
	if err := json.Unmarshal(rawName, &input.ModelName); err != nil {
		return input, errors.New("model_name must be a string")
	}
	input.ModelName = strings.TrimSpace(input.ModelName)
	if input.ModelName == "" {
		return input, errors.New("model_name must not be blank")
	}
	if err := validateModelName(input.ModelName); err != nil {
		return input, err
	}

	for field, dst := range map[string]**string{
		"display_name": &input.DisplayName,
		"description":  &input.Description,
		"icon":         &input.Icon,
	} {
		raw, ok := body[field]
		if !ok {
			continue
		}
		value, _, err := decodeNullableString(raw)
		if err != nil {
			return input, fmt.Errorf("%s %v", field, err)
		}
		switch field {
		case "icon":
			if err := validateCatalogIcon(value); err != nil {
				return input, fmt.Errorf("icon %v", err)
			}
		case "description":
			if err := validateCatalogDescription(value); err != nil {
				return input, fmt.Errorf("description %v", err)
			}
		}
		*dst = value
	}

	if raw, ok := body["vendor_id"]; ok {
		value, err := decodeNullableInt64Field(raw)
		if err != nil {
			return input, errors.New("vendor_id must be an integer or null")
		}
		if value != nil && *value <= 0 {
			return input, errors.New("vendor_id must be a positive integer or null")
		}
		input.VendorID = value
	}

	arrays := map[string]*[]byte{
		"tags": &input.Tags, "capabilities": &input.Capabilities,
		"modalities": &input.Modalities, "parameters": &input.Parameters,
	}
	for field, dst := range arrays {
		raw, ok := body[field]
		if !ok {
			continue
		}
		value, err := decodeRawJSONArray(field, raw)
		if err != nil {
			return input, err
		}
		*dst = value
	}
	if raw, ok := body["endpoints"]; ok {
		value, err := decodeRawJSONObject("endpoints", raw)
		if err != nil {
			return input, err
		}
		input.Endpoints = value
	}

	if raw, ok := body["context_length"]; ok {
		value, err := decodeNullableInt64Field(raw)
		if err != nil {
			return input, errors.New("context_length must be an integer or null")
		}
		input.ContextLength = value
	}
	if raw, ok := body["max_output"]; ok {
		value, err := decodeNullableInt64Field(raw)
		if err != nil {
			return input, errors.New("max_output must be an integer or null")
		}
		input.MaxOutput = value
	}
	if raw, ok := body["knowledge_cutoff"]; ok {
		value, _, err := decodeNullableString(raw)
		if err != nil {
			return input, errors.New("knowledge_cutoff must be a string or null")
		}
		input.KnowledgeCutoff = value
	}
	if raw, ok := body["released_at"]; ok {
		value, err := decodeReleasedAt(raw)
		if err != nil {
			return input, err
		}
		input.ReleasedAt = value
	}
	if raw, ok := body["status"]; ok {
		value, err := decodeModelStatus(raw)
		if err != nil {
			return input, err
		}
		input.Status = value
	}
	if raw, ok := body["sync_official"]; ok {
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return input, errors.New("sync_official must be a boolean")
		}
		input.SyncOfficial = &value
	}
	if raw, ok := body["name_rule"]; ok {
		value, err := decodeNameRule(raw)
		if err != nil {
			return input, err
		}
		input.NameRule = *value
	}
	return input, nil
}

// buildModelMetaPatch 把更新请求体转成 store 的部分更新。Set 标记区分
// 「未提交」与「显式置空」：description=null 必须清除列值而不是保留。
func buildModelMetaPatch(body map[string]json.RawMessage) (store.ModelMetaPatch, error) {
	var patch store.ModelMetaPatch
	hasChange := false

	if raw, ok := body["model_name"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return patch, errors.New("model_name must be a string")
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return patch, errors.New("model_name must not be blank")
		}
		if err := validateModelName(name); err != nil {
			return patch, err
		}
		patch.ModelName = &name
		hasChange = true
	}

	strFields := []struct {
		name  string
		value **string
		set   *bool
	}{
		{"display_name", &patch.DisplayName, &patch.DisplayNameSet},
		{"description", &patch.Description, &patch.DescriptionSet},
		{"icon", &patch.Icon, &patch.IconSet},
	}
	for _, field := range strFields {
		raw, ok := body[field.name]
		if !ok {
			continue
		}
		value, _, err := decodeNullableString(raw)
		if err != nil {
			return patch, fmt.Errorf("%s %v", field.name, err)
		}
		if field.name == "icon" {
			if err := validateCatalogIcon(value); err != nil {
				return patch, fmt.Errorf("icon %v", err)
			}
		}
		*field.value = value
		*field.set = true
		hasChange = true
	}

	if raw, ok := body["vendor_id"]; ok {
		value, err := decodeNullableInt64Field(raw)
		if err != nil {
			return patch, errors.New("vendor_id must be an integer or null")
		}
		if value != nil && *value <= 0 {
			return patch, errors.New("vendor_id must be a positive integer or null")
		}
		patch.VendorID = value
		patch.VendorIDSet = true
		hasChange = true
	}

	arrayFields := []struct {
		name  string
		value *[]byte
		set   *bool
	}{
		{"tags", &patch.Tags, &patch.TagsSet},
		{"capabilities", &patch.Capabilities, &patch.CapabilitiesSet},
		{"modalities", &patch.Modalities, &patch.ModalitiesSet},
		{"parameters", &patch.Parameters, &patch.ParametersSet},
	}
	for _, field := range arrayFields {
		raw, ok := body[field.name]
		if !ok {
			continue
		}
		value, err := decodeRawJSONArray(field.name, raw)
		if err != nil {
			return patch, err
		}
		*field.value = value
		*field.set = true
		hasChange = true
	}
	if raw, ok := body["endpoints"]; ok {
		value, err := decodeRawJSONObject("endpoints", raw)
		if err != nil {
			return patch, err
		}
		patch.Endpoints = value
		patch.EndpointsSet = true
		hasChange = true
	}

	if raw, ok := body["context_length"]; ok {
		value, err := decodeNullableInt64Field(raw)
		if err != nil {
			return patch, errors.New("context_length must be an integer or null")
		}
		patch.ContextLength = value
		patch.ContextLengthSet = true
		hasChange = true
	}
	if raw, ok := body["max_output"]; ok {
		value, err := decodeNullableInt64Field(raw)
		if err != nil {
			return patch, errors.New("max_output must be an integer or null")
		}
		patch.MaxOutput = value
		patch.MaxOutputSet = true
		hasChange = true
	}
	if raw, ok := body["knowledge_cutoff"]; ok {
		value, _, err := decodeNullableString(raw)
		if err != nil {
			return patch, errors.New("knowledge_cutoff must be a string or null")
		}
		patch.KnowledgeCutoff = value
		patch.KnowledgeCutoffSet = true
		hasChange = true
	}
	if raw, ok := body["released_at"]; ok {
		value, err := decodeReleasedAt(raw)
		if err != nil {
			return patch, err
		}
		patch.ReleasedAt = value
		patch.ReleasedAtSet = true
		hasChange = true
	}
	if raw, ok := body["status"]; ok {
		value, err := decodeModelStatus(raw)
		if err != nil {
			return patch, err
		}
		patch.Status = value
		hasChange = true
	}
	if raw, ok := body["sync_official"]; ok {
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return patch, errors.New("sync_official must be a boolean")
		}
		patch.SyncOfficial = &value
		hasChange = true
	}
	if raw, ok := body["name_rule"]; ok {
		value, err := decodeNameRule(raw)
		if err != nil {
			return patch, err
		}
		patch.NameRule = value
		hasChange = true
	}
	if !hasChange {
		return patch, errors.New("no fields to update")
	}
	return patch, nil
}

// parsePathID 解析 {id}；非法/非正数按「资源不存在」处理（404），
// 避免把路径拼写错误映射成 500 或泄露 SQL 细节。
func parsePathID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// 模型目录：列表 / 搜索 / 详情
// ---------------------------------------------------------------------------

func (h *ModelCatalogHandlers) handleListModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, pageSize, err := parseCatalogPage(q)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	keyword := strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(keyword) > catalogKeywordMaxRunes {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "keyword is too long", nil)
		return
	}
	status, err := parseCatalogStatus(q.Get("status"))
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	squareState, err := parseSquareState(q.Get("square_state"))
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	sortKey, err := parseModelSort(q.Get("sort"))
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	filter := store.ModelMetaFilter{Keyword: keyword, Status: status}
	if raw := strings.TrimSpace(q.Get("vendor_id")); raw != "" {
		vendorID, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || vendorID <= 0 {
			writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"vendor_id must be a positive integer", nil)
			return
		}
		filter.VendorID = &vendorID
	}

	// 【为什么全量装载】store.ListModelMeta 只按 model_name 排序，而本端点
	// 支持任意排序列与**派生**广场态筛选；两者都要求看到完整结果集才能给出
	// 正确的 total 与分页。目录是百级规模，固定 100/页的翻页成本可接受。
	all, err := h.loadAllModels(r.Context(), filter)
	if err != nil {
		h.writeModelCatalogError(w, "list model metadata", err)
		return
	}
	views, err := h.buildModelViews(r.Context(), all)
	if err != nil {
		h.log.Error("derive model catalog fields failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "", nil)
		return
	}
	if squareState != "" {
		filtered := make([]modelMetaView, 0, len(views))
		for _, view := range views {
			if view.SquareState == squareState {
				filtered = append(filtered, view)
			}
		}
		views = filtered
	}
	sortModelViews(views, sortKey)
	total := int64(len(views))
	start, end := paginate(len(views), page, pageSize)
	writeJSON(w, http.StatusOK, modelListView{
		Items: views[start:end], Total: total, Page: page, PageSize: pageSize,
	})
}

// loadAllModels 以固定页大小翻页装载全部匹配行。
func (h *ModelCatalogHandlers) loadAllModels(ctx context.Context, filter store.ModelMetaFilter) ([]store.ModelMeta, error) {
	out := make([]store.ModelMeta, 0, 64)
	for page := 1; page <= catalogMaxPages; page++ {
		filter.Page, filter.PageSize = page, catalogPageSizeAll
		items, total, err := h.repo.ListModelMeta(ctx, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) == 0 || int64(len(out)) >= total {
			break
		}
	}
	return out, nil
}

func (h *ModelCatalogHandlers) loadAllVendors(ctx context.Context, filter store.VendorFilter) ([]store.Vendor, error) {
	out := make([]store.Vendor, 0, 32)
	for page := 1; page <= catalogMaxPages; page++ {
		filter.Page, filter.PageSize = page, catalogPageSizeAll
		items, total, err := h.repo.ListVendors(ctx, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) == 0 || int64(len(out)) >= total {
			break
		}
	}
	return out, nil
}

func paginate(length, page, pageSize int) (int, int) {
	start := (page - 1) * pageSize
	if start > length {
		start = length
	}
	end := start + pageSize
	if end > length {
		end = length
	}
	return start, end
}

// buildModelViews 批量补齐派生字段（固定条数查询，不做逐行查询）。
func (h *ModelCatalogHandlers) buildModelViews(ctx context.Context, models []store.ModelMeta) ([]modelMetaView, error) {
	derived, err := h.derived.Derive(ctx, models)
	if err != nil {
		return nil, err
	}
	vendorIDs := make([]int64, 0, len(models))
	for _, rec := range models {
		if rec.VendorID != nil {
			vendorIDs = append(vendorIDs, *rec.VendorID)
		}
	}
	names, err := h.derived.VendorNames(ctx, vendorIDs)
	if err != nil {
		return nil, err
	}
	views := make([]modelMetaView, 0, len(models))
	for _, rec := range models {
		view := newModelMetaView(rec)
		if rec.VendorID != nil {
			if name, ok := names[*rec.VendorID]; ok {
				view.VendorName = &name
			}
		}
		info, ok := derived[rec.ID]
		if !ok {
			info = ModelDerived{SquareState: squareStateUnavailable, EnableGroups: []string{}}
		}
		if rec.Status == 2 {
			// status=2 ⇒ hidden 是**绝对**规则：即使仍有启用渠道在跑该模型，
			// 停用的目录档案也不得出现在公开广场（F-06-22）。
			info.SquareState = squareStateHidden
		}
		if info.EnableGroups == nil {
			info.EnableGroups = []string{}
		}
		view.SquareState = info.SquareState
		view.ConfiguredChannelCount = info.ConfiguredChannelCount
		view.EnableGroups = info.EnableGroups
		views = append(views, view)
	}
	return views, nil
}

func (h *ModelCatalogHandlers) loadModelView(ctx context.Context, rec store.ModelMeta) (modelMetaView, error) {
	views, err := h.buildModelViews(ctx, []store.ModelMeta{rec})
	if err != nil {
		return modelMetaView{}, err
	}
	return views[0], nil
}

func (h *ModelCatalogHandlers) handleSearchModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(keyword) > catalogKeywordMaxRunes {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "keyword is too long", nil)
		return
	}
	limit := catalogSearchLimitMax
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > catalogSearchLimitMax {
			writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("limit must be between 1 and %d", catalogSearchLimitMax), nil)
			return
		}
		limit = value
	}
	status, err := parseCatalogStatus(q.Get("status"))
	if err != nil {
		// 管理面不宽容降级：非法枚举 400。
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	items, err := h.repo.SearchModelMeta(r.Context(), keyword, status, limit)
	if err != nil {
		h.writeModelCatalogError(w, "search model metadata", err)
		return
	}
	views := make([]modelSearchView, 0, len(items))
	for _, rec := range items {
		views = append(views, modelSearchView{
			ID: rec.ID, ModelName: rec.ModelName, DisplayName: rec.DisplayName, Status: rec.Status,
		})
	}
	writeJSON(w, http.StatusOK, modelSearchListView{Items: views})
}

func (h *ModelCatalogHandlers) handleGetModel(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
		return
	}
	rec, err := h.repo.GetModelMeta(r.Context(), id)
	if err != nil {
		h.writeModelCatalogError(w, "get model metadata", err)
		return
	}
	view, err := h.loadModelView(r.Context(), rec)
	if err != nil {
		h.log.Error("derive model catalog fields failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ---------------------------------------------------------------------------
// 模型目录：创建 / 更新 / 删除
// ---------------------------------------------------------------------------

func (h *ModelCatalogHandlers) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	input, err := buildModelMetaInput(body)
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), nil)
		return
	}
	rec, err := h.repo.CreateModelMeta(r.Context(), input)
	if err != nil {
		h.writeModelCatalogError(w, "create model metadata", err)
		return
	}
	h.writeAudit(r, audit.ActionModelCreate, map[string]any{
		"id": rec.ID, "model_name": rec.ModelName,
	})
	view, err := h.loadModelView(r.Context(), rec)
	if err != nil {
		h.log.Error("derive model catalog fields failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *ModelCatalogHandlers) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	rawUpdatedAt, ok := body["updated_at"]
	if !ok {
		// 乐观锁版本缺失是「请求不完整」→ 400（任务书 §2），不是 409。
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking", nil)
		return
	}
	expectUpdatedAt, err := parseUpdatedAt(rawUpdatedAt)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	patch, err := buildModelMetaPatch(body)
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), nil)
		return
	}
	rec, err := h.repo.UpdateModelMeta(r.Context(), id, patch, expectUpdatedAt)
	if err != nil {
		h.writeModelCatalogError(w, "update model metadata", err)
		return
	}
	h.writeAudit(r, audit.ActionModelUpdate, map[string]any{
		"id": rec.ID, "model_name": rec.ModelName,
	})
	view, err := h.loadModelView(r.Context(), rec)
	if err != nil {
		h.log.Error("derive model catalog fields failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *ModelCatalogHandlers) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("updated_at"))
	if raw == "" {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking", nil)
		return
	}
	expectUpdatedAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at must be an RFC 3339 timestamp", nil)
		return
	}
	// 【为什么读一次】审计 detail 需要 model_name；真正的乐观锁由
	// DeleteModelMetaVersioned 的 `WHERE id = $1 AND updated_at = $2` 保证：
	// 读取之后、删除之前若有其他请求提交了 UPDATE，删除命中 0 行并返回 409，
	// 不会删掉管理员没看到的版本。读取本身不再承担校验职责。
	current, err := h.repo.GetModelMeta(r.Context(), id)
	if err != nil {
		h.writeModelCatalogError(w, "read model metadata before delete", err)
		return
	}
	if err := h.repo.DeleteModelMetaVersioned(r.Context(), id, expectUpdatedAt); err != nil {
		h.writeModelCatalogError(w, "delete model metadata", err)
		return
	}
	h.writeAudit(r, audit.ActionModelDelete, map[string]any{
		"id": current.ID, "model_name": current.ModelName,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *ModelCatalogHandlers) handleBatchDeleteModels(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	rawIDs, ok := body["ids"]
	if !ok {
		writeCatalogFieldsProblem(w, "ids is required", []string{"ids"})
		return
	}
	var ids []int64
	if err := json.Unmarshal(rawIDs, &ids); err != nil {
		writeCatalogFieldsProblem(w, "ids must be an array of integers", []string{"ids"})
		return
	}
	removeFromChannels, err := decodeOptionalBool(body, "remove_from_channels")
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), []string{"remove_from_channels"})
		return
	}
	clearPricing, err := decodeOptionalBool(body, "clear_pricing")
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), []string{"clear_pricing"})
		return
	}
	if clearPricing {
		// 【为什么这里单独判权限】路由的 required 是 channels:manage_sensitive；
		// 清定价属于**定价管理面**，只有 channels 权限的管理员不能借批量删除的
		// 副作用删掉 model_pricing/group_pricing 行（F-06-22 权限冻结）。
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil || !rbac.HasPermission(*p, rbac.PermPricingManage) {
			writeCatalogProblem(w, http.StatusForbidden,
				"https://newtg.dev/problems/field_not_writable", "Forbidden",
				"clear_pricing=true requires the pricing:manage permission", nil)
			return
		}
	}
	result, err := h.repo.BatchDeleteModelMeta(r.Context(), ids, removeFromChannels, clearPricing)
	if err != nil {
		h.writeModelCatalogError(w, "batch delete model metadata", err)
		return
	}
	h.writeAudit(r, audit.ActionModelBatchDelete, map[string]any{
		"requested": result.Requested, "deleted": result.Deleted,
		"channels_updated":      result.ChannelsUpdated,
		"pricing_deleted":       result.PricingDeleted,
		"group_pricing_deleted": result.GroupPricingDeleted,
	})
	writeJSON(w, http.StatusOK, batchDeleteResponse{
		Requested: result.Requested, Deleted: result.Deleted,
		ChannelsUpdated:     result.ChannelsUpdated,
		PricingDeleted:      result.PricingDeleted,
		GroupPricingDeleted: result.GroupPricingDeleted,
	})
}

func decodeOptionalBool(body map[string]json.RawMessage, field string) (bool, error) {
	raw, ok := body[field]
	if !ok {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("%s must be a boolean", field)
	}
	return value, nil
}

func (h *ModelCatalogHandlers) handleMissingModels(w http.ResponseWriter, r *http.Request) {
	result, err := h.repo.MissingModels(r.Context())
	if err != nil {
		h.writeModelCatalogError(w, "detect missing models", err)
		return
	}
	items := make([]missingModelView, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, missingModelView{
			ModelName: item.ModelName, ChannelCount: item.ChannelCount, HasPricing: item.HasPricing,
		})
	}
	writeJSON(w, http.StatusOK, missingListView{
		Items: items, SkippedChannels: result.SkippedChannels, Total: int64(len(items)),
	})
}

// ---------------------------------------------------------------------------
// 模型排序
// ---------------------------------------------------------------------------

func parseModelSort(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return "model_name", nil
	case "model_name", "-model_name", "status", "-status",
		"updated_at", "-updated_at", "created_at", "-created_at":
		return strings.TrimSpace(raw), nil
	default:
		return "", errors.New("sort must be one of model_name, -model_name, status, -status, updated_at, -updated_at, created_at, -created_at")
	}
}

func sortModelViews(views []modelMetaView, key string) {
	desc := strings.HasPrefix(key, "-")
	field := strings.TrimPrefix(key, "-")
	less := func(i, j int) bool { return views[i].ModelName < views[j].ModelName }
	switch field {
	case "status":
		less = func(i, j int) bool { return views[i].Status < views[j].Status }
	case "updated_at":
		less = func(i, j int) bool { return views[i].UpdatedAt.Before(views[j].UpdatedAt) }
	case "created_at":
		less = func(i, j int) bool { return views[i].CreatedAt.Before(views[j].CreatedAt) }
	}
	sort.SliceStable(views, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

// ---------------------------------------------------------------------------
// 供应商目录
// ---------------------------------------------------------------------------

func (h *ModelCatalogHandlers) handleListVendors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, pageSize, err := parseCatalogPage(q)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	keyword := strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(keyword) > catalogKeywordMaxRunes {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "keyword is too long", nil)
		return
	}
	status, err := parseCatalogStatus(q.Get("status"))
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	var attached *bool
	switch strings.TrimSpace(q.Get("attached")) {
	case "":
	case "true":
		value := true
		attached = &value
	case "false":
		value := false
		attached = &value
	default:
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"attached must be true or false", nil)
		return
	}
	sortKey, err := parseVendorSort(q.Get("sort"))
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error(), nil)
		return
	}
	filter := store.VendorFilter{Keyword: keyword, Status: status, Attached: attached}
	all, err := h.loadAllVendors(r.Context(), filter)
	if err != nil {
		h.writeVendorCatalogError(w, "list vendors", err)
		return
	}
	views := make([]vendorView, 0, len(all))
	for _, rec := range all {
		views = append(views, newVendorView(rec))
	}
	sortVendorViews(views, sortKey)
	total := int64(len(views))
	start, end := paginate(len(views), page, pageSize)
	writeJSON(w, http.StatusOK, vendorListView{
		Items: views[start:end], Total: total, Page: page, PageSize: pageSize,
	})
}

func parseVendorSort(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return "name", nil
	case "name", "-name", "model_count", "-model_count",
		"updated_at", "-updated_at", "created_at", "-created_at":
		return strings.TrimSpace(raw), nil
	default:
		return "", errors.New("sort must be one of name, -name, model_count, -model_count, updated_at, -updated_at, created_at, -created_at")
	}
}

func sortVendorViews(views []vendorView, key string) {
	desc := strings.HasPrefix(key, "-")
	field := strings.TrimPrefix(key, "-")
	less := func(i, j int) bool { return strings.ToLower(views[i].Name) < strings.ToLower(views[j].Name) }
	switch field {
	case "model_count":
		less = func(i, j int) bool { return views[i].ModelCount < views[j].ModelCount }
	case "updated_at":
		less = func(i, j int) bool { return views[i].UpdatedAt.Before(views[j].UpdatedAt) }
	case "created_at":
		less = func(i, j int) bool { return views[i].CreatedAt.Before(views[j].CreatedAt) }
	}
	sort.SliceStable(views, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

func (h *ModelCatalogHandlers) handleSearchVendors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(keyword) > catalogKeywordMaxRunes {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "keyword is too long", nil)
		return
	}
	limit := catalogSearchLimitMax
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > catalogSearchLimitMax {
			writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("limit must be between 1 and %d", catalogSearchLimitMax), nil)
			return
		}
		limit = value
	}
	items, err := h.repo.SearchVendors(r.Context(), keyword, limit)
	if err != nil {
		h.writeVendorCatalogError(w, "search vendors", err)
		return
	}
	views := make([]vendorSearchView, 0, len(items))
	for _, rec := range items {
		views = append(views, vendorSearchView{ID: rec.ID, Name: rec.Name, Status: rec.Status})
	}
	writeJSON(w, http.StatusOK, vendorSearchListView{Items: views})
}

func (h *ModelCatalogHandlers) handleGetVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
		return
	}
	rec, err := h.repo.GetVendor(r.Context(), id)
	if err != nil {
		h.writeVendorCatalogError(w, "get vendor", err)
		return
	}
	writeJSON(w, http.StatusOK, newVendorView(rec))
}

// buildVendorInput 解析创建体；status 缺省 1。
func buildVendorInput(body map[string]json.RawMessage) (store.VendorInput, error) {
	var input store.VendorInput
	rawName, ok := body["name"]
	if !ok {
		return input, errors.New("name is required")
	}
	if err := json.Unmarshal(rawName, &input.Name); err != nil {
		return input, errors.New("name must be a string")
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return input, errors.New("name must not be blank")
	}
	if utf8.RuneCountInString(input.Name) > 128 {
		return input, errors.New("name must be at most 128 characters")
	}
	for field, dst := range map[string]**string{
		"description": &input.Description,
		"icon":        &input.Icon,
	} {
		raw, ok := body[field]
		if !ok {
			continue
		}
		value, _, err := decodeNullableString(raw)
		if err != nil {
			return input, fmt.Errorf("%s %v", field, err)
		}
		switch field {
		case "icon":
			if err := validateCatalogIcon(value); err != nil {
				return input, fmt.Errorf("icon %v", err)
			}
		case "description":
			if err := validateCatalogDescription(value); err != nil {
				return input, fmt.Errorf("description %v", err)
			}
		}
		*dst = value
	}
	if raw, ok := body["status"]; ok {
		value, err := decodeModelStatus(raw)
		if err != nil {
			return input, errors.New("status must be 1 (enabled) or 2 (disabled)")
		}
		input.Status = value
	}
	return input, nil
}

func (h *ModelCatalogHandlers) handleCreateVendor(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	input, err := buildVendorInput(body)
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), []string{"name"})
		return
	}
	rec, err := h.repo.CreateVendor(r.Context(), input)
	if err != nil {
		h.writeVendorCatalogError(w, "create vendor", err)
		return
	}
	h.writeAudit(r, audit.ActionVendorCreate, map[string]any{"id": rec.ID, "name": rec.Name})
	writeJSON(w, http.StatusCreated, newVendorView(rec))
}

func (h *ModelCatalogHandlers) handleUpdateVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	rawVersion, ok := body["version"]
	if !ok {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"version is required for optimistic locking", nil)
		return
	}
	var expectVersion string
	if err := json.Unmarshal(rawVersion, &expectVersion); err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"version must be a string", nil)
		return
	}
	current, err := h.repo.GetVendor(r.Context(), id)
	if err != nil {
		h.writeVendorCatalogError(w, "read vendor before update", err)
		return
	}
	update := store.VendorUpdate{
		Name: current.Name, Description: current.Description,
		Icon: current.Icon, Status: current.Status,
	}
	if raw, ok := body["name"]; ok {
		if err := json.Unmarshal(raw, &update.Name); err != nil {
			writeCatalogFieldsProblem(w, "name must be a string", []string{"name"})
			return
		}
		update.Name = strings.TrimSpace(update.Name)
		if update.Name == "" {
			writeCatalogFieldsProblem(w, "name must not be blank", []string{"name"})
			return
		}
		if utf8.RuneCountInString(update.Name) > 128 {
			writeCatalogFieldsProblem(w, "name must be at most 128 characters", []string{"name"})
			return
		}
	}
	if raw, ok := body["description"]; ok {
		value, _, err := decodeNullableString(raw)
		if err != nil {
			writeCatalogFieldsProblem(w, "description must be a string or null", []string{"description"})
			return
		}
		if err := validateCatalogDescription(value); err != nil {
			writeCatalogFieldsProblem(w, "description "+err.Error(), []string{"description"})
			return
		}
		update.Description = value
	}
	if raw, ok := body["icon"]; ok {
		value, _, err := decodeNullableString(raw)
		if err != nil {
			writeCatalogFieldsProblem(w, "icon must be a string or null", []string{"icon"})
			return
		}
		if err := validateCatalogIcon(value); err != nil {
			writeCatalogFieldsProblem(w, "icon "+err.Error(), []string{"icon"})
			return
		}
		update.Icon = value
	}
	if raw, ok := body["status"]; ok {
		value, err := decodeModelStatus(raw)
		if err != nil {
			writeCatalogFieldsProblem(w, err.Error(), []string{"status"})
			return
		}
		update.Status = *value
	}
	rec, err := h.repo.UpdateVendor(r.Context(), id, update, expectVersion)
	if err != nil {
		h.writeVendorCatalogError(w, "update vendor", err)
		return
	}
	h.writeAudit(r, audit.ActionVendorUpdate, map[string]any{"id": rec.ID, "name": rec.Name})
	writeJSON(w, http.StatusOK, newVendorView(rec))
}

func (h *ModelCatalogHandlers) handleDeleteVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		writeCatalogProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "", nil)
		return
	}
	// 先读名称用于审计（删除后行已不存在）；读取失败按仓储错误映射。
	rec, err := h.repo.GetVendor(r.Context(), id)
	if err != nil {
		h.writeVendorCatalogError(w, "read vendor before delete", err)
		return
	}
	if err := h.repo.DeleteVendor(r.Context(), id); err != nil {
		h.writeVendorCatalogError(w, "delete vendor", err)
		return
	}
	h.writeAudit(r, audit.ActionVendorDelete, map[string]any{"id": rec.ID, "name": rec.Name})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 供应商批量操作
// ---------------------------------------------------------------------------

// parseVendorOperation 解析 preview/apply 共用入参。
// parseVendorOperation 解析批量操作请求，并返回出错字段名：RFC 7807 的
// fields 必须指向实际非法字段（target_vendor_id / model_ids / ...），而不是
// 一律把 action 报成坏字段。
func parseVendorOperation(body map[string]json.RawMessage) (store.VendorOperation, string, error) {
	var op store.VendorOperation
	rawAction, ok := body["action"]
	if !ok {
		return op, "action", errors.New("action is required")
	}
	if err := json.Unmarshal(rawAction, &op.Action); err != nil {
		return op, "action", errors.New("action must be a string")
	}
	op.Action = strings.TrimSpace(op.Action)
	switch op.Action {
	case store.VendorActionAssign, store.VendorActionMerge, store.VendorActionDelete:
	default:
		return op, "action", errors.New("action must be assign, merge or delete")
	}
	if raw, ok := body["model_ids"]; ok {
		if err := json.Unmarshal(raw, &op.ModelIDs); err != nil {
			return op, "model_ids", errors.New("model_ids must be an array of integers")
		}
	}
	if raw, ok := body["vendor_ids"]; ok {
		if err := json.Unmarshal(raw, &op.VendorIDs); err != nil {
			return op, "vendor_ids", errors.New("vendor_ids must be an array of integers")
		}
	}
	if raw, ok := body["target_vendor_id"]; ok {
		if err := json.Unmarshal(raw, &op.TargetVendorID); err != nil || op.TargetVendorID < 0 {
			return op, "target_vendor_id", errors.New("target_vendor_id must be a non-negative integer")
		}
	}
	if raw, ok := body["expected_version"]; ok {
		if err := json.Unmarshal(raw, &op.ExpectedVersion); err != nil {
			return op, "expected_version", errors.New("expected_version must be a string")
		}
		op.ExpectedVersion = strings.TrimSpace(op.ExpectedVersion)
	}
	return op, "", nil
}

func (h *ModelCatalogHandlers) handlePreviewVendorOperations(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	op, field, err := parseVendorOperation(body)
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), []string{field})
		return
	}
	preview, err := h.repo.PreviewVendorOperations(r.Context(), op)
	if err != nil {
		h.writeVendorCatalogError(w, "preview vendor operations", err)
		return
	}
	conflicts := make([]vendorConflictView, 0, len(preview.Conflicts))
	for _, conflict := range preview.Conflicts {
		conflicts = append(conflicts, vendorConflictView{
			VendorID: conflict.VendorID, CurrentVersion: conflict.CurrentVersion,
		})
	}
	vendors := make([]vendorVersionRefView, 0, len(preview.Vendors))
	for _, vendor := range preview.Vendors {
		vendors = append(vendors, vendorVersionRefView{VendorID: vendor.VendorID, Version: vendor.Version})
	}
	writeJSON(w, http.StatusOK, vendorPreviewResponse{
		Action: op.Action, Affected: preview.Affected, Conflict: preview.Conflict,
		Conflicts: conflicts, ExpectedVersion: preview.ExpectedVersion, Vendors: vendors,
	})
}

func (h *ModelCatalogHandlers) handleApplyVendorOperations(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	op, field, err := parseVendorOperation(body)
	if err != nil {
		writeCatalogFieldsProblem(w, err.Error(), []string{field})
		return
	}
	// 空版本亦视为冲突（F-06-24 / store.verifyVendorVersion 同口径）：
	// 必须先 preview 拿到指纹，不能靠「没有版本」绕过并发保护。
	if strings.TrimSpace(op.ExpectedVersion) == "" {
		writeCatalogProblem(w, http.StatusConflict, catalogProblemVendorConflict, "Vendor version conflict",
			"expected_version is required; call preview first", map[string]any{"conflicts": []vendorConflictView{}})
		return
	}
	result, err := h.repo.ApplyVendorOperations(r.Context(), op)
	if err != nil {
		h.writeVendorCatalogError(w, "apply vendor operations", err)
		return
	}
	h.writeAudit(r, audit.ActionVendorOperations, map[string]any{
		"action": op.Action, "affected": result.Affected, "target_vendor_id": op.TargetVendorID,
	})
	writeJSON(w, http.StatusOK, vendorOperationResponse{Affected: result.Affected})
}

// ---------------------------------------------------------------------------
// 上游元数据同步（preview → apply）
// ---------------------------------------------------------------------------

// syncManifestModel 是上游清单的模型条目（任务书 §2 冻结格式）。
//
// Present 记录「字段存在且非 null」。缺失与显式 null 同义（都不参与
// diff/patch，缺失 ≠ 清空），但仍显式区分「有值」：只看指针会把
// `"display_name": null` 与 `"display_name": "x"` 之外的形态混在一起，
// preview 与 apply 的口径就难以自证一致。
type syncManifestModel struct {
	ModelName     string          `json:"model_name"`
	DisplayName   *string         `json:"display_name"`
	Description   *string         `json:"description"`
	Icon          *string         `json:"icon"`
	Tags          json.RawMessage `json:"tags"`
	Capabilities  json.RawMessage `json:"capabilities"`
	ContextLength *int64          `json:"context_length"`
	MaxOutput     *int64          `json:"max_output"`

	Present map[string]bool `json:"-"`
}

func (m *syncManifestModel) UnmarshalJSON(data []byte) error {
	type plain syncManifestModel
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = syncManifestModel(decoded)
	m.Present = presentNonNilFields(raw, "display_name", "description", "icon", "tags", "capabilities",
		"context_length", "max_output")
	// 显式 null 与缺失同义：归一化为「未提供」，避免把 "null" 当数组/对象写库。
	if !m.Present["tags"] {
		m.Tags = nil
	}
	if !m.Present["capabilities"] {
		m.Capabilities = nil
	}
	return nil
}

type syncManifestVendor struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Icon        *string `json:"icon"`

	Present map[string]bool `json:"-"`
}

func (v *syncManifestVendor) UnmarshalJSON(data []byte) error {
	type plain syncManifestVendor
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*v = syncManifestVendor(decoded)
	v.Present = presentNonNilFields(raw, "description", "icon")
	return nil
}

// presentNonNilFields 返回「键存在且不是 JSON null」的字段集合。
func presentNonNilFields(raw map[string]json.RawMessage, fields ...string) map[string]bool {
	present := make(map[string]bool, len(fields))
	for _, field := range fields {
		value, ok := raw[field]
		if !ok {
			continue
		}
		if string(bytes.TrimSpace(value)) == "null" {
			continue
		}
		present[field] = true
	}
	return present
}

type syncManifest struct {
	Models  []syncManifestModel  `json:"models"`
	Vendors []syncManifestVendor `json:"vendors"`
	Version string               `json:"version"`
}

type syncFieldChange struct {
	Local    any `json:"local"`
	Upstream any `json:"upstream"`
}

type syncModelCandidate struct {
	ModelName string                     `json:"model_name"`
	Changes   map[string]syncFieldChange `json:"changes,omitempty"`
}

type syncVendorCandidate struct {
	Name    string                     `json:"name"`
	Changes map[string]syncFieldChange `json:"changes,omitempty"`
}

type syncVendorGroups struct {
	Create    []string              `json:"create"`
	Update    []syncVendorCandidate `json:"update"`
	Unchanged []string              `json:"unchanged"`
}

type syncPreviewGroups struct {
	Create          []syncModelCandidate `json:"create"`
	Update          []syncModelCandidate `json:"update"`
	Unchanged       []string             `json:"unchanged"`
	Blocked         []string             `json:"blocked"`
	MissingUpstream []string             `json:"missing_upstream"`
	MissingVendor   []string             `json:"missing_vendor"`
	Vendors         syncVendorGroups     `json:"vendors"`
}

type syncPreviewResponse struct {
	SourceVersion string            `json:"source_version"`
	Groups        syncPreviewGroups `json:"groups"`
}

// syncApplyRequest 的条目只用于选择；写入值取自缓存的上游清单（防篡改）。
type syncApplyRequest struct {
	SourceVersion string               `json:"source_version"`
	Models        []syncManifestModel  `json:"models"`
	Vendors       []syncManifestVendor `json:"vendors"`
}

type syncApplyResponse struct {
	SourceVersion  string   `json:"source_version"`
	CreatedModels  []string `json:"created_models"`
	UpdatedModels  []string `json:"updated_models"`
	CreatedVendors []string `json:"created_vendors"`
	UpdatedVendors []string `json:"updated_vendors"`
}

// syncManifestCacheKey 是 source_version → 清单原文的 Redis 键。
// 版本本身已是内容 sha256，键无需再带时间：过期由 TTL 控制。
func syncManifestCacheKey(version string) string {
	return "newtg:model_sync:manifest:" + version
}

func (h *ModelCatalogHandlers) handleSyncPreview(w http.ResponseWriter, r *http.Request) {
	if h.syncSource == nil {
		writeCatalogProblem(w, http.StatusServiceUnavailable, catalogProblemSyncUnconfigured,
			"Upstream sync is not configured", "configure model_sync.source_url before previewing", nil)
		return
	}
	raw, err := h.syncSource(r.Context())
	if err != nil {
		if errors.Is(err, ErrSyncSourceNotConfigured) {
			writeCatalogProblem(w, http.StatusServiceUnavailable, catalogProblemSyncUnconfigured,
				"Upstream sync is not configured", "configure model_sync.source_url before previewing", nil)
			return
		}
		// 固定文案：不把上游 URL、TLS/网络细节写进响应（出站面信息最小化）。
		h.log.Warn("model sync preview failed", "error", err)
		writeCatalogProblem(w, http.StatusBadGateway, catalogProblemSyncSource,
			"Upstream manifest is unavailable", "the configured upstream manifest could not be read", nil)
		return
	}
	manifest, err := parseSyncManifest(raw)
	if err != nil {
		h.log.Warn("model sync manifest is invalid", "error", err)
		writeCatalogProblem(w, http.StatusBadGateway, catalogProblemSyncSource,
			"Upstream manifest is invalid", "the upstream manifest does not match the expected schema", nil)
		return
	}
	sum := sha256.Sum256(raw)
	version := hex.EncodeToString(sum[:])
	// 缓存原文：apply 以缓存清单为写入真源，并据此判定「条目已在上游消失」。
	if err := h.rdb.Set(r.Context(), syncManifestCacheKey(version), raw, catalogSyncManifestTTL).Err(); err != nil {
		h.log.Error("cache model sync manifest failed", "error", err)
		writeCatalogProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Service temporarily unavailable", "", nil)
		return
	}

	localModels, err := h.loadAllModels(r.Context(), store.ModelMetaFilter{})
	if err != nil {
		h.writeModelCatalogError(w, "preview sync: list model metadata", err)
		return
	}
	localVendors, err := h.loadAllVendors(r.Context(), store.VendorFilter{})
	if err != nil {
		h.writeVendorCatalogError(w, "preview sync: list vendors", err)
		return
	}
	groups := buildSyncPreview(localModels, localVendors, manifest)
	writeJSON(w, http.StatusOK, syncPreviewResponse{SourceVersion: version, Groups: groups})
}

// parseSyncManifest 解析并做结构性校验；任何未知形状按 502（不是 422：
// 这是上游数据问题，管理员改本地请求无法修复）。
//
// 【名称统一裁剪（L4）】preview 的匹配键、apply 的写入值都取自这里，必须在
// 同一点裁剪，否则 `" gpt-4 "` 会与本地 `"gpt-4"` 对不上，preview 判 create、
// apply 又建出同名带空格的档案。
func parseSyncManifest(raw []byte) (syncManifest, error) {
	var manifest syncManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	// 尾随数据（例如两个 JSON 文档拼接）不是合法清单：静默忽略会让人误以为
	// 后半段也被同步了（L5）。
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return manifest, errors.New("manifest must contain exactly one JSON document")
	}
	seen := make(map[string]struct{}, len(manifest.Models))
	for i, model := range manifest.Models {
		name := strings.TrimSpace(model.ModelName)
		if name == "" {
			return manifest, fmt.Errorf("models[%d].model_name is required", i)
		}
		if err := validateModelName(name); err != nil {
			return manifest, fmt.Errorf("models[%d].model_name: %w", i, err)
		}
		if _, dup := seen[name]; dup {
			return manifest, fmt.Errorf("models[%d].model_name %q is duplicated", i, name)
		}
		seen[name] = struct{}{}
		manifest.Models[i].ModelName = name
	}
	vendorSeen := make(map[string]struct{}, len(manifest.Vendors))
	for i, vendor := range manifest.Vendors {
		name := strings.TrimSpace(vendor.Name)
		if name == "" {
			return manifest, fmt.Errorf("vendors[%d].name is required", i)
		}
		if utf8.RuneCountInString(name) > catalogVendorNameMaxRunes {
			return manifest, fmt.Errorf("vendors[%d].name must be at most %d characters", i, catalogVendorNameMaxRunes)
		}
		// vendor 行没有 DB CHECK：上游数据问题必须在入口按 502 拦住，
		// 否则会在 apply 阶段变成 422（客户端输入错误的语义）。
		if err := validateCatalogDescription(vendor.Description); err != nil {
			return manifest, fmt.Errorf("vendors[%d].description %w", i, err)
		}
		if err := validateCatalogIcon(vendor.Icon); err != nil {
			return manifest, fmt.Errorf("vendors[%d].icon %w", i, err)
		}
		key := strings.ToLower(name)
		if _, dup := vendorSeen[key]; dup {
			return manifest, fmt.Errorf("vendors[%d].name %q is duplicated", i, name)
		}
		vendorSeen[key] = struct{}{}
		manifest.Vendors[i].Name = name
	}
	return manifest, nil
}

// buildSyncPreview 对账上游清单与本地目录，给出六类候选。
//
// missing_vendor 的口径：本地模型的 vendor_id 指向不存在的供应商行
// （逻辑引用没有 FK，只能由检测面暴露）。上游模型条目格式不含 vendor 字段
// （任务书 §2 冻结），因此不存在「上游引用了不存在供应商」这一类。
func buildSyncPreview(localModels []store.ModelMeta, localVendors []store.Vendor, manifest syncManifest) syncPreviewGroups {
	groups := syncPreviewGroups{
		Create: []syncModelCandidate{}, Update: []syncModelCandidate{},
		Unchanged: []string{}, Blocked: []string{},
		MissingUpstream: []string{}, MissingVendor: []string{},
		Vendors: syncVendorGroups{Create: []string{}, Update: []syncVendorCandidate{}, Unchanged: []string{}},
	}
	upstream := make(map[string]syncManifestModel, len(manifest.Models))
	for _, model := range manifest.Models {
		upstream[model.ModelName] = model
	}
	localByName := make(map[string]store.ModelMeta, len(localModels))
	vendorIDs := make(map[int64]struct{}, len(localVendors))
	for _, vendor := range localVendors {
		vendorIDs[vendor.ID] = struct{}{}
	}
	for _, model := range localModels {
		localByName[model.ModelName] = model
		if model.VendorID != nil {
			if _, ok := vendorIDs[*model.VendorID]; !ok {
				groups.MissingVendor = append(groups.MissingVendor, model.ModelName)
			}
		}
		if _, ok := upstream[model.ModelName]; !ok {
			groups.MissingUpstream = append(groups.MissingUpstream, model.ModelName)
		}
	}
	for _, model := range manifest.Models {
		local, exists := localByName[model.ModelName]
		if !exists {
			groups.Create = append(groups.Create, syncModelCandidate{ModelName: model.ModelName})
			continue
		}
		if !local.SyncOfficial {
			// sync_official=false 是本地锁定：preview 只标 blocked，apply 永不覆盖。
			groups.Blocked = append(groups.Blocked, model.ModelName)
			continue
		}
		changes := diffSyncModel(local, model)
		if len(changes) == 0 {
			groups.Unchanged = append(groups.Unchanged, model.ModelName)
			continue
		}
		groups.Update = append(groups.Update,
			syncModelCandidate{ModelName: model.ModelName, Changes: changes})
	}
	sort.Strings(groups.Unchanged)
	sort.Strings(groups.Blocked)
	sort.Strings(groups.MissingUpstream)
	sort.Strings(groups.MissingVendor)

	localVendorsByName := make(map[string]store.Vendor, len(localVendors))
	for _, vendor := range localVendors {
		localVendorsByName[strings.ToLower(vendor.Name)] = vendor
	}
	for _, vendor := range manifest.Vendors {
		local, exists := localVendorsByName[strings.ToLower(vendor.Name)]
		if !exists {
			groups.Vendors.Create = append(groups.Vendors.Create, vendor.Name)
			continue
		}
		changes := diffSyncVendor(local, vendor)
		if len(changes) == 0 {
			groups.Vendors.Unchanged = append(groups.Vendors.Unchanged, local.Name)
			continue
		}
		groups.Vendors.Update = append(groups.Vendors.Update,
			syncVendorCandidate{Name: local.Name, Changes: changes})
	}
	sort.Strings(groups.Vendors.Create)
	sort.Strings(groups.Vendors.Unchanged)
	return groups
}

// diffSyncModel 只比较上游清单提供字段；上游缺失或显式 null 的字段不参与
// 差异（缺失 ≠ 清空）。preview 与 apply 共用 Present 口径。
func diffSyncModel(local store.ModelMeta, upstream syncManifestModel) map[string]syncFieldChange {
	changes := map[string]syncFieldChange{}
	add := func(field string, localValue, upstreamValue any) {
		if !reflect.DeepEqual(localValue, upstreamValue) {
			changes[field] = syncFieldChange{Local: localValue, Upstream: upstreamValue}
		}
	}
	if upstream.Present["display_name"] {
		add("display_name", derefString(local.DisplayName), derefString(upstream.DisplayName))
	}
	if upstream.Present["description"] {
		add("description", derefString(local.Description), derefString(upstream.Description))
	}
	if upstream.Present["icon"] {
		add("icon", derefString(local.Icon), derefString(upstream.Icon))
	}
	if upstream.Present["tags"] {
		add("tags", jsonValue(local.Tags, "[]"), jsonValue(upstream.Tags, "[]"))
	}
	if upstream.Present["capabilities"] {
		add("capabilities", jsonValue(local.Capabilities, "[]"), jsonValue(upstream.Capabilities, "[]"))
	}
	if upstream.Present["context_length"] {
		add("context_length", derefInt64(local.ContextLength), derefInt64(upstream.ContextLength))
	}
	if upstream.Present["max_output"] {
		add("max_output", derefInt64(local.MaxOutput), derefInt64(upstream.MaxOutput))
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

func diffSyncVendor(local store.Vendor, upstream syncManifestVendor) map[string]syncFieldChange {
	changes := map[string]syncFieldChange{}
	if upstream.Present["description"] &&
		!reflect.DeepEqual(derefString(local.Description), derefString(upstream.Description)) {
		changes["description"] = syncFieldChange{
			Local: derefString(local.Description), Upstream: derefString(upstream.Description),
		}
	}
	if upstream.Present["icon"] &&
		!reflect.DeepEqual(derefString(local.Icon), derefString(upstream.Icon)) {
		changes["icon"] = syncFieldChange{
			Local: derefString(local.Icon), Upstream: derefString(upstream.Icon),
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

func derefString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func derefInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// jsonValue 把 jsonb 原文解成可比较的 Go 值；空/非法时回退为 fallback 容器
// （列默认值），避免 NULL 与 [] 被误判成差异。
func jsonValue(raw json.RawMessage, fallback string) any {
	if len(raw) == 0 {
		raw = json.RawMessage(fallback)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

func (h *ModelCatalogHandlers) handleSyncApply(w http.ResponseWriter, r *http.Request) {
	body, err := readRawObject(r)
	if err != nil {
		writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object", nil)
		return
	}
	var request syncApplyRequest
	if raw, ok := body["source_version"]; ok {
		if err := json.Unmarshal(raw, &request.SourceVersion); err != nil {
			writeCatalogFieldsProblem(w, "source_version must be a string", []string{"source_version"})
			return
		}
	}
	request.SourceVersion = strings.TrimSpace(request.SourceVersion)
	if request.SourceVersion == "" {
		// 缺失与过期同口径：apply 必须回传 preview 的返回值（F-06-25）。
		writeCatalogProblem(w, http.StatusConflict, catalogProblemSyncVersion,
			"Sync source version is missing", "call GET /api/models/sync/preview first", nil)
		return
	}
	if raw, ok := body["models"]; ok {
		if err := json.Unmarshal(raw, &request.Models); err != nil {
			writeCatalogFieldsProblem(w, "models must be an array of model entries", []string{"models"})
			return
		}
	}
	if raw, ok := body["vendors"]; ok {
		if err := json.Unmarshal(raw, &request.Vendors); err != nil {
			writeCatalogFieldsProblem(w, "vendors must be an array of vendor entries", []string{"vendors"})
			return
		}
	}
	if len(request.Models) == 0 && len(request.Vendors) == 0 {
		writeCatalogProblem(w, http.StatusUnprocessableEntity, catalogProblemEmptySelection,
			"Empty selection", "select at least one model or vendor entry to apply", nil)
		return
	}

	cached, err := h.rdb.Get(r.Context(), syncManifestCacheKey(request.SourceVersion)).Bytes()
	if err != nil {
		writeCatalogProblem(w, http.StatusConflict, catalogProblemSyncVersion,
			"Sync source version is expired",
			"the preview result is missing or expired; run preview again", nil)
		return
	}
	manifest, err := parseSyncManifest(cached)
	if err != nil {
		// 缓存内容坏了等同版本失效；不是客户端输入错误。
		h.log.Error("cached model sync manifest is invalid", "error", err)
		writeCatalogProblem(w, http.StatusConflict, catalogProblemSyncVersion,
			"Sync source version is expired", "run preview again", nil)
		return
	}
	manifestModels := make(map[string]syncManifestModel, len(manifest.Models))
	for _, model := range manifest.Models {
		manifestModels[model.ModelName] = model
	}
	manifestVendors := make(map[string]syncManifestVendor, len(manifest.Vendors))
	for _, vendor := range manifest.Vendors {
		manifestVendors[strings.ToLower(strings.TrimSpace(vendor.Name))] = vendor
	}

	// 1) 选择集去重：匹配键是模型 model_name 精确、供应商 lower(name)
	//    （任务书 §2）。重复项内容一致 ⇒ 幂等合并；内容不一致 ⇒ 422——
	//    同一键携带两种写入意图时无法判定优先级，静默取一个会掩盖客户端 bug。
	modelSelections := make([]syncManifestModel, 0, len(request.Models))
	seenModels := make(map[string]syncManifestModel, len(request.Models))
	for i, selected := range request.Models {
		name := strings.TrimSpace(selected.ModelName)
		if name == "" {
			writeCatalogFieldsProblem(w, fmt.Sprintf("models[%d].model_name is required", i), []string{"models"})
			return
		}
		selected.ModelName = name
		if previous, ok := seenModels[name]; ok {
			if !reflect.DeepEqual(previous, selected) {
				writeCatalogFieldsProblem(w,
					fmt.Sprintf("models[%d] duplicates model_name %q with conflicting content", i, name),
					[]string{"models"})
				return
			}
			continue
		}
		seenModels[name] = selected
		modelSelections = append(modelSelections, selected)
	}
	vendorSelections := make([]syncManifestVendor, 0, len(request.Vendors))
	seenVendors := make(map[string]syncManifestVendor, len(request.Vendors))
	for i, selected := range request.Vendors {
		name := strings.TrimSpace(selected.Name)
		if name == "" {
			writeCatalogFieldsProblem(w, fmt.Sprintf("vendors[%d].name is required", i), []string{"vendors"})
			return
		}
		selected.Name = name
		key := strings.ToLower(name)
		if previous, ok := seenVendors[key]; ok {
			if !sameSyncVendorSelection(previous, selected) {
				writeCatalogFieldsProblem(w,
					fmt.Sprintf("vendors[%d] duplicates name %q with conflicting content", i, name),
					[]string{"vendors"})
				return
			}
			continue
		}
		seenVendors[key] = selected
		vendorSelections = append(vendorSelections, selected)
	}

	// 2) 选择集校验：条目必须仍在本次 preview 的清单里（消失 ⇒ 409 整体拒绝）。
	//    写入值取自服务端缓存的清单，请求条目只用于选择（防篡改）。
	selectedModels := make([]store.ModelSyncModelEntry, 0, len(modelSelections))
	for _, selected := range modelSelections {
		model, ok := manifestModels[selected.ModelName]
		if !ok {
			writeCatalogProblem(w, http.StatusConflict, catalogProblemSyncVersion,
				"Sync source version conflict",
				fmt.Sprintf("model %q no longer exists in the previewed manifest; run preview again", selected.ModelName), nil)
			return
		}
		selectedModels = append(selectedModels, store.ModelSyncModelEntry{
			ModelName: model.ModelName, DisplayName: model.DisplayName,
			Description: model.Description, Icon: model.Icon,
			Tags: model.Tags, Capabilities: model.Capabilities,
			ContextLength: model.ContextLength, MaxOutput: model.MaxOutput,
		})
	}
	selectedVendors := make([]store.ModelSyncVendorEntry, 0, len(vendorSelections))
	for _, selected := range vendorSelections {
		vendor, ok := manifestVendors[strings.ToLower(selected.Name)]
		if !ok {
			writeCatalogProblem(w, http.StatusConflict, catalogProblemSyncVersion,
				"Sync source version conflict",
				fmt.Sprintf("vendor %q no longer exists in the previewed manifest; run preview again", selected.Name), nil)
			return
		}
		selectedVendors = append(selectedVendors, store.ModelSyncVendorEntry{
			Name: vendor.Name, Description: vendor.Description, Icon: vendor.Icon,
		})
	}

	// 3) 单事务写入。store 在事务内重新读取现状（create/update、sync_official
	//    锁定），因此这里不依赖事务外的快照；任一条失败整体回滚。
	result, applyErr := h.repo.ApplyModelSync(r.Context(), selectedVendors, selectedModels)
	if applyErr != nil {
		h.writeSyncApplyError(w, "apply model sync", applyErr)
		return
	}
	response := syncApplyResponse{
		SourceVersion:  request.SourceVersion,
		CreatedModels:  result.CreatedModels,
		UpdatedModels:  result.UpdatedModels,
		CreatedVendors: result.CreatedVendors,
		UpdatedVendors: result.UpdatedVendors,
	}

	h.writeAudit(r, audit.ActionModelSync, map[string]any{
		"source_version":  request.SourceVersion,
		"created_models":  len(response.CreatedModels),
		"updated_models":  len(response.UpdatedModels),
		"created_vendors": len(response.CreatedVendors),
		"updated_vendors": len(response.UpdatedVendors),
	})
	writeJSON(w, http.StatusOK, response)
}

// sameSyncVendorSelection 判断两个选择条目内容是否一致（大小写不同的同名
// 供应商是同一键，内容按字段比较）。
func sameSyncVendorSelection(a, b syncManifestVendor) bool {
	return strings.EqualFold(a.Name, b.Name) &&
		sameOptionalString(a.Description, b.Description) &&
		sameOptionalString(a.Icon, b.Icon)
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// writeSyncApplyError 把同步事务错误统一映射为 409（唯一约束/乐观锁）或
// 422（字段校验），blocked 仍是 400。
func (h *ModelCatalogHandlers) writeSyncApplyError(w http.ResponseWriter, op string, err error) {
	var blocked store.ErrSyncBlocked
	var invalid store.ErrModelMetaInvalid
	switch {
	case errors.As(err, &blocked):
		writeCatalogProblem(w, http.StatusBadRequest, catalogProblemSyncBlocked,
			"Blocked model selected",
			fmt.Sprintf("model %q has sync_official=false and cannot be overwritten", blocked.ModelName), nil)
	case errors.Is(err, store.ErrModelMetaDuplicate),
		errors.Is(err, store.ErrModelMetaConflict),
		errors.Is(err, store.ErrVendorDuplicate),
		errors.Is(err, store.ErrVendorConflict):
		writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the sync conflicted with concurrent catalog changes; no changes were applied, run preview again", nil)
	case errors.As(err, &invalid):
		fields := []string{}
		if invalid.Field != "" {
			fields = []string{invalid.Field}
		}
		detail := "model metadata is invalid"
		if invalid.Reason != "" {
			detail = invalid.Field + ": " + invalid.Reason
		}
		writeCatalogFieldsProblem(w, detail, fields)
	case errors.Is(err, store.ErrVendorInvalid):
		writeCatalogFieldsProblem(w, "vendor input is invalid", []string{"name"})
	default:
		h.log.Error(op+" failed", "error", err)
		writeCatalogProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "", nil)
	}
}

// ---------------------------------------------------------------------------
// 派生字段（广场态 / 启用渠道数 / 启用分组）
// ---------------------------------------------------------------------------

// SQLCatalogDerived 是派生字段的生产实现。
//
// 【为什么在 httpapi 而不是 store】D1b 的文件边界禁止改 store；这组读取面
// 只服务展示，不构成写入不变量。实现固定为 3 条查询（用户分组、启用渠道、
// 供应商名），匹配在内存完成——目录百级规模，避免逐模型查询。
type SQLCatalogDerived struct {
	db *sql.DB
}

// NewSQLCatalogDerived 构造派生读取面。
func NewSQLCatalogDerived(db *sql.DB) *SQLCatalogDerived {
	return &SQLCatalogDerived{db: db}
}

// staticCatalogDerived 是无派生数据源时的保底实现（单元测试/无 DB 装配）。
type staticCatalogDerived struct{}

func (staticCatalogDerived) Derive(_ context.Context, models []store.ModelMeta) (map[int64]ModelDerived, error) {
	out := make(map[int64]ModelDerived, len(models))
	for _, model := range models {
		out[model.ID] = ModelDerived{SquareState: squareStateUnavailable, EnableGroups: []string{}}
	}
	return out, nil
}

func (staticCatalogDerived) VendorNames(_ context.Context, _ []int64) (map[int64]string, error) {
	return map[int64]string{}, nil
}

func (d *SQLCatalogDerived) Derive(ctx context.Context, models []store.ModelMeta) (map[int64]ModelDerived, error) {
	out := make(map[int64]ModelDerived, len(models))
	if len(models) == 0 {
		return out, nil
	}
	groups, err := d.enabledGroups(ctx)
	if err != nil {
		return nil, err
	}
	channels, err := d.enabledChannels(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		info := ModelDerived{SquareState: squareStateUnavailable, EnableGroups: []string{}}
		covered := make(map[string]struct{})
		allGroups := false
		var matchedChannels int64
		for _, channel := range channels {
			if !channelSupportsModel(channel.models, model.ModelName, model.NameRule) {
				continue
			}
			matchedChannels++
			if channel.allGroups {
				allGroups = true
				continue
			}
			for _, group := range channel.groups {
				covered[group] = struct{}{}
			}
		}
		info.ConfiguredChannelCount = matchedChannels
		info.EnableGroups = renderEnableGroups(groups, covered, allGroups)
		switch {
		case matchedChannels == 0:
			info.SquareState = squareStateUnavailable
		case allGroups || coversAll(groups, covered):
			info.SquareState = squareStateVisible
		case len(info.EnableGroups) > 0:
			info.SquareState = squareStatePartial
		default:
			info.SquareState = squareStateUnavailable
		}
		out[model.ID] = info
	}
	return out, nil
}

func (d *SQLCatalogDerived) VendorNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	unique := dedupeInt64(ids)
	rows, err := d.db.QueryContext(ctx, `SELECT id, name FROM vendors WHERE id = ANY($1::bigint[])`, unique)
	if err != nil {
		return nil, fmt.Errorf("httpapi: load vendor names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("httpapi: scan vendor name: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: iterate vendor names: %w", err)
	}
	return out, nil
}

// enabledGroups 返回启用的用户分组名（升序）。
func (d *SQLCatalogDerived) enabledGroups(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT name FROM user_groups WHERE status = 1 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("httpapi: load user groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	groups := make([]string, 0, 8)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("httpapi: scan user group: %w", err)
		}
		groups = append(groups, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: iterate user groups: %w", err)
	}
	return groups, nil
}

type derivedChannel struct {
	models    []string
	groups    []string
	allGroups bool
}

// enabledChannels 读取启用渠道的 supported_models 与 group_tags。
//
// group_tags 为 SQL NULL 表示「全部分组」（channels 的既有语义）。空数组
// 被 channels_group_tags_chk 拒绝，但历史数据仍可能以 '[]' 出现：按「无分组」
// 处理，不把它误当成全部分组。
func (d *SQLCatalogDerived) enabledChannels(ctx context.Context) ([]derivedChannel, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT supported_models, group_tags FROM channels WHERE status = 1`)
	if err != nil {
		return nil, fmt.Errorf("httpapi: load enabled channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	channels := make([]derivedChannel, 0, 16)
	for rows.Next() {
		var supported, tags []byte
		if err := rows.Scan(&supported, &tags); err != nil {
			return nil, fmt.Errorf("httpapi: scan channel: %w", err)
		}
		channel := derivedChannel{models: jsonStringArray(supported)}
		if tags == nil {
			channel.allGroups = true
		} else {
			channel.groups = jsonStringArray(tags)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: iterate channels: %w", err)
	}
	return channels, nil
}

// jsonStringArray 解出 JSON 字符串数组；非字符串元素被忽略（用于展示派生，
// 不影响路由——缺失检测的严格口径另见 store.MissingModels）。
func jsonStringArray(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if value, ok := item.(string); ok {
			out = append(out, value)
		}
	}
	return out
}

// channelSupportsModel 判断渠道的模型清单是否命中目录模式。
// 语义与 store.modelNameMatches 逐字对应（D1-02）。
func channelSupportsModel(models []string, pattern string, rule int16) bool {
	for _, name := range models {
		switch rule {
		case 0:
			if name == pattern {
				return true
			}
		case 1:
			if strings.HasPrefix(name, pattern) {
				return true
			}
		case 2:
			if strings.Contains(name, pattern) {
				return true
			}
		case 3:
			if strings.HasSuffix(name, pattern) {
				return true
			}
		}
	}
	return false
}

func renderEnableGroups(all []string, covered map[string]struct{}, allGroups bool) []string {
	if allGroups {
		out := append([]string(nil), all...)
		sort.Strings(out)
		if out == nil {
			out = []string{}
		}
		return out
	}
	out := make([]string, 0, len(covered))
	for _, group := range all {
		if _, ok := covered[group]; ok {
			out = append(out, group)
		}
	}
	return out
}

func coversAll(all []string, covered map[string]struct{}) bool {
	if len(all) == 0 {
		// 没有启用分组时「覆盖全部」为空真；广场态仍有启用渠道 → visible。
		return true
	}
	for _, group := range all {
		if _, ok := covered[group]; !ok {
			return false
		}
	}
	return true
}

func dedupeInt64(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// ---------------------------------------------------------------------------
// 上游清单拉取（SSRF 防护的最小实现）
// ---------------------------------------------------------------------------

// syncEgressResolver 是同步出站的 DNS 缝隙（仅测试；生产为 nil = 系统解析器）。
var syncEgressResolver func(ctx context.Context, host string) ([]net.IP, error)

// NewModelSyncSourceFromConfig 从配置中心读取 model_sync.source_url 构造拉取函数。
// 配置读取失败/空值时返回 ErrSyncSourceNotConfigured（preview → 503）。
func NewModelSyncSourceFromConfig(cfg ConfigReader) func(ctx context.Context) ([]byte, error) {
	if cfg == nil {
		return nil
	}
	return func(ctx context.Context) ([]byte, error) {
		raw, ok := cfg.Get("model_sync.source_url")
		if !ok {
			return nil, ErrSyncSourceNotConfigured
		}
		var sourceURL string
		if err := json.Unmarshal(raw, &sourceURL); err != nil {
			return nil, ErrSyncSourceNotConfigured
		}
		sourceURL = strings.TrimSpace(sourceURL)
		if sourceURL == "" {
			return nil, ErrSyncSourceNotConfigured
		}
		guard := egress.NewGuard(egress.PolicyFromConfig(cfg))
		if syncEgressResolver != nil {
			guard = guard.WithResolver(syncEgressResolver)
		}
		return fetchSyncManifest(ctx, sourceURL, guard)
	}
}

// fetchSyncManifest 拉取上游清单。
//
// 【统一出站策略】security-04 §2.4 / B2：协议、端口、userinfo、域名与
// 解析后的全部地址由 internal/egress 校验；拨号直连已校验 IP（反
// rebinding）；重定向每一跳重新校验，上限 sec.ssrf.max_redirects。
// 本函数只保留同步特有的语义：10s 总超时、Accept 头、4MB 上限与 2xx。
func fetchSyncManifest(ctx context.Context, rawURL string, guard *egress.Guard) ([]byte, error) {
	// 只做形状校验，**不预先解析**：解析发生在执行器的 DialContext，
	// 一次解析、校验全部 IP、再拨已校验 IP（冻结语义 3）。
	if _, err := guard.ValidateURLShape(rawURL); err != nil {
		return nil, ErrSyncSourceUnavailable
	}
	client := guard.NewClient(egress.ClientOptions{
		// 总超时沿用 D1b 的 10s；连接超时由 sec.ssrf.connect_timeout_seconds
		// 控制（执行器内部使用）。
		Timeout:         10 * time.Second,
		FollowRedirects: true,
	})
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, ErrSyncSourceUnavailable
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrSyncSourceUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrSyncSourceUnavailable
	}
	limited := io.LimitReader(response.Body, catalogSyncManifestMaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) > catalogSyncManifestMaxBytes {
		return nil, ErrSyncSourceUnavailable
	}
	return body, nil
}

// validateSyncURL 与 isDisallowedSyncIP 已由 internal/egress 统一实现
// （B2）：形状校验、端口白名单、地址分类与反 rebinding 都在执行器内。

// ---------------------------------------------------------------------------
// 审计 / 幂等
// ---------------------------------------------------------------------------

// writeAudit 写一条目录变更审计。detail 只含 id/名称/影响计数，不含整行 JSON
// （任务书 §1）。审计失败不影响业务结果（日志库独立、无跨库事务）。
func (h *ModelCatalogHandlers) writeAudit(r *http.Request, action string, detail map[string]any) {
	actor := int64(0)
	if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
		actor = p.UserID
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action: action, Actor: actor, Result: "success",
		Method: r.Method, Route: r.Pattern, StatusCode: http.StatusOK,
		IP: clientIP(r), UserAgent: r.UserAgent(), Detail: raw,
	})
}

// idempotentCreate 提供 24h 幂等重放（与定价/用户端点同构）：同 key + 同
// body 返回首次响应；同 key + 不同 body → 422。scope 隔离资源，键里带操作者
// ID，避免跨管理员共享幂等空间。无 Idempotency-Key 时直接执行。
func (h *ModelCatalogHandlers) idempotentCreate(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil || p.UserID == 0 {
			writeCatalogProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", nil)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > adminIdempotencyKeyHdrMax {
			writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"Idempotency-Key is too long", nil)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
		if err != nil {
			writeCatalogProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"unable to read request body", nil)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		fingerprint := tokenBodyFingerprint(body)

		ctx := r.Context()
		cacheKey := fmt.Sprintf("newtg:idem:catalog:%s:create:%d:%s", scope, p.UserID, key)
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
					writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
						"request cancelled before the idempotent result was ready", nil)
					return
				case <-time.After(25 * time.Millisecond):
				}
				if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
					replayAdminIdempotent(w, cached, fingerprint)
					return
				}
			}
			writeCatalogProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
				"a request with the same Idempotency-Key is still in progress; retry shortly", nil)
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

func (h *ModelCatalogHandlers) loadIdempotent(ctx context.Context, key string) (adminCachedHTTPResponse, bool) {
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

func (h *ModelCatalogHandlers) saveIdempotent(ctx context.Context, key string, cached adminCachedHTTPResponse) {
	raw, err := json.Marshal(cached)
	if err != nil {
		return
	}
	_ = h.rdb.Set(ctx, key, raw, adminIdempotencyTTL).Err()
}
