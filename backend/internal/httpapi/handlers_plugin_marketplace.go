// 插件市场端点（modules-08 §3.10 F-08-33/34 / R-M6D-08）。
//
//	GET  /api/plugin/task/marketplace                admin（聚合索引）
//	GET  /api/plugin/task/marketplace/sources        admin
//	PUT  /api/plugin/task/marketplace/sources        settings:manage（整份替换）
//	POST /api/plugin/task/marketplace/sources/{id}/check settings:manage
//	POST /api/plugin/task/marketplace/install        settings:manage
//
// 市场是出站面（S2）：URL 只允许 https，最小 SSRF 守卫与禁止重定向由
// internal/plugin/market 实现；安装必须校验 source_hash（S3），任何不符在
// 触碰数据库之前返回 422，保证「零写入」。
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/plugin/market"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const (
	// marketCacheTTL 是聚合索引的进程内缓存（F-08-33 的【假设】5 分钟）。
	// 缓存只影响 GET 的延迟与出站频率，不构成真源：refresh=1 或安装路径都
	// 重新拉取，安装永远基于最新索引 + 实测 hash。
	marketCacheTTL = 5 * time.Minute
	// marketFetchTimeout 是单个市场源的拉取上限；多个源串行聚合时总耗时
	// 是该值的倍数，因此保持较短（10s）。
	marketFetchTimeout = 10 * time.Second
	// marketBatchBudget 是「一次请求内串行访问多个源」的总预算（聚合/保存试拉）。
	// 没有它时 100 个源 × 10s 会把管理请求拖到十几分钟；预算到期后剩余源
	// 记为不可达，而不是无限等待。
	marketBatchBudget = 30 * time.Second
	maxMarketSources  = 100
)

const (
	pluginProblemMarketDisabled  = "https://newtg.dev/problems/plugin_marketplace_disabled"
	pluginProblemMarketUnreach   = "https://newtg.dev/problems/plugin_market_unreachable"
	pluginProblemMarketInvalid   = "https://newtg.dev/problems/plugin_market_index_invalid"
	pluginProblemHashMismatch    = "https://newtg.dev/problems/plugin_hash_mismatch"
	pluginProblemURLRejected     = "https://newtg.dev/problems/plugin_source_url_rejected"
	pluginProblemSourceConflict  = "https://newtg.dev/problems/plugin_market_source_conflict"
	pluginProblemPackageMismatch = "https://newtg.dev/problems/plugin_package_mismatch"
)

var (
	pluginMarketSourcesAction = rbac.AuthzAction{ResourceKind: "plugin_marketplace", ActionName: "update_sources"}
	pluginMarketCheckAction   = rbac.AuthzAction{ResourceKind: "plugin_marketplace", ActionName: "check"}
	pluginMarketInstallAction = rbac.AuthzAction{ResourceKind: "plugin_marketplace", ActionName: "install"}
)

// registerMarketWritePolicies 登记市场写端点的字段白名单（BOPLA）。
func registerMarketWritePolicies(authz *rbac.AuthzMiddleware) {
	sourceFields := &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"id": nil, "name": nil, "url": nil, "enabled": nil,
	}}
	authz.SetWritePolicy(pluginMarketSourcesAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"sources": sourceFields,
	}})
	authz.SetWritePolicy(pluginMarketCheckAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	authz.SetWritePolicy(pluginMarketInstallAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"source_id": nil, "key": nil, "version": nil, "activate": nil,
	}})
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type marketSourceView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	Enabled         bool       `json:"enabled"`
	LastCheckStatus string     `json:"last_check_status"`
	LastCheckAt     *time.Time `json:"last_check_at"`
	LastError       *string    `json:"last_error"`
}

func marketSourceViewOf(source store.PluginMarketplaceSource) marketSourceView {
	return marketSourceView{
		ID: source.ID, Name: source.Name, URL: source.URL, Enabled: source.Enabled,
		LastCheckStatus: source.LastCheckStatus, LastCheckAt: source.LastCheckAt,
		LastError: source.LastError,
	}
}

type marketplaceVersionView struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	SourceURL  string `json:"source_url"`
	SourceHash string `json:"source_hash"`
	SourceID   string `json:"source_id"`
	Installed  bool   `json:"installed"`
	Conflict   bool   `json:"conflict,omitempty"`
}

type marketplacePluginView struct {
	Key              string                   `json:"key"`
	Name             string                   `json:"name"`
	Description      string                   `json:"description"`
	IconURL          string                   `json:"icon_url"`
	LatestVersion    string                   `json:"latest_version"`
	Versions         []marketplaceVersionView `json:"versions"`
	Installed        bool                     `json:"installed"`
	InstalledVersion string                   `json:"installed_version,omitempty"`
	UpgradeAvailable bool                     `json:"upgrade_available"`
	Conflicts        []string                 `json:"conflicts,omitempty"`
}

type marketplaceSourceState struct {
	marketSourceView
	// Error 是本次聚合的分类摘要，不落库、不含响应体；用于「部分源不可达」
	// 时的行内提示（与 last_error 的持久化状态区分）。
	Error string `json:"error,omitempty"`
}

type marketplaceIndexView struct {
	SchemaVersion int                      `json:"schema_version"`
	Plugins       []marketplacePluginView  `json:"plugins"`
	Sources       []marketplaceSourceState `json:"sources"`
	CachedAt      time.Time                `json:"cached_at"`
}

// ---------------------------------------------------------------------------
// 市场源 CRUD
// ---------------------------------------------------------------------------

func (h *TaskPluginHandlers) handleMarketSourcesList(w http.ResponseWriter, r *http.Request) {
	sources, err := h.store.ListMarketSources(r.Context())
	if err != nil {
		h.writeMarketError(w, err)
		return
	}
	items := make([]marketSourceView, 0, len(sources))
	for _, source := range sources {
		items = append(items, marketSourceViewOf(source))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *TaskPluginHandlers) handleMarketSourcesPut(w http.ResponseWriter, r *http.Request) {
	if !h.marketplaceEnabled() {
		writePluginProblem(w, http.StatusConflict, pluginProblemMarketDisabled,
			"Marketplace is disabled", "plugin.marketplace.enabled is false", nil)
		return
	}
	var req struct {
		Sources []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			URL     string `json:"url"`
			Enabled *bool  `json:"enabled"`
		} `json:"sources"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Sources) > maxMarketSources {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", fmt.Sprintf("at most %d marketplace sources are allowed", maxMarketSources), nil)
		return
	}

	inputs := make([]store.MarketSourceInput, 0, len(req.Sources))
	seen := make(map[string]struct{}, len(req.Sources))
	for idx, source := range req.Sources {
		name := strings.TrimSpace(source.Name)
		rawURL := strings.TrimSpace(source.URL)
		if name == "" || len([]rune(name)) > 64 {
			writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
				"Invalid request", fmt.Sprintf("sources[%d].name must be 1..64 characters", idx), nil)
			return
		}
		if len([]rune(rawURL)) > 1024 {
			writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
				"Invalid request", fmt.Sprintf("sources[%d].url is too long", idx), nil)
			return
		}
		if _, err := market.ValidateURL(rawURL); err != nil {
			// http 源默认拒绝（R-M6D-03）；私网字面量同样拒绝（S2）。
			writePluginProblem(w, http.StatusUnprocessableEntity, pluginProblemURLRejected,
				"Marketplace source URL rejected", marketErrorSummary(err), nil)
			return
		}
		if _, dup := seen[rawURL]; dup {
			writePluginProblem(w, http.StatusConflict, pluginProblemSourceConflict,
				"Duplicate marketplace source", "the same url appears more than once", nil)
			return
		}
		seen[rawURL] = struct{}{}
		enabled := true
		if source.Enabled != nil {
			enabled = *source.Enabled
		}
		inputs = append(inputs, store.MarketSourceInput{
			ID: strings.TrimSpace(source.ID), Name: name, URL: rawURL, Enabled: enabled,
		})
	}

	before, _ := h.store.ListMarketSources(r.Context())
	saved, err := h.store.ReplaceMarketSources(r.Context(), inputs)
	if err != nil {
		h.writeMarketError(w, err)
		return
	}

	// F-08-34：保存时立即试拉索引并把结果写回该行；失败仍允许保存，
	// 状态列区分 ok/failed。并发/慢源不影响保存结果——错误只影响该行状态。
	checkCtx, cancelCheck := context.WithTimeout(r.Context(), marketBatchBudget)
	defer cancelCheck()
	for _, source := range saved {
		if !source.Enabled {
			continue
		}
		if checkCtx.Err() != nil {
			// 总预算耗尽：剩余行保持 pending，由管理员稍后「重新校验」。
			break
		}
		_, updateErr := h.checkMarketSource(checkCtx, source)
		if updateErr != nil {
			h.log.Warn("plugin: marketplace source check after save failed",
				"source_id", source.ID, "err", updateErr)
		}
	}
	after, err := h.store.ListMarketSources(r.Context())
	if err != nil {
		h.writeMarketError(w, err)
		return
	}
	h.writeMarketSourceAudit(r, before, after)
	h.invalidateMarketCache()
	items := make([]marketSourceView, 0, len(after))
	for _, source := range after {
		items = append(items, marketSourceViewOf(source))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *TaskPluginHandlers) handleMarketSourceCheck(w http.ResponseWriter, r *http.Request) {
	if !h.marketplaceEnabled() {
		writePluginProblem(w, http.StatusConflict, pluginProblemMarketDisabled,
			"Marketplace is disabled", "plugin.marketplace.enabled is false", nil)
		return
	}
	source, err := h.store.GetMarketSource(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeMarketError(w, err)
		return
	}
	updated, checkErr := h.checkMarketSource(r.Context(), *source)
	if checkErr != nil {
		// DB 写回失败是真正的服务端错误；远端失败已由 checkMarketSource 写进
		// last_check_status=failed 并返回 200。
		h.writeMarketError(w, checkErr)
		return
	}
	writeJSON(w, http.StatusOK, marketSourceViewOf(*updated))
}

// checkMarketSource 拉取索引并把结果写回 B-13。
//
// 返回值语义：远端失败不是本函数的 error——它被转换为 last_check_status=failed
// 并持久化（管理面需要行内显示原因）；只有 DB 写回失败才返回 error。
func (h *TaskPluginHandlers) checkMarketSource(ctx context.Context, source store.PluginMarketplaceSource) (*store.PluginMarketplaceSource, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, marketFetchTimeout)
	defer cancel()
	_, fetchErr := h.market.FetchIndex(fetchCtx, source.URL)

	status := "ok"
	summary := ""
	if fetchErr != nil {
		status = "failed"
		summary = marketErrorSummary(fetchErr)
	}
	updated, err := h.store.UpdateMarketSourceCheck(ctx, source.ID, status, summary, h.now())
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (h *TaskPluginHandlers) writeMarketSourceAudit(r *http.Request, before, after []store.PluginMarketplaceSource) {
	rows := make([]map[string]any, 0, len(after))
	for _, source := range after {
		rows = append(rows, map[string]any{
			"id": source.ID, "name": source.Name, "url": sanitizeMarketURL(source.URL),
			"enabled": source.Enabled,
		})
	}
	h.writePluginAudit(r, audit.ActionPluginMarketSourceUpdate, http.StatusOK, map[string]any{
		"before_count": len(before), "after_count": len(after), "sources": rows,
	})
}

// sanitizeMarketURL 剥离 userinfo/query/fragment 后再入审计。
//
// 市场源 URL 理论上不该带令牌，但管理员可能误贴带签名参数的索引地址；
// 审计保留 12 个月，宁可少记路径参数也不留下长期凭据副本。
func sanitizeMarketURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// ---------------------------------------------------------------------------
// 聚合索引
// ---------------------------------------------------------------------------

func (h *TaskPluginHandlers) marketplaceEnabled() bool {
	return readConfigBool(h.config, "plugin.marketplace.enabled", true)
}

func (h *TaskPluginHandlers) handleMarketplace(w http.ResponseWriter, r *http.Request) {
	if !h.marketplaceEnabled() {
		writePluginProblem(w, http.StatusConflict, pluginProblemMarketDisabled,
			"Marketplace is disabled", "plugin.marketplace.enabled is false", nil)
		return
	}
	refresh := false
	switch strings.TrimSpace(r.URL.Query().Get("refresh")) {
	case "1", "true":
		refresh = true
	}
	view, err := h.aggregateMarketplace(r.Context(), refresh)
	if err != nil {
		h.writeMarketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// aggregateMarketplace 聚合所有启用源并标注安装状态。
//
// 缓存只在 refresh=0 且未过期时命中；DB 读取失败时返回错误且不覆盖旧缓存
// （赋值只在成功构建后发生）。单个源不可达不是聚合失败：它只体现在
// Sources[].error 中，其余源继续聚合。多源同 key 同 version 不同 hash →
// 该版本标 conflict（F-08-33 的【假设】默认：标灰而不是猜源）。
func (h *TaskPluginHandlers) aggregateMarketplace(ctx context.Context, refresh bool) (*marketplaceIndexView, error) {
	h.marketMu.Lock()
	defer h.marketMu.Unlock()
	if !refresh && h.marketCache != nil && h.now().Sub(h.marketCachedAt) < marketCacheTTL {
		return h.marketCache, nil
	}

	sources, err := h.store.ListMarketSources(ctx)
	if err != nil {
		return nil, err
	}

	aggregateCtx, cancelAggregate := context.WithTimeout(ctx, marketBatchBudget)
	defer cancelAggregate()
	aggregated := make(map[string]*marketplacePluginView)
	order := make([]string, 0, len(sources))
	sourceStates := make([]marketplaceSourceState, 0, len(sources))
	for _, source := range sources {
		state := marketplaceSourceState{marketSourceView: marketSourceViewOf(source)}
		if !source.Enabled {
			sourceStates = append(sourceStates, state)
			continue
		}
		if aggregateCtx.Err() != nil {
			state.Error = "timeout"
			sourceStates = append(sourceStates, state)
			continue
		}
		fetchCtx, cancel := context.WithTimeout(aggregateCtx, marketFetchTimeout)
		index, fetchErr := h.market.FetchIndex(fetchCtx, source.URL)
		cancel()
		if fetchErr != nil {
			state.Error = marketErrorSummary(fetchErr)
			// 聚合时的失败不写库（试拉状态由显式 check/保存路径维护），
			// 避免 GET 成为隐式写路径。
			sourceStates = append(sourceStates, state)
			continue
		}
		for _, p := range index.Plugins {
			entry, ok := aggregated[p.Key]
			if !ok {
				entry = &marketplacePluginView{
					Key: p.Key, Name: p.Name, Description: p.Description, IconURL: p.IconURL,
				}
				aggregated[p.Key] = entry
				order = append(order, p.Key)
			} else {
				if entry.Name == "" {
					entry.Name = p.Name
				}
				if entry.Description == "" {
					entry.Description = p.Description
				}
				if entry.IconURL == "" {
					entry.IconURL = p.IconURL
				}
			}
			for _, v := range p.Versions {
				existing := findMarketVersion(entry.Versions, v.Version)
				if existing != nil {
					if existing.SourceHash != v.SourceHash {
						existing.Conflict = true
						entry.Conflicts = appendUnique(entry.Conflicts, v.Version)
					}
					continue
				}
				entry.Versions = append(entry.Versions, marketplaceVersionView{
					Version: v.Version, APIVersion: v.APIVersion, SourceURL: v.SourceURL,
					SourceHash: v.SourceHash, SourceID: source.ID,
				})
			}
		}
		sourceStates = append(sourceStates, state)
	}

	if err := h.annotateInstalled(ctx, aggregated); err != nil {
		return nil, err
	}
	plugins := make([]marketplacePluginView, 0, len(order))
	for _, key := range order {
		entry := aggregated[key]
		entry.LatestVersion = highestVersion(entry.Versions)
		if entry.InstalledVersion != "" && highestVersion(entry.Versions) != "" {
			entry.UpgradeAvailable = compareVersions(highestVersion(entry.Versions), entry.InstalledVersion) > 0
		}
		plugins = append(plugins, *entry)
	}
	view := &marketplaceIndexView{
		SchemaVersion: 1, Plugins: plugins, Sources: sourceStates, CachedAt: h.now(),
	}
	h.marketCache = view
	h.marketCachedAt = view.CachedAt
	return view, nil
}

// annotateInstalled 标注索引中每个插件/版本的本地安装状态。
func (h *TaskPluginHandlers) annotateInstalled(ctx context.Context, aggregated map[string]*marketplacePluginView) error {
	plugins, err := h.store.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range plugins {
		entry, ok := aggregated[p.Key]
		if !ok {
			continue
		}
		versions, err := h.store.ListVersions(ctx, p.Key)
		if err != nil {
			return err
		}
		entry.Installed = len(versions) > 0
		for _, v := range versions {
			for i := range entry.Versions {
				if entry.Versions[i].Version == v.Version {
					entry.Versions[i].Installed = true
				}
			}
		}
		installedVersion := ""
		if p.ActiveVersion != nil {
			installedVersion = *p.ActiveVersion
		} else if len(versions) > 0 {
			installedVersion = versions[0].Version
		}
		entry.InstalledVersion = installedVersion
	}
	return nil
}

// invalidateMarketCache 让下一次 GET 重新聚合（安装会改变 installed 标记，
// 保存源会改变参与聚合的源集合）。
func (h *TaskPluginHandlers) invalidateMarketCache() {
	h.marketMu.Lock()
	defer h.marketMu.Unlock()
	h.marketCache = nil
	h.marketCachedAt = time.Time{}
}

func findMarketVersion(versions []marketplaceVersionView, version string) *marketplaceVersionView {
	for i := range versions {
		if versions[i].Version == version {
			return &versions[i]
		}
	}
	return nil
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

// highestVersion 用最小 semver 比较取最高版本；非 semver 时回退字典序，
// 保证展示稳定（真正的版本顺序由安装对话框逐项列出，不依赖该函数）。
func highestVersion(versions []marketplaceVersionView) string {
	best := ""
	for _, v := range versions {
		if best == "" || compareVersions(v.Version, best) > 0 {
			best = v.Version
		}
	}
	return best
}

// compareVersions 比较两个点分数字版本；任一段非数字时回退字符串比较。
// 只用于「可升级」标记，不用于任何写入决策。
func compareVersions(a, b string) int {
	parse := func(value string) ([]int64, bool) {
		parts := strings.Split(value, ".")
		out := make([]int64, 0, len(parts))
		for _, part := range parts {
			// 只接受纯数字段；pre-release（-beta）等走回退分支。
			if part == "" || strings.Trim(part, "0123456789") != "" {
				return nil, false
			}
			var n int64
			for _, r := range part {
				if r < '0' || r > '9' {
					return nil, false
				}
				n = n*10 + int64(r-'0')
			}
			out = append(out, n)
		}
		return out, true
	}
	na, okA := parse(a)
	nb, okB := parse(b)
	if !okA || !okB {
		return strings.Compare(a, b)
	}
	for i := 0; i < len(na) || i < len(nb); i++ {
		var x, y int64
		if i < len(na) {
			x = na[i]
		}
		if i < len(nb) {
			y = nb[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 安装
// ---------------------------------------------------------------------------

// handleMarketInstall 安装市场索引中的一个版本。
//
// 零写入保证（S3）：索引拉取 → 版本定位 → 包体下载 → hash 校验 → 编译/校验
// 全部发生在任何 DB 调用之前；任一步失败都不会创建主表行或版本行。
// 默认不激活（R-M6D-07 / F-08-33）；只有显式 activate=true 才在入库后激活。
func (h *TaskPluginHandlers) handleMarketInstall(w http.ResponseWriter, r *http.Request) {
	if !h.marketplaceEnabled() {
		writePluginProblem(w, http.StatusConflict, pluginProblemMarketDisabled,
			"Marketplace is disabled", "plugin.marketplace.enabled is false", nil)
		return
	}
	var req struct {
		SourceID string `json:"source_id"`
		Key      string `json:"key"`
		Version  string `json:"version"`
		Activate *bool  `json:"activate"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	sourceID := strings.TrimSpace(req.SourceID)
	key := strings.TrimSpace(req.Key)
	version := strings.TrimSpace(req.Version)
	if sourceID == "" || key == "" || version == "" {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "source_id, key and version are required", nil)
		return
	}
	source, err := h.store.GetMarketSource(r.Context(), sourceID)
	if err != nil {
		h.writeMarketError(w, err)
		return
	}

	fetchCtx, cancel := context.WithTimeout(r.Context(), marketFetchTimeout)
	index, err := h.market.FetchIndex(fetchCtx, source.URL)
	cancel()
	if err != nil {
		h.auditMarketInstallFailure(r, key, version, "index_unreachable", http.StatusBadGateway)
		h.writeMarketFetchError(w, err)
		return
	}
	pluginEntry, versionEntry := index.FindVersion(key, version)
	if pluginEntry == nil {
		writePluginProblem(w, http.StatusNotFound, pluginProblemNotFound,
			"Plugin not found in marketplace index", "the index does not declare this plugin key", nil)
		return
	}
	if versionEntry == nil {
		writePluginProblem(w, http.StatusNotFound, pluginProblemVersionNotFound,
			"Version not found in marketplace index", "the index does not declare this plugin version", nil)
		return
	}

	sourceCtx, cancelSource := context.WithTimeout(r.Context(), marketFetchTimeout)
	sourceBytes, err := h.market.FetchSource(sourceCtx, versionEntry.SourceURL, versionEntry.SourceHash)
	cancelSource()
	if err != nil {
		h.auditMarketInstallFailure(r, key, version, "package_rejected", http.StatusUnprocessableEntity)
		h.writeMarketFetchError(w, err)
		return
	}
	sourceText := string(sourceBytes)

	compiled, err := plugin.Compile(r.Context(), sourceText, plugin.CompileOptions{Layer: plugin.LayerOverride})
	if err != nil {
		h.auditMarketInstallFailure(r, key, version, "compile_failed", http.StatusUnprocessableEntity)
		status, body := pluginIssuesResponse(http.StatusUnprocessableEntity, pluginProblemCompileFailed,
			"Plugin compile failed", "the downloaded plugin source failed compilation or validation", err)
		writeJSONBytes(w, status, body)
		return
	}
	manifest := compiled.Manifest()
	if manifest.Key != key || manifest.Version != version {
		// F-08-33：包体版本与索引声明不符 → 拒绝。否则 index 声明的
		// (key, version) 与实际入库的内容会指向不同版本，hash 校验也失去意义。
		h.auditMarketInstallFailure(r, key, version, "package_mismatch", http.StatusUnprocessableEntity)
		writePluginProblem(w, http.StatusUnprocessableEntity, pluginProblemPackageMismatch,
			"Package does not match index",
			fmt.Sprintf("index declares %s@%s, package exports %s@%s", key, version, manifest.Key, manifest.Version), nil)
		return
	}

	actor := actorID(r)
	upload := store.TaskPluginUpload{
		Key: manifest.Key, Name: manifest.Name, Version: manifest.Version,
		APIVersion: "v1", SourceText: sourceText, SourceHash: compiled.SourceHash(),
		Icon: stringPtr(manifest.Icon), Description: stringPtr(chooseDescription(manifest.Description)),
		UploadedBy: actor,
	}
	p, v, created, err := h.store.Upload(r.Context(), upload)
	if err != nil {
		if errors.Is(err, store.ErrTaskPluginVersionConflict) {
			h.auditMarketInstallFailure(r, key, version, "version_conflict", http.StatusConflict)
			writePluginProblem(w, http.StatusConflict, pluginProblemVersionConflict,
				"Plugin version already exists",
				"the same plugin key/version is already stored with different source content", nil)
			return
		}
		h.log.Error("plugin: market install persist failed", "err", err)
		h.auditMarketInstallFailure(r, key, version, "store_error", http.StatusInternalServerError)
		writePluginProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "", nil)
		return
	}

	activated := false
	if req.Activate != nil && *req.Activate {
		p, _, err = h.store.Activate(r.Context(), key, version, nil)
		if err != nil {
			h.auditMarketInstallFailure(r, key, version, "activate_failed", http.StatusConflict)
			h.writeStoreError(w, "activate marketplace install", err)
			return
		}
		activated = true
	}
	h.reloadAsync(r.Context())
	h.invalidateMarketCache()
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	h.writePluginAudit(r, audit.ActionPluginMarketInstall, status, map[string]any{
		"source_id": source.ID, "plugin_key": key, "version": version,
		"source_hash_prefix": hashPrefix(v.SourceHash), "created": created, "activated": activated,
	})
	counts, err := h.bindingCounts(r.Context(), []string{p.Key})
	if err != nil {
		h.writeStoreError(w, "count bound channels", err)
		return
	}
	writeJSON(w, status, map[string]any{
		"installed": true, "created": created, "activated": activated,
		"source_id": source.ID, "key": key, "version": version,
		"plugin": h.pluginView(*p, counts[p.Key]), "version_row": versionView(*v),
	})
}

// auditMarketInstallFailure 记录安装失败（result=failed）；detail 不含包体。
func (h *TaskPluginHandlers) auditMarketInstallFailure(r *http.Request, key, version, reason string, status int) {
	h.writePluginAudit(r, audit.ActionPluginMarketInstall, status, map[string]any{
		"plugin_key": key, "version": version, "result_reason": reason,
	})
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func (h *TaskPluginHandlers) writeMarketError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrMarketSourceNotFound):
		writePluginProblem(w, http.StatusNotFound, pluginProblemNotFound,
			"Marketplace source not found", "the requested marketplace source does not exist", nil)
	case errors.Is(err, store.ErrMarketSourceURLConflict):
		writePluginProblem(w, http.StatusConflict, pluginProblemSourceConflict,
			"Duplicate marketplace source", "the same url appears more than once", nil)
	default:
		h.log.Error("plugin: marketplace operation failed", "err", err)
		writePluginProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "", nil)
	}
}

// writeMarketFetchError 把出站失败分类映射为稳定错误码。
//
// hash 不符必须是 422 plugin_hash_mismatch（S3 的验收点）；URL/重定向拒绝
// 是 422 plugin_source_url_rejected；索引结构问题 422；网络/HTTP 状态 502。
func (h *TaskPluginHandlers) writeMarketFetchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, market.ErrHashMismatch):
		writePluginProblem(w, http.StatusUnprocessableEntity, pluginProblemHashMismatch,
			"Plugin hash mismatch", "the downloaded source does not match the index source_hash", nil)
	case errors.Is(err, market.ErrNonHTTPS), errors.Is(err, market.ErrUnsafeURL), errors.Is(err, market.ErrRedirect):
		writePluginProblem(w, http.StatusUnprocessableEntity, pluginProblemURLRejected,
			"Marketplace URL rejected", marketErrorSummary(err), nil)
	case errors.Is(err, market.ErrInvalidIndex):
		writePluginProblem(w, http.StatusUnprocessableEntity, pluginProblemMarketInvalid,
			"Invalid marketplace index", marketErrorSummary(err), nil)
	case errors.Is(err, market.ErrTooLarge):
		writePluginProblem(w, http.StatusUnprocessableEntity, pluginProblemMarketInvalid,
			"Marketplace response too large", marketErrorSummary(err), nil)
	default:
		writePluginProblem(w, http.StatusBadGateway, pluginProblemMarketUnreach,
			"Marketplace source unreachable", marketErrorSummary(err), nil)
	}
}

// marketErrorSummary 生成**不含响应体**的短摘要（B-13 last_error 口径）。
func marketErrorSummary(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, market.ErrHashMismatch):
		return "source hash mismatch"
	case errors.Is(err, market.ErrRedirect):
		return "redirects are not allowed"
	case errors.Is(err, market.ErrNonHTTPS):
		return "url must use https"
	case errors.Is(err, market.ErrUnsafeURL):
		return "url rejected by SSRF guard"
	case errors.Is(err, market.ErrTooLarge):
		return "response exceeds size limit"
	case errors.Is(err, market.ErrInvalidIndex):
		return "invalid marketplace index"
	}
	var statusErr *market.StatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("HTTP %d", statusErr.Code)
	}
	// 网络错误只保留类别，避免把远端 DNS/连接细节（可能含内网地址）写进库。
	return "source unreachable"
}
