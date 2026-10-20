// 任务插件管理端点（modules-08 §3.10 / relay-06 §2.4–§2.6）。
//
//	GET    /api/plugin/task                        admin
//	GET    /api/plugin/task/{key}                  admin
//	GET    /api/plugin/task/{key}/versions         admin
//	GET    /api/plugin/task/{key}/source           admin
//	POST   /api/plugin/task                        settings:manage（上传）
//	POST   /api/plugin/task/{key}/activate         settings:manage
//	POST   /api/plugin/task/{key}/status           settings:manage
//	DELETE /api/plugin/task/{key}/versions/{version} settings:manage
//	POST   /api/plugin/task/{key}/dryrun           settings:manage
//	GET    /api/plugin/task/runtime/status         admin
//
// 权限口径（R-M6D-02）：读 admin、写 settings:manage。读侧复用
// requireAdminRole（只要求 role=admin），写侧走 RBAC 三层判定并登记字段白名单。
// 上传是唯一例外：请求体可能是 multipart，RBAC 的字段层只解析 JSON 对象，
// 因此上传在 handler 内用 CheckPermission 做功能级判定（与渠道端点同构），
// 字段解析交给本文件的专用读取器。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/plugin/market"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// TaskPluginStore 是管理端点的存储接缝。生产实现是 *store.TaskPluginRepo；
// 单测注入内存 fake，覆盖权限/筛选/审计等不依赖真 PG 的语义。
type TaskPluginStore interface {
	List(ctx context.Context) ([]store.TaskPlugin, error)
	Get(ctx context.Context, key string) (*store.TaskPlugin, error)
	ListVersions(ctx context.Context, key string) ([]store.TaskPluginVersion, error)
	GetVersion(ctx context.Context, key, version string) (*store.TaskPluginVersion, error)
	Upload(ctx context.Context, in store.TaskPluginUpload) (*store.TaskPlugin, *store.TaskPluginVersion, bool, error)
	Activate(ctx context.Context, key, version string, expect *time.Time) (*store.TaskPlugin, *store.TaskPluginVersion, error)
	SetEnabled(ctx context.Context, key string, enabled bool, expect *time.Time) (*store.TaskPlugin, error)
	DeleteVersion(ctx context.Context, key, version string, expect *time.Time) (*store.TaskPlugin, string, error)
	ListMarketSources(ctx context.Context) ([]store.PluginMarketplaceSource, error)
	GetMarketSource(ctx context.Context, id string) (*store.PluginMarketplaceSource, error)
	ReplaceMarketSources(ctx context.Context, inputs []store.MarketSourceInput) ([]store.PluginMarketplaceSource, error)
	UpdateMarketSourceCheck(ctx context.Context, id, status, lastError string, at time.Time) (*store.PluginMarketplaceSource, error)
	UpsertBuiltinPlugins(ctx context.Context, plugins []store.BuiltinPlugin) error
}

// 编译期断言：仓储漂移在编译期暴露。
var _ TaskPluginStore = (*store.TaskPluginRepo)(nil)

// 冻结的 error code / RFC 7807 type（沿用 https://newtg.dev/problems/<slug>）。
const (
	pluginProblemInvalidRequest    = "https://newtg.dev/problems/plugin_invalid_request"
	pluginProblemNotFound          = "https://newtg.dev/problems/plugin_not_found"
	pluginProblemVersionNotFound   = "https://newtg.dev/problems/plugin_version_not_found"
	pluginProblemCompileFailed     = "https://newtg.dev/problems/plugin_compile_failed"
	pluginProblemVersionConflict   = "https://newtg.dev/problems/plugin_version_conflict"
	pluginProblemVersionRequired   = "https://newtg.dev/problems/plugin_version_required"
	pluginProblemUpdatedAtConflict = "https://newtg.dev/problems/plugin_updated_at_conflict"
	pluginProblemSourceTooLarge    = "https://newtg.dev/problems/plugin_source_too_large"
	pluginProblemDryRunFailed      = "https://newtg.dev/problems/plugin_dryrun_failed"
	pluginProblemOutbound          = "https://newtg.dev/problems/dryrun_outbound_unavailable"
)

var (
	// 读端点同样要求 settings:manage（裁决 R-M6D-16，取代 R-M6D-02 的
	// 「读 admin」）：受限 admin 不应能读插件源码/运行时诊断。与 OpenAPI
	// 声明一致，避免「文档更严、实现更宽」的授权面漂移。
	pluginListAction       = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "list"}
	pluginDetailAction     = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "read"}
	pluginVersionsAction   = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "versions"}
	pluginSourceAction     = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "source"}
	pluginRuntimeAction    = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "runtime_status"}
	pluginMarketReadAction = rbac.AuthzAction{ResourceKind: "plugin_marketplace", ActionName: "read"}

	pluginUploadAction   = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "upload"}
	pluginActivateAction = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "activate"}
	pluginStatusAction   = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "status"}
	pluginDeleteAction   = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "version_delete"}
	pluginDryRunAction   = rbac.AuthzAction{ResourceKind: "task_plugins", ActionName: "dryrun"}
	// 绑定选项属于渠道配置面（模块 06）：权限复用 channels:read
	// （R-M6D-02），不新增权限键，也不要求 settings:manage。
	pluginOptionsAction = rbac.AuthzAction{ResourceKind: "channels", ActionName: "task_plugin_options"}
)

// ChannelBindingCounter 是「插件被多少启用渠道绑定」的读取面
// （生产实现 *store.ChannelRepo）。nil 时计数恒为 0（纯内存装配/测试），
// 但生产装配必须注入——0 会被前端当成「无受影响渠道」。
type ChannelBindingCounter interface {
	CountBoundChannels(ctx context.Context, keys []string) (map[string]int, error)
}

const (
	// maxAuthBodyBytes（1 MiB）是管理面 JSON 请求体的通用上限；上传体积
	// 单独由 `plugin.upload.max_bytes`（默认 2 MiB）+ MaxBytesReader 约束，
	// 绝不复用这里的 1 MiB（brief 明确要求专用读取器）。
	maxPluginUploadDefault = 2 << 20
	maxPluginUploadLimit   = 64 << 20
	// multipartOverhead 覆盖 boundary/头字段/分隔符；文件正文另有精确上限。
	multipartOverhead = 64 << 10
	// JSON 字符串最坏情况下每个字节可膨胀为 \uXXXX（6 字节）。读取器按
	// 6× 上限 + 4 KiB 设 MaxBytesReader，再以**解码后的字节数**做业务判定，
	// 这样既能收住内存，又不会把合法但转义较多的源误判为超限。
	jsonEscapeFactor = 6
)

// TaskPluginHandlerConfig 是插件管理端点装配输入。
type TaskPluginHandlerConfig struct {
	Store   TaskPluginStore
	Manager *plugin.Manager
	Config  ConfigReader
	Authz   *rbac.AuthzMiddleware
	// Access 是上传端点的权限回源（settings:manage）。nil 时上传对未在
	// Principal 携带权限的会话失败关闭。
	Access UserAccessStore
	Audit  *audit.Logger
	Logger *slog.Logger
	Idem   IdempotencyStore
	Market *market.Client
	Now    func() time.Time
	// Bindings 是绑定渠道计数（M6d Task 7a）。nil 时 bound_channels=0。
	Bindings ChannelBindingCounter
}

// TaskPluginHandlers 持有插件管理端点依赖，可并发使用。
type TaskPluginHandlers struct {
	store    TaskPluginStore
	manager  *plugin.Manager
	config   ConfigReader
	authz    *rbac.AuthzMiddleware
	access   UserAccessStore
	audit    *audit.Logger
	log      *slog.Logger
	idem     IdempotencyStore
	market   *market.Client
	now      func() time.Time
	bindings ChannelBindingCounter

	// marketMu 保护 5 分钟索引缓存；刷新失败不覆写旧缓存（见 handlers_plugin_marketplace.go）。
	marketMu       sync.Mutex
	marketCache    *marketplaceIndexView
	marketCachedAt time.Time
}

// NewTaskPluginHandlers 构造插件管理端点。
func NewTaskPluginHandlers(cfg TaskPluginHandlerConfig) (*TaskPluginHandlers, error) {
	if cfg.Store == nil {
		return nil, errors.New("httpapi: TaskPluginHandlerConfig.Store is required")
	}
	manager := cfg.Manager
	if manager == nil {
		// 没有装配运行时也要让管理面可读（列表仍可展示 DB 行）；空注册表
		// 的所有 key 状态为 not_registered，而不是让端点 500。
		created, err := plugin.NewManager(nil, nil, plugin.Options{MasterEnabled: true})
		if err != nil {
			return nil, err
		}
		manager = created
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	// 写侧字段白名单（BOPLA）：未登记字段 403，不静默丢弃。
	authz.SetWritePolicy(pluginActivateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"version": nil, "expect_updated_at": nil,
	}})
	authz.SetWritePolicy(pluginStatusAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"enabled": nil, "expect_updated_at": nil,
	}})
	authz.SetWritePolicy(pluginDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"expect_updated_at": nil,
	}})
	authz.SetWritePolicy(pluginDryRunAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"version": nil, "endpoint": nil, "payload": nil, "allow_outbound": nil,
	}})
	registerMarketWritePolicies(authz)
	idem := cfg.Idem
	if idem == nil {
		idem = NewMemoryIdempotencyStore()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	client := cfg.Market
	if client == nil {
		// 市场源是管理员可填 URL + 服务端出站：生产默认客户端必须从
		// sec.ssrf.* 读取策略（B2 / security-04 §2.4）。
		client = market.NewClientWithConfig(cfg.Config)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &TaskPluginHandlers{
		store: cfg.Store, manager: manager, config: cfg.Config, authz: authz,
		access: cfg.Access, audit: cfg.Audit, log: log,
		idem: idem, market: client, now: now, bindings: cfg.Bindings,
	}, nil
}

// Register 挂载插件管理端点。静态子路径先于 {key} 注册；Go 1.22 ServeMux
// 按「字面量优先」择路，显式顺序仅用于让契约在阅读时成立。
func (h *TaskPluginHandlers) Register(mux *http.ServeMux) {
	read := func(action rbac.AuthzAction, fn http.HandlerFunc) http.Handler {
		return h.authz.Handler(rbac.PermSettingsManage, action)(fn)
	}
	write := func(action rbac.AuthzAction, fn http.HandlerFunc) http.Handler {
		return h.authz.Handler(rbac.PermSettingsManage, action)(fn)
	}

	// 读端点。
	mux.Handle("GET /api/plugin/task", read(pluginListAction, h.handleList))
	mux.Handle("GET /api/plugin/task/runtime/status", read(pluginRuntimeAction, h.handleRuntimeStatus))
	mux.Handle("GET /api/plugin/task/marketplace/sources", read(pluginMarketReadAction, h.handleMarketSourcesList))
	mux.Handle("GET /api/plugin/task/marketplace", read(pluginMarketReadAction, h.handleMarketplace))
	mux.Handle("GET /api/plugin/task/{key}", read(pluginDetailAction, h.handleGet))
	mux.Handle("GET /api/plugin/task/{key}/versions", read(pluginVersionsAction, h.handleVersions))
	mux.Handle("GET /api/plugin/task/{key}/source", read(pluginSourceAction, h.handleSource))
	// 绑定选项（M6d Task 7a / relay-06 §2.6）：渠道配置面读取，权限
	// channels:read（R-M6D-02），客户端必须持有该键而不是 settings:manage。
	mux.Handle("GET /api/task_plugin_options",
		h.authz.Handler(rbac.PermChannelsRead, pluginOptionsAction)(http.HandlerFunc(h.handlePluginOptions)))

	// 写端点。
	// multipart 不能走 authz.Handler（字段层只解析 JSON），上传单独授权。
	mux.Handle("POST /api/plugin/task", http.HandlerFunc(h.handleUpload))
	mux.Handle("POST /api/plugin/task/{key}/activate", write(pluginActivateAction, h.handleActivate))
	mux.Handle("POST /api/plugin/task/{key}/status", write(pluginStatusAction, h.handleStatus))
	mux.Handle("DELETE /api/plugin/task/{key}/versions/{version}", write(pluginDeleteAction, h.handleDeleteVersion))
	mux.Handle("POST /api/plugin/task/{key}/dryrun", write(pluginDryRunAction, h.handleDryRun))

	// 市场写端点。
	mux.Handle("PUT /api/plugin/task/marketplace/sources",
		write(pluginMarketSourcesAction, h.handleMarketSourcesPut))
	mux.Handle("POST /api/plugin/task/marketplace/sources/{id}/check",
		write(pluginMarketCheckAction, h.handleMarketSourceCheck))
	mux.Handle("POST /api/plugin/task/marketplace/install",
		write(pluginMarketInstallAction, h.handleMarketInstall))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// taskPluginView 是列表/详情共用的白名单投影。
//
// 刻意不含任何密钥字段（S5）；source_text 只在详情 include_source=1 时
// 由 detailView 追加，列表永不携带源码正文。
type taskPluginView struct {
	Key           string              `json:"key"`
	Name          string              `json:"name"`
	Icon          *string             `json:"icon"`
	Source        string              `json:"source"`
	Builtin       bool                `json:"builtin"`
	Enabled       bool                `json:"enabled"`
	ActiveVersion *string             `json:"active_version"`
	Description   *string             `json:"description"`
	RuntimeStatus plugin.PluginStatus `json:"runtime_status"`
	// Version 是运行时快照的生效版本（factory 层来自编译产物，override 层
	// 来自 active 行）；内置插件没有 B-12 active 行，UI 的「当前版本」取它。
	Version       string        `json:"version,omitempty"`
	Layer         plugin.Layer  `json:"layer,omitempty"`
	CompileError  string        `json:"compile_error,omitempty"`
	RuntimeErrors plugin.Issues `json:"runtime_errors,omitempty"`
	// BoundChannels 是「启用且绑定该 key」的渠道数（M6d Task 7a 实时查询：
	// 单绑 task_plugin_key 或多绑 task_extend_plugin_keys 命中均计一次）。
	// 关系不落库，每次读取按 channels 当前状态计算。
	BoundChannels int       `json:"bound_channels"`
	UpdatedAt     time.Time `json:"updated_at"`
	// SourceText 只出现在 include_source=1 的详情响应。
	SourceText string `json:"source_text,omitempty"`
}

type taskPluginVersionView struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Version    string    `json:"version"`
	APIVersion string    `json:"api_version"`
	SourceHash string    `json:"source_hash"`
	Icon       *string   `json:"icon"`
	Enabled    bool      `json:"enabled"`
	Active     bool      `json:"active"`
	UploadedBy int64     `json:"uploaded_by"`
	CreatedAt  time.Time `json:"created_at"`
}

func versionView(v store.TaskPluginVersion) taskPluginVersionView {
	return taskPluginVersionView{
		ID: v.ID, Key: v.Key, Version: v.Version, APIVersion: v.APIVersion,
		SourceHash: v.SourceHash, Icon: v.Icon, Enabled: v.Enabled,
		Active: v.Active, UploadedBy: v.UploadedBy, CreatedAt: v.CreatedAt,
	}
}

// bindingCounts 查询绑定渠道数；没有注入计数依赖时返回全 0（不省略字段）。
// 查询失败向上返回：管理面把 0 当成「无受影响渠道」是危险的错误信息，
// 宁可 500 也不要伪造计数（停用预检依赖该值）。
func (h *TaskPluginHandlers) bindingCounts(ctx context.Context, keys []string) (map[string]int, error) {
	counts := make(map[string]int, len(keys))
	for _, key := range keys {
		counts[key] = 0
	}
	if h.bindings == nil || len(keys) == 0 {
		return counts, nil
	}
	return h.bindings.CountBoundChannels(ctx, keys)
}

// pluginView 把 DB 行与运行时条目合成管理面投影。bound 是该 key 的绑定渠道数
// （由调用方批量/单次查询后传入；无绑定计数依赖时为 0）。
func (h *TaskPluginHandlers) pluginView(p store.TaskPlugin, bound int) taskPluginView {
	entry := h.manager.Snapshot().Lookup(p.Key)
	view := taskPluginView{
		Key: p.Key, Name: p.Name, Icon: p.Icon, Source: p.Source, Builtin: p.Builtin,
		Enabled: p.Enabled, ActiveVersion: p.ActiveVersion, Description: p.Description,
		RuntimeStatus: entry.Status, Version: entry.Version, Layer: entry.Layer, BoundChannels: bound,
		UpdatedAt: p.UpdatedAt,
	}
	if view.ActiveVersion == nil && p.Builtin && entry.Version != "" {
		// 内置插件没有 B-12 active 行，B-11 镜像为 NULL；管理面的「当前版本」
		// 由运行时快照补充（只影响响应投影，不回写 DB）。
		version := entry.Version
		view.ActiveVersion = &version
	}
	if len(entry.Errors) > 0 {
		view.RuntimeErrors = append(plugin.Issues(nil), entry.Errors...)
		view.CompileError = entry.Errors[0].Message
	}
	return view
}

// ---------------------------------------------------------------------------
// 列表 / 详情 / 版本 / 源查看器
// ---------------------------------------------------------------------------

// runtimeSeverity 实现「运行状态异常优先，其次名称」的默认排序。
func runtimeSeverity(status plugin.PluginStatus) int {
	switch status {
	case plugin.StatusCompileFailed:
		return 0
	case plugin.StatusNotRegistered:
		return 1
	case plugin.StatusDisabledFallback:
		return 2
	case plugin.StatusDisabled:
		return 3
	default:
		return 4
	}
}

func (h *TaskPluginHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	source := strings.TrimSpace(query.Get("source"))
	enabledFilter := strings.TrimSpace(query.Get("enabled"))
	runtimeFilter := strings.TrimSpace(query.Get("runtime_status"))

	plugins, err := h.store.List(r.Context())
	if err != nil {
		h.writeStoreError(w, "list task plugins", err)
		return
	}
	pluginKeys := make([]string, 0, len(plugins))
	for _, p := range plugins {
		pluginKeys = append(pluginKeys, p.Key)
	}
	counts, err := h.bindingCounts(r.Context(), pluginKeys)
	if err != nil {
		h.writeStoreError(w, "count bound channels", err)
		return
	}
	views := make([]taskPluginView, 0, len(plugins))
	for _, p := range plugins {
		if source != "" && p.Source != source {
			continue
		}
		if enabledFilter != "" {
			want, parseErr := strconv.ParseBool(enabledFilter)
			if parseErr != nil {
				writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
					"Invalid request", "enabled must be true or false", nil)
				return
			}
			if p.Enabled != want {
				continue
			}
		}
		view := h.pluginView(p, counts[p.Key])
		if runtimeFilter != "" && string(view.RuntimeStatus) != runtimeFilter {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(p.Key), search) &&
			!strings.Contains(strings.ToLower(p.Name), search) {
			continue
		}
		views = append(views, view)
	}

	sort.SliceStable(views, func(i, j int) bool {
		si, sj := runtimeSeverity(views[i].RuntimeStatus), runtimeSeverity(views[j].RuntimeStatus)
		if si != sj {
			return si < sj
		}
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].Key < views[j].Key
	})

	page, size := pageParams(query)
	total := len(views)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": views[start:end], "total": total, "page": page, "page_size": size,
	})
}

// pageParams 复用全局分页口径（p 1 基，page_size 默认 20、上限 100）。
func pageParams(query map[string][]string) (int, int) {
	page, size := 1, 20
	if raw := firstQuery(query, "p"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 {
			page = parsed
		}
	}
	if raw := firstQuery(query, "page_size"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 {
			size = parsed
		}
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func firstQuery(query map[string][]string, key string) string {
	if values, ok := query[key]; ok && len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

func (h *TaskPluginHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	p, err := h.store.Get(r.Context(), key)
	if err != nil {
		h.writeStoreError(w, "get task plugin", err)
		return
	}
	versions, err := h.store.ListVersions(r.Context(), key)
	if err != nil {
		h.writeStoreError(w, "list task plugin versions", err)
		return
	}
	counts, err := h.bindingCounts(r.Context(), []string{p.Key})
	if err != nil {
		h.writeStoreError(w, "count bound channels", err)
		return
	}
	view := h.pluginView(*p, counts[p.Key])
	viewMap := map[string]any{"plugin": view, "versions": make([]taskPluginVersionView, 0, len(versions))}
	items := make([]taskPluginVersionView, 0, len(versions))
	for _, v := range versions {
		items = append(items, versionView(v))
	}
	viewMap["versions"] = items

	includeSource := strings.TrimSpace(r.URL.Query().Get("include_source"))
	if includeSource == "1" || strings.EqualFold(includeSource, "true") {
		// 源查看器只读：返回 active 版本的源码；没有 active 时回退最新版本，
		// 便于管理员在上传后、激活前核对内容（F-08-29 完整性页签）。
		// 正文不在列表投影里（S5），必须单独调用 GetVersion。
		sourceVersion := ""
		if p.ActiveVersion != nil {
			sourceVersion = *p.ActiveVersion
		} else if len(versions) > 0 {
			sourceVersion = versions[0].Version
		}
		if sourceVersion != "" {
			if full, err := h.store.GetVersion(r.Context(), key, sourceVersion); err == nil {
				viewMap["source_text"] = full.SourceText
				viewMap["source_version"] = full.Version
			}
		}
	}
	writeJSON(w, http.StatusOK, viewMap)
}

func (h *TaskPluginHandlers) handleVersions(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if _, err := h.store.Get(r.Context(), key); err != nil {
		h.writeStoreError(w, "get task plugin", err)
		return
	}
	versions, err := h.store.ListVersions(r.Context(), key)
	if err != nil {
		h.writeStoreError(w, "list task plugin versions", err)
		return
	}
	items := make([]taskPluginVersionView, 0, len(versions))
	for _, v := range versions {
		items = append(items, versionView(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *TaskPluginHandlers) handleSource(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if version == "" {
		p, err := h.store.Get(r.Context(), key)
		if err != nil {
			h.writeStoreError(w, "get task plugin", err)
			return
		}
		if p.ActiveVersion != nil {
			version = *p.ActiveVersion
		}
	}
	if version == "" {
		versions, err := h.store.ListVersions(r.Context(), key)
		if err != nil {
			h.writeStoreError(w, "list task plugin versions", err)
			return
		}
		if len(versions) == 0 {
			writePluginProblem(w, http.StatusNotFound, pluginProblemVersionNotFound,
				"Version not found", "this plugin has no versions", nil)
			return
		}
		version = versions[0].Version
	}
	v, err := h.store.GetVersion(r.Context(), key, version)
	if err != nil {
		h.writeStoreError(w, "get task plugin version", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key": v.Key, "version": v.Version, "source_hash": v.SourceHash,
		"source_text": v.SourceText,
	})
}

// ---------------------------------------------------------------------------
// 上传
// ---------------------------------------------------------------------------

type uploadBody struct {
	Source string
}

// handleUpload 处理 JSON `{source_text}` 与 multipart 单文件两种形态。
//
// 安全顺序（S1）：读取（体积上界）→ 编译/校验 → 事务入库。编译失败直接
// 422 且没有任何写库调用；同 (key, version) 同 hash 幂等返回 200/已有行，
// 异 hash 409。所有响应都不回显源码正文。
func (h *TaskPluginHandlers) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !h.requireSettingsManage(w, r) {
		return
	}
	body, errStatus, errBody := h.readUploadBody(w, r)
	if errStatus != 0 {
		writeJSONBytes(w, errStatus, errBody)
		return
	}
	fingerprint := settingsFingerprint([]byte(body.Source))

	actor := actorID(r)
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "Idempotency-Key header is required for plugin upload", nil)
		return
	}
	key := fmt.Sprintf("plugin-upload:%d:%s %s:%s:%s",
		actor, r.Method, r.URL.RequestURI(), fingerprint, headerKey)

	claim, claimErr := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
	if claimErr != nil {
		// fail-open：幂等存储不可用不阻断上传；数据库的 (key,version) 唯一
		// 约束与 hash 比对仍保证重复上传不会写出第二行/错误内容。
		status, response := h.performUpload(r, actor, body.Source)
		writeJSONBytes(w, status, response)
		return
	}
	switch claim.Status {
	case IdempotencyClaimReplay:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSONBytes(w, claim.Response.Status, claim.Response.Body)
		return
	case IdempotencyClaimInFlight:
		w.Header().Set("Retry-After", "1")
		writeProblemDetail(w, http.StatusConflict, problemTypeIdempotencyInProgress,
			"Idempotency key is in progress",
			"another request with this Idempotency-Key is still being processed; retry shortly")
		return
	}

	status, response := h.performUpload(r, actor, body.Source)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
	defer cancel()
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		if err := h.idem.Complete(ctx, key, claim.Token,
			IdempotentResponse{Status: status, Body: response}, IdempotencyTTL); err != nil {
			h.log.Error("plugin: upload idempotency complete failed", "err", err)
		}
	} else if err := h.idem.Release(ctx, key, claim.Token); err != nil {
		h.log.Error("plugin: upload idempotency release failed", "err", err)
	}
	writeJSONBytes(w, status, response)
}

// readUploadBody 读取两种上传形态并返回源文本。
//
// 体积控制分两层：
//  1. MaxBytesReader 在读取阶段硬上限（multipart = max + 64 KiB 头部开销；
//     JSON = 6×max + 4 KiB 以容纳 \uXXXX 转义膨胀）；
//  2. 解码后的源文本字节数再与 max 比较——第 1 层不是业务上限。
func (h *TaskPluginHandlers) readUploadBody(w http.ResponseWriter, r *http.Request) (uploadBody, int, []byte) {
	maxBytes := h.maxUploadBytes()
	contentType := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(contentType)

	if mediaType == "multipart/form-data" {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes+multipartOverhead))
		if err != nil {
			return uploadBodyProblem(pluginProblemSourceTooLarge,
				"Plugin source is too large", "upload exceeds the configured plugin source limit")
		}
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return uploadBodyProblem(pluginProblemInvalidRequest,
					"Invalid request", "malformed multipart body")
			}
			name := part.FormName()
			filename := part.FileName()
			if filename == "" && name != "file" && name != "source" {
				continue
			}
			source, err := io.ReadAll(io.LimitReader(part, maxBytes+1))
			if err != nil {
				return uploadBodyProblem(pluginProblemSourceTooLarge,
					"Plugin source is too large", "unable to read uploaded file")
			}
			if int64(len(source)) > maxBytes {
				return uploadBodyProblem(pluginProblemSourceTooLarge,
					"Plugin source is too large", "upload exceeds the configured plugin source limit")
			}
			if filename != "" && !strings.HasSuffix(strings.ToLower(filename), ".js") {
				return uploadBodyProblem(pluginProblemInvalidRequest,
					"Invalid request", "plugin source file must use the .js extension")
			}
			return uploadBody{Source: string(source)}, 0, nil
		}
		return uploadBodyProblem(pluginProblemInvalidRequest,
			"Invalid request", "multipart body must contain a plugin source file")
	}

	limit := maxBytes*jsonEscapeFactor + 4096
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return uploadBodyProblem(pluginProblemSourceTooLarge,
			"Plugin source is too large", "upload exceeds the configured plugin source limit")
	}
	var payload struct {
		SourceText string `json:"source_text"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return uploadBodyProblem(pluginProblemInvalidRequest,
			"Invalid request", "body must be JSON {source_text} or multipart/form-data")
	}
	if int64(len(payload.SourceText)) > maxBytes {
		return uploadBodyProblem(pluginProblemSourceTooLarge,
			"Plugin source is too large", "upload exceeds the configured plugin source limit")
	}
	if strings.TrimSpace(payload.SourceText) == "" {
		return uploadBodyProblem(pluginProblemInvalidRequest,
			"Invalid request", "source_text must not be empty")
	}
	return uploadBody{Source: payload.SourceText}, 0, nil
}

func uploadBodyProblem(code, title, detail string) (uploadBody, int, []byte) {
	problem := map[string]any{
		"type": code, "title": title, "status": http.StatusUnprocessableEntity, "detail": detail,
	}
	raw, _ := json.Marshal(problem)
	return uploadBody{}, http.StatusUnprocessableEntity, raw
}

// performUpload 编译后入库，返回 (HTTP 状态, 响应体)。
func (h *TaskPluginHandlers) performUpload(r *http.Request, actor int64, source string) (int, []byte) {
	compiled, err := plugin.Compile(r.Context(), source, plugin.CompileOptions{Layer: plugin.LayerOverride})
	if err != nil {
		// 失败也要留审计（result=failed），但只记可得的 key 与规则名，
		// 不记源码/编译错误正文（错误正文可能包含源片段）。
		h.writePluginAudit(r, audit.ActionPluginUpload, http.StatusUnprocessableEntity, map[string]any{
			"plugin_key": issueKey(err), "result_reason": "compile_failed",
		})
		return pluginIssuesResponse(http.StatusUnprocessableEntity, pluginProblemCompileFailed,
			"Plugin compile failed", "the plugin source failed compilation or validation", err)
	}
	manifest := compiled.Manifest()
	if source == "" || manifest == nil {
		return pluginIssuesResponse(http.StatusUnprocessableEntity, pluginProblemCompileFailed,
			"Plugin compile failed", "the plugin source produced no manifest", nil)
	}

	icon := stringPtr(manifest.Icon)
	description := stringPtr(chooseDescription(manifest.Description))
	version := store.TaskPluginUpload{
		Key: manifest.Key, Name: manifest.Name, Version: manifest.Version,
		APIVersion: "v1", SourceText: source, SourceHash: compiled.SourceHash(),
		Icon: icon, Description: description, UploadedBy: actor,
		// 覆盖内置 key 时主表行仍必须标记 builtin/source=builtin（内置事实
		// 不由上传改写；版本行才是 upload 层）。
		Builtin: h.manager.HasFactoryKey(manifest.Key),
	}
	p, v, created, err := h.store.Upload(r.Context(), version)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTaskPluginVersionConflict):
			h.writePluginAudit(r, audit.ActionPluginUpload, http.StatusConflict, map[string]any{
				"plugin_key": manifest.Key, "version": manifest.Version, "result_reason": "version_conflict",
			})
			return pluginProblemResponse(http.StatusConflict, pluginProblemVersionConflict,
				"Plugin version already exists",
				"the same plugin key/version is already stored with different source content")
		default:
			h.log.Error("plugin: upload persist failed", "err", err)
			h.writePluginAudit(r, audit.ActionPluginUpload, http.StatusInternalServerError, map[string]any{
				"plugin_key": manifest.Key, "version": manifest.Version, "result_reason": "store_error",
			})
			return pluginProblemResponse(http.StatusInternalServerError, authProblemInternal,
				"Internal Server Error", "")
		}
	}
	h.reloadAsync(r.Context())
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	h.auditUpload(r, actor, p.Key, v.Version, v.SourceHash, created, status)
	counts, err := h.bindingCounts(r.Context(), []string{p.Key})
	if err != nil {
		return pluginProblemResponse(http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
	}
	body, _ := json.Marshal(map[string]any{
		"created": created, "plugin": h.pluginView(*p, counts[p.Key]), "version": versionView(*v),
	})
	return status, body
}

func (h *TaskPluginHandlers) maxUploadBytes() int64 {
	limit := int64(maxPluginUploadDefault)
	if h.config != nil {
		if raw, ok := h.config.Get("plugin.upload.max_bytes"); ok {
			var parsed int64
			if err := json.Unmarshal(raw, &parsed); err == nil && parsed > 0 {
				limit = parsed
			}
		}
	}
	if limit > maxPluginUploadLimit {
		limit = maxPluginUploadLimit
	}
	return limit
}

// chooseDescription 从 localised description 中取一个稳定值。
// 优先 en（manifest 要求 en 必需），其次按 locale 升序，保证同源重传结果一致。
func chooseDescription(description map[string]string) string {
	if value := strings.TrimSpace(description["en"]); value != "" {
		return value
	}
	locales := make([]string, 0, len(description))
	for locale := range description {
		locales = append(locales, locale)
	}
	sort.Strings(locales)
	for _, locale := range locales {
		if value := strings.TrimSpace(description[locale]); value != "" {
			return value
		}
	}
	return ""
}

func stringPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

// requireSettingsManage 是上传端点的功能级门（multipart 无法走 RBAC 字段层）。
func (h *TaskPluginHandlers) requireSettingsManage(w http.ResponseWriter, r *http.Request) bool {
	err := CheckPermission(r.Context(), h.access, rbac.PermSettingsManage)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		writeUnauthorized(w)
	case errors.Is(err, ErrAccessStore):
		writePluginProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "", nil)
	default:
		writeForbidden(w)
	}
	return false
}

// ---------------------------------------------------------------------------
// 激活 / 启停 / 删除 / dryrun
// ---------------------------------------------------------------------------

func (h *TaskPluginHandlers) handleActivate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version         string     `json:"version"`
		ExpectUpdatedAt *time.Time `json:"expect_updated_at"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Version) == "" {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "version is required", nil)
		return
	}
	key := r.PathValue("key")
	p, v, err := h.store.Activate(r.Context(), key, req.Version, req.ExpectUpdatedAt)
	if err != nil {
		h.auditWriteFailure(r, audit.ActionPluginActivate, key, err)
		h.writeStoreError(w, "activate plugin version", err)
		return
	}
	h.reloadAsync(r.Context())
	h.writePluginAudit(r, audit.ActionPluginActivate, http.StatusOK, map[string]any{
		"plugin_key": v.Key, "version": v.Version, "source_hash_prefix": hashPrefix(v.SourceHash),
	})
	counts, err := h.bindingCounts(r.Context(), []string{p.Key})
	if err != nil {
		h.writeStoreError(w, "count bound channels", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plugin": h.pluginView(*p, counts[p.Key]), "version": versionView(*v),
	})
}

func (h *TaskPluginHandlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled         *bool      `json:"enabled"`
		ExpectUpdatedAt *time.Time `json:"expect_updated_at"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "enabled is required", nil)
		return
	}
	key := r.PathValue("key")
	p, err := h.store.SetEnabled(r.Context(), key, *req.Enabled, req.ExpectUpdatedAt)
	if err != nil {
		action := audit.ActionPluginDisable
		if *req.Enabled {
			action = audit.ActionPluginEnable
		}
		h.auditWriteFailure(r, action, key, err)
		h.writeStoreError(w, "set plugin status", err)
		return
	}
	h.reloadAsync(r.Context())
	action := audit.ActionPluginDisable
	if *req.Enabled {
		action = audit.ActionPluginEnable
	}
	// bound_channels 是真实绑定计数（M6d Task 7a）：停用预检据此提示
	// 「受影响渠道数」，即使插件已停用，绑定关系本身仍存在，计数照常返回。
	counts, err := h.bindingCounts(r.Context(), []string{p.Key})
	if err != nil {
		h.writeStoreError(w, "count bound channels", err)
		return
	}
	bound := counts[p.Key]
	h.writePluginAudit(r, action, http.StatusOK, map[string]any{
		"plugin_key": p.Key, "enabled": p.Enabled, "bound_channels": bound,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"key": p.Key, "enabled": p.Enabled, "bound_channels": bound, "updated_at": p.UpdatedAt,
	})
}

// handlePluginOptions 返回「可绑定插件」选项（relay-06 §2.6 / F-08-28）。
//
// 只列注册表中当前可执行的插件（entry.Plugin != nil：registered 与
// disabled_fallback 有可执行层，not_registered/compile_failed/disabled 无）；
// 按 key 升序。channel_count 只统计启用渠道（与 bound_channels 同口径），
// 供渠道表单提示「这个插件已被多少渠道使用」。
func (h *TaskPluginHandlers) handlePluginOptions(w http.ResponseWriter, r *http.Request) {
	snapshot := h.manager.Snapshot()
	rows, err := h.store.List(r.Context())
	if err != nil {
		h.writeStoreError(w, "list task plugins", err)
		return
	}
	names := make(map[string]string, len(rows))
	for _, row := range rows {
		names[row.Key] = row.Name
	}
	keys := make([]string, 0, len(snapshot.Keys()))
	for _, key := range snapshot.Keys() {
		if entry := snapshot.Lookup(key); entry.Plugin != nil {
			keys = append(keys, key)
		}
	}
	counts, err := h.bindingCounts(r.Context(), keys)
	if err != nil {
		h.writeStoreError(w, "count bound channels", err)
		return
	}
	items := make([]taskPluginOptionView, 0, len(keys))
	for _, key := range keys {
		name := names[key]
		if name == "" {
			if entry := snapshot.Lookup(key); entry.Plugin != nil {
				name = entry.Plugin.Manifest().Name
			}
		}
		if name == "" {
			name = key
		}
		items = append(items, taskPluginOptionView{
			Key: key, Name: name, ChannelCount: counts[key],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// taskPluginOptionView 是绑定选项的白名单投影（无源码、无密钥）。
type taskPluginOptionView struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	ChannelCount int    `json:"channel_count"`
}

func (h *TaskPluginHandlers) handleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectUpdatedAt *time.Time `json:"expect_updated_at"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "unable to read request body", nil)
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
				"Invalid request", "invalid JSON body", nil)
			return
		}
	}
	key, version := r.PathValue("key"), r.PathValue("version")
	p, promoted, err := h.store.DeleteVersion(r.Context(), key, version, req.ExpectUpdatedAt)
	if err != nil {
		h.auditWriteFailure(r, audit.ActionPluginVersionDelete, key, err)
		h.writeStoreError(w, "delete plugin version", err)
		return
	}
	h.reloadAsync(r.Context())
	h.writePluginAudit(r, audit.ActionPluginVersionDelete, http.StatusOK, map[string]any{
		"plugin_key": key, "version": version, "promoted_version": promoted,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"key": p.Key, "deleted_version": version, "promoted_version": promoted,
		"active_version": p.ActiveVersion, "updated_at": p.UpdatedAt,
	})
}

func (h *TaskPluginHandlers) handleDryRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version       string          `json:"version"`
		Endpoint      string          `json:"endpoint"`
		Payload       json.RawMessage `json:"payload"`
		AllowOutbound bool            `json:"allow_outbound"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var payload any
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
				"Invalid request", "payload must be valid JSON", nil)
			return
		}
	}
	result, err := h.manager.DryRun(r.Context(), plugin.DryRunRequest{
		Key: r.PathValue("key"), Version: req.Version, Endpoint: req.Endpoint,
		Payload: payload, AllowOutbound: req.AllowOutbound,
	})
	if err != nil {
		var issues plugin.Issues
		if errors.As(err, &issues) {
			h.writePluginAudit(r, audit.ActionPluginDryRun, http.StatusUnprocessableEntity, map[string]any{
				"plugin_key": r.PathValue("key"), "version": req.Version, "endpoint": req.Endpoint,
				"result_reason": "dryrun_failed",
			})
			for _, issue := range issues {
				if issue.Rule == "dryrun_outbound_unavailable" {
					writePluginIssues(w, http.StatusUnprocessableEntity, pluginProblemOutbound,
						"Outbound is unavailable in dryrun", "dryrun does not support allow_outbound=true", issues)
					return
				}
			}
			writePluginIssues(w, http.StatusUnprocessableEntity, pluginProblemDryRunFailed,
				"Dry run failed", "the plugin hook could not be executed", issues)
			return
		}
		h.log.Warn("plugin: dryrun failed", "err", err)
		h.writePluginAudit(r, audit.ActionPluginDryRun, http.StatusInternalServerError, map[string]any{
			"plugin_key": r.PathValue("key"), "version": req.Version, "endpoint": req.Endpoint,
			"result_reason": "internal",
		})
		writePluginProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "", nil)
		return
	}
	// 审计只记 key/version/endpoint/结果摘要，不上送样例请求体或返回值。
	h.writePluginAudit(r, audit.ActionPluginDryRun, http.StatusOK, map[string]any{
		"plugin_key": result.Key, "version": result.Version, "endpoint": result.Endpoint,
		"duration_ms": result.DurationMS,
	})
	writeJSON(w, http.StatusOK, result)
}

// handleRuntimeStatus 返回单进程 Manager 快照（relay-06 §2.5）。
//
// generation 是节点本地号；跨节点比较必须用 revision。逐副本聚合需要实例
// 心跳表（F-08-21），当前返回单节点对象并明确 node 字段；已登记。
func (h *TaskPluginHandlers) handleRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.manager.Status())
}

// ---------------------------------------------------------------------------
// 审计 / 错误 / 通用
// ---------------------------------------------------------------------------

func (h *TaskPluginHandlers) auditUpload(r *http.Request, actor int64, key, version, hash string, created bool, status int) {
	h.writePluginAudit(r, audit.ActionPluginUpload, status, map[string]any{
		"plugin_key": key, "version": version, "source_hash_prefix": hashPrefix(hash), "created": created,
	})
}

// writePluginAudit 写插件域审计。detail 由调用方只填非秘密字段；本函数
// 统一补 result/route，并保证 source_text 与市场包体不会出现在 before/after
// （调用方根本不传它们）。
func (h *TaskPluginHandlers) writePluginAudit(r *http.Request, action string, status int, detail map[string]any) {
	if h.audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	result := "success"
	if status >= http.StatusBadRequest {
		result = "failed"
	}
	detail["result"] = result
	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action: action, Actor: actorID(r), Result: result,
		Method: r.Method, Route: r.Pattern, StatusCode: status,
		IP: clientIP(r), UserAgent: r.UserAgent(), Detail: raw,
	})
}

// auditWriteFailure 记录失败写路径（409/乐观锁/404）的审计。
// detail 只有 key 与可分类的原因，不含请求体。
func (h *TaskPluginHandlers) auditWriteFailure(r *http.Request, action, key string, err error) {
	status, reason := pluginStoreErrorAudit(err)
	h.writePluginAudit(r, action, status, map[string]any{
		"plugin_key": key, "result_reason": reason,
	})
}

// pluginStoreErrorAudit 把仓储错误映射为与 HTTP 响应一致的状态码/原因。
func pluginStoreErrorAudit(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrTaskPluginNotFound):
		return http.StatusNotFound, "plugin_not_found"
	case errors.Is(err, store.ErrTaskPluginVersionNotFound):
		return http.StatusNotFound, "version_not_found"
	case errors.Is(err, store.ErrTaskPluginVersionConflict):
		return http.StatusConflict, "version_conflict"
	case errors.Is(err, store.ErrTaskPluginVersionRequired):
		return http.StatusConflict, "version_required"
	case errors.Is(err, store.ErrTaskPluginStale):
		return http.StatusConflict, "updated_at_conflict"
	default:
		return http.StatusInternalServerError, "store_error"
	}
}

// hashPrefix 返回 source hash 前 8 位（brief 允许的审计脱敏形态）。
func hashPrefix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

func (h *TaskPluginHandlers) writeStoreError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, store.ErrTaskPluginNotFound):
		writePluginProblem(w, http.StatusNotFound, pluginProblemNotFound,
			"Plugin not found", "the requested plugin does not exist", nil)
	case errors.Is(err, store.ErrTaskPluginVersionNotFound):
		writePluginProblem(w, http.StatusNotFound, pluginProblemVersionNotFound,
			"Version not found", "the requested plugin version does not exist", nil)
	case errors.Is(err, store.ErrTaskPluginVersionConflict):
		writePluginProblem(w, http.StatusConflict, pluginProblemVersionConflict,
			"Plugin version already exists",
			"the same plugin key/version is already stored with different source content", nil)
	case errors.Is(err, store.ErrTaskPluginVersionRequired):
		writePluginProblem(w, http.StatusConflict, pluginProblemVersionRequired,
			"A version must remain active",
			"deleting this version would leave the plugin without an active version; upload a new version first", nil)
	case errors.Is(err, store.ErrTaskPluginStale):
		writePluginProblem(w, http.StatusConflict, pluginProblemUpdatedAtConflict,
			"Plugin was modified",
			"expect_updated_at does not match the current row; reload and retry", nil)
	case errors.Is(err, store.ErrMarketSourceNotFound):
		h.writeMarketError(w, err)
	default:
		h.log.Error("plugin: "+operation+" failed", "err", err)
		writePluginProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "", nil)
	}
}

// reloadAsync 在写成功后重建本副本快照。
//
// 失败不反向影响写结果：DB 已是权威真源，旧快照仍可服务（fail-closed 的
// 「保留上一份可用快照」语义），错误会出现在 runtime/status 的 reload_error。
// 30s 轮询/pub-sub 接线属后续装配项（T3 遗留），已登记。
func (h *TaskPluginHandlers) reloadAsync(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), plugin.DefaultTimeout)
	defer cancel()
	// 内置插件的数据面开关由「设置禁用集 + B-11 enabled=false」共同决定：
	// /status 停用内置 key 必须让它从 factory 层消失（否则管理面显示禁用、
	// 运行时仍注册）。每次写后重算并交给 Manager，随后的 Reload 原子生效。
	disabled := readConfigStringList(h.config, "plugin.disabled_factory_keys")
	if plugins, err := h.store.List(ctx); err == nil {
		for _, p := range plugins {
			if p.Builtin && !p.Enabled {
				disabled = append(disabled, p.Key)
			}
		}
	}
	h.manager.SetDisabledFactoryKeys(disabled)
	if err := h.manager.Reload(ctx); err != nil {
		h.log.Warn("plugin: reload after write failed", "err", err)
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "unable to read request body", nil)
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "JSON body is required", nil)
		return false
	}
	if err := json.Unmarshal(body, target); err != nil {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "invalid JSON body", nil)
		return false
	}
	return true
}

// writePluginProblem 写 RFC 7807 problem（可选附加字段）。
func writePluginProblem(w http.ResponseWriter, status int, problemType, title, detail string, extra map[string]any) {
	problem := map[string]any{
		"type": problemType, "title": title, "status": status,
	}
	if detail != "" {
		problem["detail"] = detail
	}
	for key, value := range extra {
		problem[key] = value
	}
	raw, _ := json.Marshal(problem)
	writeJSONBytes(w, status, raw)
}

func pluginProblemResponse(status int, problemType, title, detail string) (int, []byte) {
	raw, _ := json.Marshal(map[string]any{
		"type": problemType, "title": title, "status": status, "detail": detail,
	})
	return status, raw
}

func pluginIssuesResponse(status int, problemType, title, detail string, err error) (int, []byte) {
	raw, _ := json.Marshal(map[string]any{
		"type": problemType, "title": title, "status": status, "detail": detail,
		"issues": issuesFromError(err),
	})
	return status, raw
}

func writePluginIssues(w http.ResponseWriter, status int, problemType, title, detail string, issues plugin.Issues) {
	writePluginProblem(w, status, problemType, title, detail, map[string]any{"issues": issues})
}

// issueKey 从结构化错误里取第一个非空 key（编译失败时用于审计定位）。
func issueKey(err error) string {
	var issues plugin.Issues
	if errors.As(err, &issues) {
		for _, issue := range issues {
			if issue.Key != "" {
				return issue.Key
			}
		}
	}
	return ""
}

func issuesFromError(err error) plugin.Issues {
	var issues plugin.Issues
	if errors.As(err, &issues) {
		return issues
	}
	if err != nil {
		return plugin.Issues{{Key: "", Rule: "internal", Message: err.Error()}}
	}
	return plugin.Issues{}
}
