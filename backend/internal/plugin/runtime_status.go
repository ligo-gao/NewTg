package plugin

// 插件注册表热刷新与逐副本运行时状态（relay-06 §2.3/§2.5、modules-08 F-08-36）。
//
// 为什么把协调器放在 plugin 包：Manager 的快照、generation 与逐插件状态都在
// 这里，协调器只是「按 30s 与 Redis 失效信号驱动 Reload，并把 Status 投影到
// 共享观测面」的薄壳。放到 platform 会让 SQL loader 与快照语义分居两层，
// 测试也不得不同时拉起 PG/Redis 才能验证一次 Reload。
//
// Redis 故障的边界：DB 是权威真源，Redis 只承担「加速副本收敛」与「实例
// 观测」。因此任何 Redis 错误都只 Warn，绝不返回给写路径、绝不让 ticker
// 停止——30s 轮询本身就是 Redis 不可用时的兜底路径。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// PluginChangedChannel 是管理端写成功后广播「注册表已变化」的固定频道。
	// 与配置中心的 newtg:cfg:changed 分开：插件失效只应触发插件 Reload，
	// 不需要配置中心整表重读。
	PluginChangedChannel = "newtg:plugin:changed"

	// RuntimeStatusKeyPrefix 是逐副本运行时状态的 Redis key 前缀。
	// 形状：newtg:plugin:runtime:<instance_id>（Hash）。
	RuntimeStatusKeyPrefix = "newtg:plugin:runtime:"

	// RuntimeStatusInterval 是重载与状态写入周期（relay-06 §2.5：30s）。
	RuntimeStatusInterval = 30 * time.Second
	// RuntimeStatusTTL 是状态 key 的存活时间。90s = 3 个周期：单次丢写不会
	// 让副本从聚合里消失；连续 3 次失败才视为失联（与 F-08-21 同口径）。
	RuntimeStatusTTL = 90 * time.Second

	// runtimeRedisTimeout 限制单次 Redis 观测/发布操作。Redis 抖动时读端点
	// 与写后广播必须快速失败并降级，不能把管理面写请求挂住。
	runtimeRedisTimeout = 2 * time.Second
	// runtimeWarnInterval 避免 Redis 长时间不可用时每 30s 刷屏。
	runtimeWarnInterval = 5 * time.Minute
)

// RuntimeInstanceStatus 是运行时状态聚合中的一行（一个未过期副本）。
//
// 字段全部来自 RuntimeStatus 与 Redis Hash 的非秘密元数据：不含源文本、
// 不含上游 URL、不含任何凭据。generation 仍是节点本地号，跨节点比较用
// Revision（relay-06 §2.5）。
type RuntimeInstanceStatus struct {
	InstanceID string                `json:"instance_id"`
	Node       string                `json:"node"`
	Role       string                `json:"role"`
	Version    string                `json:"version"`
	Local      bool                  `json:"local"`
	StartedAt  time.Time             `json:"started_at"`
	LastSeen   time.Time             `json:"last_seen"`
	Generation uint64                `json:"generation"`
	Revision   string                `json:"revision"`
	ReloadedAt time.Time             `json:"reloaded_at"`
	ReloadErr  string                `json:"reload_error,omitempty"`
	Plugins    []PluginRuntimeStatus `json:"plugins"`
}

// RuntimeAggregate 是 `GET /api/plugin/task/runtime/status` 的响应。
//
// AggregateUnavailable=true 表示 Redis 聚合不可用，此时 Instances 只含
// 本地副本——不是空列表，也不是 5xx：管理面仍应能看到「本副本是否健康」。
type RuntimeAggregate struct {
	Instances            []RuntimeInstanceStatus `json:"instances"`
	AggregateUnavailable bool                    `json:"aggregate_unavailable"`
}

// RuntimeStatusKey 返回逐副本运行时状态的 Redis key。
func RuntimeStatusKey(instanceID string) string {
	return RuntimeStatusKeyPrefix + instanceID
}

// LocalRuntimeInstance 把本进程的 RuntimeStatus 投影成聚合行。
// instanceID 为空时退回 status.Node，便于未配置 INSTANCE_ID 的装配/测试。
func LocalRuntimeInstance(status RuntimeStatus, instanceID, role, version string,
	startedAt, lastSeen time.Time) RuntimeInstanceStatus {

	status = sanitizeRuntimeStatus(status)
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		instanceID = status.Node
	}
	if lastSeen.IsZero() {
		lastSeen = startedAt
	}
	return RuntimeInstanceStatus{
		InstanceID: instanceID,
		Node:       status.Node,
		Role:       role,
		Version:    version,
		Local:      true,
		StartedAt:  startedAt.UTC(),
		LastSeen:   lastSeen.UTC(),
		Generation: status.Generation,
		Revision:   status.Revision,
		ReloadedAt: status.ReloadedAt,
		ReloadErr:  status.ReloadError,
		Plugins:    status.Plugins,
	}
}

// AggregateRuntimeInstances 合并 Redis 中所有未过期副本与本地副本。
//
// 本地副本永远覆盖 Redis 中的同 id 行：本进程刚写入的 Status 是最新事实，
// 而 Redis 行可能滞后一个周期（或写入失败）。rdb 为 nil 或读取失败时返回
// 本地副本并置 AggregateUnavailable。
func AggregateRuntimeInstances(ctx context.Context, rdb *redis.Client,
	local RuntimeInstanceStatus, now func() time.Time) RuntimeAggregate {

	if now == nil {
		now = time.Now
	}
	local.Local = true
	if local.LastSeen.IsZero() {
		local.LastSeen = now().UTC()
	}
	out := RuntimeAggregate{Instances: []RuntimeInstanceStatus{local}}
	if rdb == nil {
		out.AggregateUnavailable = true
		return out
	}

	entries, err := loadRuntimeEntries(ctx, rdb)
	if err != nil {
		out.AggregateUnavailable = true
		return out
	}
	byID := make(map[string]RuntimeInstanceStatus, len(entries)+1)
	for id, fields := range entries {
		instance, ok := decodeRuntimeEntry(id, fields)
		if !ok {
			// 部分写入/损坏行跳过：聚合是观测面，不应因一行坏数据整体失败。
			continue
		}
		byID[id] = instance
	}
	byID[local.InstanceID] = local

	out.Instances = out.Instances[:0]
	for _, instance := range byID {
		out.Instances = append(out.Instances, instance)
	}
	sort.Slice(out.Instances, func(i, j int) bool {
		return out.Instances[i].InstanceID < out.Instances[j].InstanceID
	})
	return out
}

// loadRuntimeEntries 读取全部未过期运行时 key（SCAN + HGETALL）。
//
// 为什么不用集合索引：集合成员不会随 key 过期自动移除；SCAN + TTL 是无
// 维护者的最终一致方案（与实例心跳同一取舍）。副本数是个位到几十。
func loadRuntimeEntries(ctx context.Context, rdb *redis.Client) (map[string]map[string]string, error) {
	readCtx, cancel := context.WithTimeout(ctx, runtimeRedisTimeout)
	defer cancel()

	seen := make(map[string]struct{})
	entries := make(map[string]map[string]string)
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(readCtx, cursor, RuntimeStatusKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			values, err := rdb.HGetAll(readCtx, key).Result()
			if err != nil {
				return nil, err
			}
			if len(values) == 0 {
				// SCAN 与 HGETALL 之间过期：跳过而不是产出空行。
				continue
			}
			id := strings.TrimPrefix(key, RuntimeStatusKeyPrefix)
			if id == "" {
				continue
			}
			entries[id] = values
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return entries, nil
}

// decodeRuntimeEntry 把一行 Hash 还原成聚合行。status 缺失/损坏时返回 false。
func decodeRuntimeEntry(id string, fields map[string]string) (RuntimeInstanceStatus, bool) {
	var status RuntimeStatus
	if err := json.Unmarshal([]byte(fields["status"]), &status); err != nil {
		return RuntimeInstanceStatus{}, false
	}
	status = sanitizeRuntimeStatus(status)
	instance := RuntimeInstanceStatus{
		InstanceID: id,
		Node:       status.Node,
		Role:       fields["role"],
		Version:    fields["version"],
		Generation: status.Generation,
		Revision:   status.Revision,
		ReloadedAt: status.ReloadedAt,
		ReloadErr:  status.ReloadError,
		Plugins:    status.Plugins,
	}
	if started, err := time.Parse(time.RFC3339Nano, fields["started_at"]); err == nil {
		instance.StartedAt = started.UTC()
	}
	if seenAt, err := time.Parse(time.RFC3339Nano, fields["last_seen"]); err == nil {
		instance.LastSeen = seenAt.UTC()
	}
	return instance, true
}

// RuntimeCoordinatorConfig 是热刷新/状态循环的装配输入。
type RuntimeCoordinatorConfig struct {
	Manager *Manager
	// Redis 为 nil 时退化为「本地单副本」模式：ticker 仍每 30s Reload，
	// 但不写状态、不订阅、聚合返回 aggregate_unavailable=true。
	Redis      *redis.Client
	InstanceID string
	Node       string
	Role       string
	Version    string
	StartedAt  time.Time
	// Interval 覆盖 30s 周期（测试注入短周期）。
	Interval time.Duration
	// TTL 覆盖 90s 状态存活时间（测试注入短 TTL）。
	TTL    time.Duration
	Logger *slog.Logger
	// Now 是时间接缝；nil 时 time.Now。
	Now func() time.Time
	// RefreshDisabledFactoryKeys 在每次 Reload 前重算内置禁用集合。
	// B-11 的 enabled=false 不会出现在 active 版本加载结果里，若只在启动
	// 时算一次，其它副本收到 pub/sub 后 Reload 仍会用旧禁用集——「管理面
	// 已停用、另一副本仍注册」的状态分裂。nil 时跳过（纯内存测试）。
	RefreshDisabledFactoryKeys func(context.Context) ([]string, error)
}

// RuntimeCoordinator 驱动插件快照热刷新并维护逐副本状态。
//
// 并发模型：Start 只允许一次；Reload 由 Manager 串行化；Stop 取消循环、
// 等待退出后 DEL 本实例 key（停机立即从聚合消失，而不是等 TTL）。
type RuntimeCoordinator struct {
	manager    *Manager
	rdb        *redis.Client
	instanceID string
	role       string
	version    string
	startedAt  time.Time
	interval   time.Duration
	ttl        time.Duration
	now        func() time.Time
	log        *slog.Logger
	refresh    func(context.Context) ([]string, error)

	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// subReady 在首次订阅确认后关闭：测试据此在 publish 前确认对端已订阅
	// （生产不等待，避免 Redis 故障拖慢启动）。
	subReady     chan struct{}
	subReadyOnce sync.Once

	warnMu   sync.Mutex
	lastWarn time.Time
}

// NewRuntimeCoordinator 校验并构造协调器。Redis 缺失不是错误（本地模式）。
func NewRuntimeCoordinator(cfg RuntimeCoordinatorConfig) (*RuntimeCoordinator, error) {
	if cfg.Manager == nil {
		return nil, errors.New("plugin: runtime coordinator requires a manager")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = RuntimeStatusInterval
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = RuntimeStatusTTL
	}
	instanceID := strings.TrimSpace(cfg.InstanceID)
	if instanceID == "" {
		instanceID = strings.TrimSpace(cfg.Node)
	}
	if instanceID == "" {
		instanceID = "local"
	}
	role := strings.TrimSpace(cfg.Role)
	if role == "" {
		role = "gateway"
	}
	startedAt := cfg.StartedAt
	if startedAt.IsZero() {
		startedAt = now()
	}
	return &RuntimeCoordinator{
		manager: cfg.Manager, rdb: cfg.Redis,
		instanceID: instanceID, role: role, version: cfg.Version,
		startedAt: startedAt.UTC(), interval: interval, ttl: ttl,
		now: now, log: logger, refresh: cfg.RefreshDisabledFactoryKeys,
		subReady: make(chan struct{}),
	}, nil
}

// Start 立即执行一次 Reload + 状态写入，然后启动周期与订阅循环。
// 重复调用安全（只生效一次）。Redis 故障不会阻止启动。
func (c *RuntimeCoordinator) Start(parent context.Context) {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		c.cancel = cancel
		// 启动即 Reload：装配层可能已 Reload 过，这里再次执行是幂等的
		// （内容签名不变不会递增 generation），同时保证装配层漏调时仍收敛。
		c.reloadAndReport(ctx)
		c.wg.Add(2)
		go func() {
			defer c.wg.Done()
			c.runTicker(ctx)
		}()
		go func() {
			defer c.wg.Done()
			c.runSubscription(ctx)
		}()
	})
}

// Stop 取消循环、等待退出并删除本实例状态 key。
//
// 删除使用独立超时上下文：停机时父 ctx 已取消，复用它会跳过 DEL，实例会
// 在 TTL 内继续显示为存活。
func (c *RuntimeCoordinator) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
	})
	c.wg.Wait()
	c.remove()
}

// PublishPluginChanged 广播注册表失效。Redis 未装配/不可用时返回错误由
// 调用方 Warn 并继续响应——写路径的成功事实在 DB，不依赖广播。
func (c *RuntimeCoordinator) PublishPluginChanged(ctx context.Context) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	pubCtx, cancel := context.WithTimeout(ctx, runtimeRedisTimeout)
	defer cancel()
	return c.rdb.Publish(pubCtx, PluginChangedChannel, c.instanceID).Err()
}

// Aggregate 返回全部未过期副本的运行时状态（本地副本永远在内）。
func (c *RuntimeCoordinator) Aggregate(ctx context.Context) RuntimeAggregate {
	if c == nil {
		return RuntimeAggregate{AggregateUnavailable: true}
	}
	local := c.localInstance()
	return AggregateRuntimeInstances(ctx, c.rdb, local, c.now)
}

// localInstance 生成本进程当前状态行。
func (c *RuntimeCoordinator) localInstance() RuntimeInstanceStatus {
	status := sanitizeRuntimeStatus(c.manager.Status())
	return LocalRuntimeInstance(status, c.instanceID, c.role, c.version,
		c.startedAt, c.now())
}

func (c *RuntimeCoordinator) runTicker(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reloadAndReport(ctx)
		}
	}
}

// reloadAndReport 是一次周期/失效触发的完整动作：刷新禁用集 → Reload →
// 写状态。RefreshDisabledFactoryKeys 失败时保留旧禁用集继续 Reload，避免
// 一次 DB 抖动把内置插件全部重新启用（fail-open）。
func (c *RuntimeCoordinator) reloadAndReport(ctx context.Context) {
	if c.refresh != nil {
		keys, err := c.refresh(ctx)
		if err != nil {
			c.warn(ctx, "plugin runtime: refresh disabled factory keys failed", err)
		} else {
			c.manager.SetDisabledFactoryKeys(keys)
		}
	}
	if err := c.manager.Reload(ctx); err != nil {
		// Reload 自己保留旧快照；这里只记录，ticker 下个周期会重试。
		c.warn(ctx, "plugin runtime: reload failed", err)
	}
	c.writeStatus(ctx)
}

// writeStatus 写入 newtg:plugin:runtime:<instance_id>（Hash + TTL）。
//
// HSet 与 Expire 放同一 pipeline：若两次命令之间进程被杀，只有 HSet 落地
// 的 key 没有 TTL，会永久留在聚合里。
func (c *RuntimeCoordinator) writeStatus(ctx context.Context) {
	if c.rdb == nil {
		return
	}
	status := sanitizeRuntimeStatus(c.manager.Status())
	raw, err := json.Marshal(status)
	if err != nil {
		c.warn(ctx, "plugin runtime: marshal status failed", err)
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, runtimeRedisTimeout)
	defer cancel()
	key := RuntimeStatusKey(c.instanceID)
	pipe := c.rdb.TxPipeline()
	pipe.HSet(writeCtx, key, map[string]interface{}{
		"status":     string(raw),
		"version":    c.version,
		"role":       c.role,
		"started_at": c.startedAt.Format(time.RFC3339Nano),
		"last_seen":  c.now().UTC().Format(time.RFC3339Nano),
	})
	pipe.Expire(writeCtx, key, c.ttl)
	if _, err := pipe.Exec(writeCtx); err != nil {
		c.warn(ctx, "plugin runtime: write status failed", err)
	}
}

// runSubscription 订阅失效频道，断线后退避重连。
//
// 不用 pubsub.Channel 的默认阻塞循环作为唯一路径：Redis 重启后 pubsub 会
// 关闭，必须重建订阅，否则「订阅还活着」的错觉会让副本永久等不到广播
// （只能靠 30s ticker 兜底）。重连间隔复用 interval。
func (c *RuntimeCoordinator) runSubscription(ctx context.Context) {
	for ctx.Err() == nil {
		if c.rdb == nil {
			<-ctx.Done()
			return
		}
		pubsub, err := c.subscribe(ctx)
		if err != nil {
			c.warn(ctx, "plugin runtime: subscribe failed", err)
			if !c.waitRetry(ctx) {
				return
			}
			continue
		}
		// 订阅已由服务端确认：通知等待方（测试用）可以安全发布。
		c.subReadyOnce.Do(func() { close(c.subReady) })
		if !c.consume(ctx, pubsub) {
			_ = pubsub.Close()
			return
		}
		_ = pubsub.Close()
		if !c.waitRetry(ctx) {
			return
		}
	}
}

// waitSubscribed 等待首次订阅确认（仅测试/诊断使用）。Redis 不可用时返回
// ctx 错误，调用方不应把它当成启动失败。
func (c *RuntimeCoordinator) waitSubscribed(ctx context.Context) error {
	if c == nil {
		return errors.New("plugin: nil runtime coordinator")
	}
	select {
	case <-c.subReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RuntimeCoordinator) subscribe(ctx context.Context) (*redis.PubSub, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, runtimeRedisTimeout)
	defer cancel()
	pubsub := c.rdb.Subscribe(attemptCtx, PluginChangedChannel)
	// Receive 等待服务端 SUBSCRIBE 确认，确保 Subscribe 返回后消息不会漏收。
	if _, err := pubsub.Receive(attemptCtx); err != nil {
		_ = pubsub.Close()
		return nil, err
	}
	return pubsub, nil
}

// consume 消费订阅消息直到 ctx 取消或频道关闭；返回 false 表示 ctx 已结束。
func (c *RuntimeCoordinator) consume(ctx context.Context, pubsub *redis.PubSub) bool {
	messages := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return false
		case _, ok := <-messages:
			if !ok {
				return true
			}
			// payload 只是发布者 instance id（诊断用）；任何消息都触发 Reload。
			c.reloadAndReport(ctx)
		}
	}
}

// waitRetry 等待一个周期或 ctx 结束；返回 false 表示 ctx 已结束。
func (c *RuntimeCoordinator) waitRetry(ctx context.Context) bool {
	timer := time.NewTimer(c.interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *RuntimeCoordinator) remove() {
	if c.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeRedisTimeout)
	defer cancel()
	if err := c.rdb.Del(ctx, RuntimeStatusKey(c.instanceID)).Err(); err != nil {
		c.log.Warn("plugin runtime: delete status on shutdown failed",
			"error", err, "instance", c.instanceID)
	}
}

// warn 对同类 Redis 故障按 5 分钟节流。
func (c *RuntimeCoordinator) warn(ctx context.Context, msg string, err error) {
	if ctx.Err() != nil {
		return
	}
	now := c.now()
	c.warnMu.Lock()
	last := c.lastWarn
	if !last.IsZero() && now.Sub(last) < runtimeWarnInterval {
		c.warnMu.Unlock()
		return
	}
	c.lastWarn = now
	c.warnMu.Unlock()
	c.log.Warn(msg, "error", err, "instance", c.instanceID)
}

// redaction 覆盖运行时诊断文本里可能出现的凭据与 URL。
//
// 为什么在输出前脱敏而不是信任来源：reload_error 包装的是任意 DB/驱动错误，
// 某些驱动会回显连接串；编译错误也可能包含作者写的 URL。F-08-36 的响应
// 要求不含凭据/URL/源码，因此观测面统一做最后一道闸门。
var (
	runtimeCredentialPattern = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|authorization)\b\s*[=:]\s*[^\s,;]+`)
	runtimeURLPattern        = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"']+`)
)

func sanitizeRuntimeStatus(status RuntimeStatus) RuntimeStatus {
	status.ReloadError = redactRuntimeText(status.ReloadError)
	if len(status.Plugins) == 0 {
		return status
	}
	plugins := make([]PluginRuntimeStatus, len(status.Plugins))
	copy(plugins, status.Plugins)
	for i := range plugins {
		if len(plugins[i].Errors) == 0 {
			continue
		}
		errors := make(Issues, len(plugins[i].Errors))
		copy(errors, plugins[i].Errors)
		for j := range errors {
			errors[j].Message = redactRuntimeText(errors[j].Message)
		}
		plugins[i].Errors = errors
	}
	status.Plugins = plugins
	return status
}

func redactRuntimeText(text string) string {
	if text == "" {
		return ""
	}
	text = runtimeCredentialPattern.ReplaceAllString(text, "$1=[redacted]")
	text = runtimeURLPattern.ReplaceAllString(text, "[redacted-url]")
	return text
}

// RuntimeAggregateForManager 是未装配协调器时的降级投影（单副本）。
// 供管理端点在没有 Redis 的纯内存装配/单测中保持响应形状一致。
func RuntimeAggregateForManager(manager *Manager, instanceID, role, version string, now func() time.Time) RuntimeAggregate {
	if now == nil {
		now = time.Now
	}
	if manager == nil {
		return RuntimeAggregate{AggregateUnavailable: true}
	}
	status := manager.Status()
	local := LocalRuntimeInstance(status, instanceID, role, version, status.ReloadedAt, now())
	return AggregateRuntimeInstances(context.Background(), nil, local, now)
}
