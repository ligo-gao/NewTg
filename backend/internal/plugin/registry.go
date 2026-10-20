package plugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PluginStatus 是运行状态的五态词汇（relay-06 §2.3/§2.5 与 modules-08
// F-08-28 同集）。UI 徽章与 `runtime/status` 必须使用同一组字面量：
//
//	registered        正常（含「上传覆盖层覆盖内置」的成功态）
//	disabled          已禁用（全局关闭，或内置被禁用且无覆盖层）
//	disabled_fallback 内置被全局禁用，但同 key 有可用上传覆盖层（回退生效）
//	not_registered    未注册（无任何可用层）
//	compile_failed    编译/校验失败，且没有可回退的层
type PluginStatus string

const (
	StatusRegistered       PluginStatus = "registered"
	StatusDisabled         PluginStatus = "disabled"
	StatusDisabledFallback PluginStatus = "disabled_fallback"
	StatusNotRegistered    PluginStatus = "not_registered"
	StatusCompileFailed    PluginStatus = "compile_failed"
)

// ActiveVersion 是调用方（DB 层）注入的版本行投影。
//
// 只承载运行时需要的列：管理元数据（icon/uploaded_by 等）不进运行时快照。
// `Enabled`/`Active` 由调用方筛选，管理器仍再判一次（fail-closed：注入方
// 的筛选错误不应让未启用版本被执行）。
type ActiveVersion struct {
	Key        string
	Version    string
	SourceText string
	SourceHash string
	Enabled    bool
	Active     bool
	UpdatedAt  time.Time
}

// VersionLoader 是运行时读取 DB 版本层的最小接口。
//
// 30s 轮询与 Redis pub/sub 接线属于装配层（t3-brief 冻结约定 6）；
// 本包只提供 Reload，调用方决定何时触发。
type VersionLoader interface {
	// LoadActive 返回全部版本的 active 投影（实现方通常只查 active=true 行；
	// 管理器会忽略 Active=false 的行）。
	LoadActive(ctx context.Context) ([]ActiveVersion, error)
	// LoadVersion 读取指定 key/version 的源（dryrun 指定版本时使用）。
	LoadVersion(ctx context.Context, key, version string) (*ActiveVersion, error)
}

// Options 是管理器配置。
type Options struct {
	Node string
	// Timeout 覆盖单次执行上限；0 走 DefaultTimeout（5s）。
	Timeout time.Duration
	// MasterEnabled 是系统级总开关 `TaskPluginEnabled`（relay-06 §0.1）。
	// 关闭时全部 key 状态为 disabled，不注册任何数据面路由。
	MasterEnabled bool
	// DisabledFactoryKeys 是 `plugin.disabled_factory_keys` 设置项。
	DisabledFactoryKeys []string
}

// Entry 是快照中一个 key 的生效结果。
type Entry struct {
	Key     string          `json:"key"`
	Status  PluginStatus    `json:"status"`
	Version string          `json:"version,omitempty"`
	Layer   Layer           `json:"layer,omitempty"`
	Plugin  *CompiledPlugin `json:"-"`
	// Errors 是加载期结构化错误。status=registered 时也可能出现
	// 「覆盖层失败但已回退 factory」的非致命诊断，规则为
	// override_compile_failed（见 buildSnapshot）。
	Errors Issues `json:"errors,omitempty"`
}

// Snapshot 是一次 generation-atomic 发布的结果（relay-06 §2.3）。
//
// 发布后不可变：调用方拿到 *Snapshot 后整个请求生命周期内都用它，
// 期间新的 active 版本不会改变本次请求看到的路由/插件（generation-atomic）。
type Snapshot struct {
	generation uint64
	revision   string
	builtAt    time.Time
	entries    map[string]*Entry
	routes     map[string]string // 归一化路由形状 → 插件 key（诊断用）
	// routeIndex 是 HTTP 面消费的可匹配索引（T6c-a / R-03）。它与 routes
	// 同批构建、同批冻结：routes 只回答「谁占了哪个形状」，routeIndex 回答
	// 「这个请求命中哪条路由」，后者必须携带 Route 与插件 key 才能分发。
	routeIndex RouteIndex
}

// Generation 返回节点本地单调递增代际（relay-06 §2.5：跨节点比较用 Revision）。
func (s *Snapshot) Generation() uint64 { return s.generation }

// Revision 返回 DB 版本的 max(updated_at) 字符串（冻结约定 6）。
func (s *Snapshot) Revision() string { return s.revision }

// BuiltAt 返回快照构建时间。
func (s *Snapshot) BuiltAt() time.Time { return s.builtAt }

// Keys 返回快照内全部 key（升序）。
func (s *Snapshot) Keys() []string {
	if s == nil {
		return nil
	}
	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Lookup 返回 key 的生效条目；未知 key 合成 not_registered，绝不返回 nil。
func (s *Snapshot) Lookup(key string) *Entry {
	if s != nil {
		if entry, ok := s.entries[key]; ok {
			return entry
		}
	}
	return &Entry{Key: key, Status: StatusNotRegistered}
}

// Plugin 返回可执行的插件；disabled/compile_failed/not_registered 返回 false。
func (s *Snapshot) Plugin(key string) (*CompiledPlugin, bool) {
	entry := s.Lookup(key)
	if entry.Plugin == nil {
		return nil, false
	}
	return entry.Plugin, true
}

// RouteOwners 返回路由索引的只读副本（诊断与测试用）。
func (s *Snapshot) RouteOwners() map[string]string {
	out := make(map[string]string, len(s.routes))
	for shape, key := range s.routes {
		out[shape] = key
	}
	return out
}

// Routes 返回本快照的原生路由索引（T6c-a）。
//
// 返回值是**已冻结**的只读快照：索引在 buildSnapshot 后不再修改，调用方可以
// 在整个请求生命周期内安全复用（generation-atomic）。零值 Snapshot / 首次
// Reload 之前返回空索引，Match 一律未命中，而不是 nil 恐慌。
func (s *Snapshot) Routes() RouteIndex {
	if s == nil {
		return RouteIndex{}
	}
	return s.routeIndex
}

// Routes 返回当前快照的路由索引（等价于 Snapshot().Routes()）。
//
// 名字按 t1-brief 的「Registry.Routes」保留：仓库里没有独立的 Registry 类型，
// Snapshot 就是注册结果的权威载体，Manager 只是它的最新发布者。
func (m *Manager) Routes() RouteIndex {
	return m.Snapshot().Routes()
}

// SnapshotManager 是 Manager 的冻结别名（t3-brief 冻结约定 6 使用该名称）。
// 保留两个名字：包内实现统一用 Manager，装配层按 brief 的名字接线不受影响。
type SnapshotManager = Manager

// Manager 拥有 factory 层、覆盖层 loader 与当前快照。
//
// 并发模型：Reload 串行（mu），发布用 atomic.Pointer；读路径永不拿锁，
// 保证请求路径不会被 30s 轮询阻塞。
type Manager struct {
	mu      sync.Mutex
	node    string
	timeout time.Duration

	factory map[string]string
	// factoryCompiled 是 factory 层的编译缓存：内置插件是构建期产物，
	// 每次 Reload 重新编译既浪费又可能因引擎状态出现「偶发失败」，
	// 启动时编译一次、失败即 panic（MustNewManager）。
	factoryCompiled map[string]*CompiledPlugin
	factoryErrs     map[string]Issues

	loader          VersionLoader
	masterEnabled   bool
	disabledFactory map[string]struct{}

	generation   uint64
	lastBuildKey string
	reloadedAt   time.Time
	reloadErr    string
	snapshot     atomic.Pointer[Snapshot]
}

// NewManager 编译 factory 层并建立管理器。
//
// 内置插件编译/校验失败返回错误：按 relay-06 §2.1 这属于不可恢复的
// 构建期缺陷（内置插件编入二进制），调用方应使用 MustNewManager 让进程
// 启动失败，而不是带着残缺注册表上线。
func NewManager(factorySources map[string]string, loader VersionLoader, opts Options) (*Manager, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	m := &Manager{
		node:            opts.Node,
		timeout:         timeout,
		factory:         make(map[string]string, len(factorySources)),
		factoryCompiled: make(map[string]*CompiledPlugin, len(factorySources)),
		factoryErrs:     make(map[string]Issues),
		loader:          loader,
		masterEnabled:   opts.MasterEnabled,
		disabledFactory: make(map[string]struct{}, len(opts.DisabledFactoryKeys)),
	}
	if m.node == "" {
		m.node = "local"
	}
	for key, source := range factorySources {
		m.factory[key] = source
	}
	for _, key := range opts.DisabledFactoryKeys {
		m.disabledFactory[key] = struct{}{}
	}

	for _, key := range sortedKeys(m.factory) {
		plugin, err := Compile(context.Background(), m.factory[key], CompileOptions{
			Timeout:     timeout,
			ExpectedKey: key,
			Layer:       LayerFactory,
		})
		if err != nil {
			m.factoryErrs[key] = asIssues(key, err)
			continue
		}
		m.factoryCompiled[key] = plugin
	}
	if len(m.factoryErrs) > 0 {
		var all Issues
		for _, key := range sortedKeys(m.factoryErrs) {
			all = append(all, m.factoryErrs[key]...)
		}
		return nil, all
	}
	return m, nil
}

// MustNewManager 同 NewManager，但内置插件失败时 panic（relay-06 §2.1）。
func MustNewManager(factorySources map[string]string, loader VersionLoader, opts Options) *Manager {
	m, err := NewManager(factorySources, loader, opts)
	if err != nil {
		panic(fmt.Sprintf("plugin: 内置插件加载失败: %v", err))
	}
	return m
}

// SetMasterEnabled 更新系统级总开关；下次 Reload 生效。
func (m *Manager) SetMasterEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.masterEnabled = enabled
}

// SetDisabledFactoryKeys 更新内置禁用集合；下次 Reload 生效。
func (m *Manager) SetDisabledFactoryKeys(keys []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		next[key] = struct{}{}
	}
	m.disabledFactory = next
}

// MasterEnabled 返回当前总开关。
func (m *Manager) MasterEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.masterEnabled
}

// Reload 重建并原子发布快照。
//
// 「内容不变就不发布」：30s 轮询若每次都递增 generation，会让在途请求的
// generation 失去「版本代际」意义（relay-06 §2.3 要求一次请求全程锁定
// 一个 generation）。buildKey 覆盖 revision、总开关、内置禁用集合与 factory
// 源哈希，任一变化才发布新 generation。
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var versions []ActiveVersion
	revision := ""
	if m.loader != nil {
		loaded, err := m.loader.LoadActive(ctx)
		if err != nil {
			// 加载失败保留上一份快照（fail-closed：宁可继续用旧的可用版本，
			// 也不清空注册表让全部任务不可用），同时暴露 reload_error。
			m.reloadErr = err.Error()
			return err
		}
		versions = loaded
		for _, version := range versions {
			stamp := version.UpdatedAt.UTC().Format(time.RFC3339Nano)
			if stamp > revision {
				revision = stamp
			}
		}
	}

	buildKey := m.buildKey(revision, versions)
	if m.snapshot.Load() != nil && buildKey == m.lastBuildKey {
		m.reloadedAt = time.Now()
		m.reloadErr = ""
		return nil
	}

	snap := m.buildSnapshot(versions, revision)
	m.generation++
	snap.generation = m.generation
	m.snapshot.Store(snap)
	m.lastBuildKey = buildKey
	m.reloadedAt = time.Now()
	m.reloadErr = ""
	return nil
}

// Snapshot 返回当前快照；首次 Reload 之前返回空快照（不是 nil）。
func (m *Manager) Snapshot() *Snapshot {
	if snap := m.snapshot.Load(); snap != nil {
		return snap
	}
	return &Snapshot{entries: map[string]*Entry{}, routes: map[string]string{}}
}

// buildKey 把影响快照内容的动态输入压成签名：DB revision、系统总开关、
// 内置禁用集合，以及每个版本的 (key/version/enabled/active/hash)。
// factory 源是编译期常量（进程内不会变化），故不入签名；timeout/Node
// 不改变注册结果，也不入签名。签名相同则 Reload 不发布新 generation。
func (m *Manager) buildKey(revision string, versions []ActiveVersion) string {
	var b strings.Builder
	b.WriteString(revision)
	b.WriteString("|master=")
	b.WriteString(fmt.Sprintf("%t", m.masterEnabled))
	disabled := make([]string, 0, len(m.disabledFactory))
	for key := range m.disabledFactory {
		disabled = append(disabled, key)
	}
	sort.Strings(disabled)
	b.WriteString("|disabled=")
	b.WriteString(strings.Join(disabled, ","))
	rows := make([]string, 0, len(versions))
	for _, version := range versions {
		rows = append(rows, fmt.Sprintf("%s/%s/%t/%t/%s", version.Key, version.Version, version.Enabled, version.Active, version.SourceHash))
	}
	sort.Strings(rows)
	b.WriteString("|versions=")
	b.WriteString(strings.Join(rows, ";"))
	return b.String()
}

// buildSnapshot 执行三层模型的生效判定。
//
// 生效顺序（plan R-M6D-06）：
//  1. 总开关关闭 → 全部 disabled；
//  2. 内置未禁用 → 覆盖层（active 版本）优先，编译失败回退 factory；
//  3. 内置已禁用 → 只用覆盖层；有可用覆盖层为 disabled_fallback，
//     无覆盖层为 disabled；
//  4. 没有任何可用层且存在失败层 → compile_failed
//     （「compile_failed 仅当无可用层」）。
func (m *Manager) buildSnapshot(versions []ActiveVersion, revision string) *Snapshot {
	snap := &Snapshot{
		revision:   revision,
		builtAt:    time.Now(),
		entries:    make(map[string]*Entry, len(m.factory)+len(versions)),
		routes:     make(map[string]string),
		routeIndex: newRouteIndex(),
	}
	overrides := make(map[string][]ActiveVersion, len(versions))
	keys := make(map[string]struct{}, len(m.factory)+len(versions))
	for _, version := range versions {
		keys[version.Key] = struct{}{}
		if !version.Enabled || !version.Active {
			continue
		}
		overrides[version.Key] = append(overrides[version.Key], version)
	}
	for key := range m.factory {
		keys[key] = struct{}{}
	}

	channelOwners := make(map[int]string)
	for _, key := range sortedKeys(keys) {
		entry := m.effectiveEntry(key, overrides[key])
		if entry.Plugin != nil {
			if conflict := registerRoutes(snap.routes, &snap.routeIndex, entry.Plugin); conflict != nil {
				entry.Status = StatusCompileFailed
				entry.Plugin = nil
				entry.Version = ""
				entry.Errors = append(entry.Errors, conflict...)
			}
		}
		if entry.Plugin != nil {
			if conflict := registerChannelTypes(channelOwners, entry.Plugin); conflict != nil {
				entry.Status = StatusCompileFailed
				entry.Plugin = nil
				entry.Version = ""
				entry.Errors = append(entry.Errors, conflict...)
			}
		}
		snap.entries[key] = entry
	}
	return snap
}

// effectiveEntry 计算单个 key 的层优先级与状态。
func (m *Manager) effectiveEntry(key string, overrides []ActiveVersion) *Entry {
	entry := &Entry{Key: key}

	if !m.masterEnabled {
		// 系统级总开关关闭：不注册任何插件，但保留 key 便于 UI 展示。
		entry.Status = StatusDisabled
		return entry
	}

	_, factoryDisabled := m.disabledFactory[key]
	_, hasFactory := m.factory[key]

	var overrideErrs Issues
	var overridePlugin *CompiledPlugin
	if len(overrides) > 1 {
		// active 部分唯一索引保证最多一行；出现多行说明注入方/DB 约束损坏
		// （数据完整性破坏）。裁决 R-M6D-15：保留 fail-closed 的
		// compile_failed，错误码明确为 multiple_active_versions；
		// 不猜哪个是权威版本（选错会让任务解析到非预期插件版本）。
		entry.Status = StatusCompileFailed
		entry.Errors = append(entry.Errors, issuef(key, "versions", "", "multiple_active_versions",
			"同 key 存在 %d 个 active 版本（数据完整性破坏），拒绝选择", len(overrides)))
		return entry
	}
	if len(overrides) == 1 {
		plugin, issues := compileOverride(key, overrides[0], m.timeout)
		if plugin != nil {
			overridePlugin = plugin
		} else {
			overrideErrs = issues
		}
	}

	if !factoryDisabled {
		if overridePlugin != nil {
			// 覆盖层成功：正常注册（五态词汇不区分「来自哪一层」，
			// 层信息放在 Entry.Layer 供详情页展示）。
			entry.Status = StatusRegistered
			entry.Plugin = overridePlugin
			entry.Version = overridePlugin.Version
			entry.Layer = LayerOverride
			return entry
		}
		if cached, ok := m.factoryCompiled[key]; ok {
			entry.Status = StatusRegistered
			entry.Plugin = cached
			entry.Version = cached.Version
			entry.Layer = LayerFactory
			if overrideErrs.HasErrors() {
				// plan R-M6D-06：覆盖层编译失败不激活，运行期回退 factory。
				// 错误保留为诊断，但不改变「可用」这一事实。
				entry.Errors = append(entry.Errors, Issue{
					Key: key, FieldPath: "versions." + overrides[0].Version, Rule: "override_compile_failed",
					Message: "覆盖层编译失败，已回退内置插件: " + overrideErrs.Error(),
				})
			}
			return entry
		}
		if cacheErr, ok := m.factoryErrs[key]; ok {
			// NewManager 已在内置编译失败时返回错误，正常不会走到这里。
			entry.Status = StatusCompileFailed
			entry.Errors = cacheErr
			return entry
		}
		// 覆盖层失败且无 factory：无可用层。若根本没有覆盖层行（例如注入方
		// 返回了 enabled=false 的版本），状态是 not_registered，不是 compile_failed。
		if len(overrides) > 0 {
			entry.Status = StatusCompileFailed
			entry.Errors = overrideErrs
			return entry
		}
		entry.Status = StatusNotRegistered
		return entry
	}

	// 内置被全局禁用：只允许覆盖层。
	if overridePlugin != nil {
		entry.Status = StatusDisabledFallback
		entry.Plugin = overridePlugin
		entry.Version = overridePlugin.Version
		entry.Layer = LayerOverride
		return entry
	}
	if len(overrides) > 0 && overrideErrs.HasErrors() {
		// 有覆盖层但不可用，且内置被禁用 → 无可用层（plan R-M6D-06）。
		entry.Status = StatusCompileFailed
		entry.Errors = overrideErrs
		return entry
	}
	if hasFactory {
		entry.Status = StatusDisabled
		return entry
	}
	entry.Status = StatusNotRegistered
	return entry
}

// compileOverride 编译单个覆盖层版本，并校验 source_hash 完整性。
func compileOverride(key string, version ActiveVersion, timeout time.Duration) (*CompiledPlugin, Issues) {
	hash := hashSource(version.SourceText)
	if version.SourceHash != "" && version.SourceHash != hash {
		// 完整性闸门：DB 中的 source_text 与上传时记录的 hash 不一致，
		// 说明内容被旁路修改——不执行、不激活（fail-closed）。
		return nil, Issues{issuef(key, "versions."+version.Version, "", "source_hash_mismatch",
			"source_hash 与 source_text 不一致（记录 %s，实际 %s）", version.SourceHash, hash)}
	}
	plugin, err := Compile(context.Background(), version.SourceText, CompileOptions{
		Timeout:     timeout,
		ExpectedKey: key,
		Layer:       LayerOverride,
	})
	if err != nil {
		return nil, asIssues(key, err)
	}
	return plugin, nil
}

// builtinRouteShapes 是内建 /v1 路由的保留形状集（复核 Important-2 / R-10）。
//
// 为什么需要：内建路由是 ServeMux 上的更具体模式，插件即使注册成功、进入
// RouteIndex，也永远收不到请求（ServeMux 优先精确/更具体模式）。这类
// 「注册成功但静默不可达」比加载期拒绝更糟——管理面显示 registered，排障时
// 却找不到任何错误。
//
// 维护契约（R-10）：任何任务在 /v1 面新增或删除内建路由（T2 的
// `POST /v1/responses`、T3 的 `GET /v1/responses/{id}` 与 compact、T4 的
// `/v1/videos`、`/v1/batches` 等），**必须**同步增删本集合；参数段统一写成
// `{}`，由 routeShape 的同一套段归一化与插件路由比较。集合只是「内建形状
// 登记表」：是否拒绝插件声明还要看 pluginDispatchedBuiltinShapes（处理器
// 是否先查 RouteIndex）与 builtinReservedRouteFamilies（通配族）。
//
// 当前条目与本仓库的注册点一一对应：
//   - relay_endpoints.go：chat/completions、models、responses（T2 的创建、
//     T3 的 compact）、GET responses 回放（T3 的产物检索，处理器在
//     handlers_tasks.go 注册）；
//   - handlers_tasks.go：tasks/{key} 及其产物族；
//   - handlers_multimodal.go：images/embeddings/rerank/audio/engines/models 通配/realtime。
var builtinRouteShapes = map[string]struct{}{
	"POST /v1/chat/completions":             {},
	"GET /v1/models":                        {},
	"POST /v1/responses":                    {},
	"GET /v1/responses/{}":                  {},
	"POST /v1/responses/compact":            {},
	"POST /v1/tasks/{}":                     {},
	"GET /v1/tasks/{}":                      {},
	"GET /v1/tasks/{}/artifacts":            {},
	"GET /v1/tasks/{}/artifacts/{}/content": {},
	// T4 / P-04：OpenAI Videos 与 Batches 是 tasks 数据面的纯内建薄封装。
	// 处理器不查 RouteIndex（R-10），故只进保留集、不进例外集。
	"POST /v1/videos":                {},
	"GET /v1/videos/{}":              {},
	"GET /v1/videos/{}/content":      {},
	"POST /v1/batches":               {},
	"GET /v1/batches/{}":             {},
	"POST /v1/batches/{}/cancel":     {},
	"POST /v1/images/generations":    {},
	"POST /v1/images/edits":          {},
	"POST /v1/embeddings":            {},
	"POST /v1/rerank":                {},
	"POST /v1/audio/speech":          {},
	"POST /v1/audio/transcriptions":  {},
	"POST /v1/audio/translations":    {},
	"POST /v1/engines/{}/embeddings": {},
	"POST /v1/models/{}":             {}, // ServeMux 通配 /v1/models/{path...}
	"POST /v1beta/models/{}":         {}, // ServeMux 通配 /v1beta/models/{path...}
	"GET /v1/realtime":               {},
}

// pluginDispatchedBuiltinShapes 是 builtinRouteShapes 中「处理器显式先查
// RouteIndex」的子集（P-01）。
//
// 为什么需要这个例外：`POST /v1/responses` 与 `GET /v1/responses/{id}` 的内建
// 处理器都先 Match 插件索引——命中即转任务插件（提交 / 查询），只有在未命中时
// 才走内建分支（前者 501 fail-closed，后者任务产物回放）。这类内建路由对插件
// **可达**，不能按「ServeMux 更具体模式会吞掉」的理由拒绝，否则 P-01 冻结的
// 插件分支永远注册不进来（整插件 route_conflict 失败）。
// 其余内建形状仍按保留集拒绝：它们没有插件优先的分发面，放行只会造成
// 静默不可达（`POST /v1/responses/compact` 即属此类：它是 fail-closed 的
// 独立计费口径，不查插件索引）。
var pluginDispatchedBuiltinShapes = map[string]struct{}{
	"POST /v1/responses":   {},
	"GET /v1/responses/{}": {},
}

// builtinReservedRouteFamilies 是内建 ServeMux 通配族的保留集合（R-11/R-12/R-14）。
//
// 为什么按「族根 + 其下全部路径」而不是形状相等：ServeMux 的
// `/v1/models/{path...}` 会吞掉该族下**任意**精确声明（`/v1/models/gpt-4o`、
// `/v1/models/x/y` …），而形状相等比较只挡得住 `/v1/models/{}` 这一种写法。
// 漏掉的精确声明会「注册成功但永远不可达」，与本文件的 fail-closed 口径冲突。
//
// 族根 `/v1/models`（所有方法）必须在族内（R-14）：注册 `POST /v1/models/{path...}`
// 会让 ServeMux 对 `/v1/models` 发 307 重定向到 `/v1/models/`，随后由多模态
// embeddings 处理器接管——插件声明的 `POST /v1/models` 永远收不到请求。
var builtinReservedRouteFamilies = []string{"/v1/models", "/v1beta/models"}

// builtinReservedFamilyFor 返回 route.Path 所属的内建保留族（无则空串）。
//
// 族根与「族根 + `/` + 任意后缀」都算命中；尾斜杠先归一，避免
// `/v1/models/` 被当成族外路径。只做字面比较：manifest 路径必须以 `/v1/`
// 开头（R-09），参数段（`{x}`/`:x`）出现在族内同样是要拒绝的形态。
func builtinReservedFamilyFor(path string) string {
	normalized := path
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	normalized = strings.TrimSuffix(normalized, "/")
	for _, family := range builtinReservedRouteFamilies {
		if normalized == family || strings.HasPrefix(normalized, family+"/") {
			return family
		}
	}
	return ""
}

// registerRoutes 把插件路由加入全局索引（形状表 + 可匹配索引）；冲突时返回
// 结构化错误。
//
// 冲突判定 = 方法 + 归一化路径形状（relay-06 §2.2）：`:id`、`*id`、`{id}`
// 形状相同，视为同一路由。先到先得（key 升序），后到插件整插件失败——
// 路由歧义没有安全的运行时消解方式。冲突必须在**注册期**解决而不是让
// RouteIndex 在匹配时猜：运行期消歧会让同一条请求在不同快照/不同节点上
// 命中不同插件，计费与产物归属随之分裂。
//
// 两个索引同批提交、同批回滚：routes 用于诊断/形状去重，routeIndex 供 HTTP
// 面分发；漏回滚任意一个都会让「已判定失败」的插件仍然可达或仍显示占用。
//
// 三类内建保留（判定顺序：前缀 → 形状）：
//   - 通配族（R-11/R-12/R-14）：族根及其下任意声明都不可达，一律拒绝；
//   - ServeMux 独占形状（R-10）：插件永远收不到，拒绝；
//   - 处理器先查 RouteIndex 的形状（P-01）：插件可达，只参与插件间去重。
func registerRoutes(index map[string]string, routeIndex *RouteIndex, plugin *CompiledPlugin) Issues {
	var issues Issues
	seen := make(map[string]struct{}, len(plugin.Manifest().Routes))
	for idx, route := range plugin.Manifest().Routes {
		shape := routeShape(route)
		// R-11/R-12/R-14：内建通配族按「族根 + 其下任意深度」保留（任意方法）。
		// 必须在形状比较之前判定——精确声明 `/v1/models/gpt-4o` 的形状并不等于
		// `/v1/models/{}`，只靠 map 比较会放行一条永远不可达的路由；族根
		// `/v1/models` 还会被 ServeMux 307 到 `/v1/models/` 交给多模态处理器。
		if family := builtinReservedFamilyFor(route.Path); family != "" {
			issues = append(issues, issuef(plugin.Key, routeFieldPath(idx), "", "route_conflict",
				"路由 %s 落在内建保留族 %s 下（内建模式优先，插件路由将不可达）", shape, family))
			continue
		}
		if _, reserved := builtinRouteShapes[shape]; reserved {
			// P-01 例外：处理器先查 RouteIndex 的内建形状对插件可达（见
			// pluginDispatchedBuiltinShapes），不得按「不可达」拒绝。
			if _, dispatched := pluginDispatchedBuiltinShapes[shape]; !dispatched {
				issues = append(issues, issuef(plugin.Key, routeFieldPath(idx), "", "route_conflict",
					"路由 %s 与内建路由冲突（内建模式优先，插件路由将不可达）", shape))
				continue
			}
		}
		if owner, dup := index[shape]; dup {
			issues = append(issues, issuef(plugin.Key, routeFieldPath(idx), "", "route_conflict",
				"路由 %s 已被插件 %q 注册", shape, owner))
			continue
		}
		if _, dup := seen[shape]; dup {
			issues = append(issues, issuef(plugin.Key, routeFieldPath(idx), "", "route_conflict",
				"同插件内路由 %s 重复", shape))
			continue
		}
		seen[shape] = struct{}{}
		index[shape] = plugin.Key
		routeIndex.add(plugin.Key, route)
	}
	if issues.HasErrors() {
		// 冲突插件已写入的部分索引必须回滚，否则会把「失败的插件」
		// 残留在全局路由表里。回滚后再统一报错。
		for shape, owner := range index {
			if owner == plugin.Key {
				delete(index, shape)
			}
		}
		routeIndex.removePlugin(plugin.Key)
		return issues
	}
	return nil
}

// routeShape 归一化「方法 + 路径形状」。
//
// 参数段（`:x` / `*x` / `{x}`）统一替换为 `{}`；空路径段保留以便 `/a//b` 与
// `/a/b` 不被误判为同一形状。段语法判据与 RouteIndex 共用
// routeSegmentParamName：注册期与运行期对同一段必须给出同一答案（Important-1）。
func routeShape(route Route) string {
	trimmed := strings.Trim(route.Path, "/")
	if trimmed == "" {
		return route.Method + " /"
	}
	parts := strings.Split(trimmed, "/")
	for idx, part := range parts {
		if routeSegmentParamName(part) != "" {
			parts[idx] = "{}"
		}
	}
	return route.Method + " /" + strings.Join(parts, "/")
}

// registerChannelTypes 强制渠道类型与插件一一对应（relay-06 §1.1、§2.2）。
//
// 60/61 已在解码期拒绝；这里处理跨插件复用（同一渠道类型只能声明一次）。
func registerChannelTypes(owners map[int]string, plugin *CompiledPlugin) Issues {
	var issues Issues
	for idx, channelType := range plugin.Manifest().ChannelTypes {
		if owner, dup := owners[channelType]; dup {
			issues = append(issues, issuef(plugin.Key, "meta.channelTypes["+itoa(idx)+"]", "", "channel_type_conflict",
				"渠道类型 %d 已被插件 %q 声明", channelType, owner))
			continue
		}
		owners[channelType] = plugin.Key
	}
	if issues.HasErrors() {
		for channelType, owner := range owners {
			if owner == plugin.Key {
				delete(owners, channelType)
			}
		}
		return issues
	}
	return nil
}

// asIssues 把任意 error 收敛为 Issues（编译入口保证类型正确，兜底防漏）。
func asIssues(key string, err error) Issues {
	if issues, ok := err.(Issues); ok {
		return issues
	}
	return Issues{issuef(key, "", "", "internal", "%v", err)}
}

// sortedKeys 返回 map 的升序 key（泛型便于处理 string/int 两种索引）。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
