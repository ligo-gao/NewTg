package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
)

// 本文件是 `async_task_poll` 的后台轮询器（relay-06 §8）与最小补偿扫描。
//
// 调度语义冻结（brief）：
//   - 周期 15s；只处理 state NOT IN ('success','failure')；
//   - 同一渠道内逐任务串行，任务之间间隔 1s（可注入，生产 1s）；
//   - 外层超时 submit_time + TASK_TIMEOUT_MINUTES(1440) 或连续失败
//     TASK_POLL_MAX_FAILURES(20) → FAILURE + 退款。
//
// 正确性锚点：终态推进走 `ApplyPoll` 的 (id, fromState) CAS，只有 CAS 赢家
// 触发结算/退款，保证「终态恰好一次」。非终态推进也走 CAS，失败即放弃本轮
// ——另一个副本已经推进过，重复写入没有意义。

const (
	// DefaultPollInterval 是后台轮询周期（设计 §8.1 冻结 15s）。
	DefaultPollInterval = 15 * time.Second
	// DefaultTaskGap 是同渠道任务之间的串行间隔（设计 §8.1 冻结 1s）。
	DefaultTaskGap = 1 * time.Second
	// DefaultQueryLimit 是单轮扫描的未完成任务上限（TASK_QUERY_LIMIT=1000）。
	DefaultQueryLimit = 1000
	// DefaultMaxPollFailures 是连续失败上限（TASK_POLL_MAX_FAILURES=20；0=禁用）。
	DefaultMaxPollFailures = 20
	// DefaultTaskTimeout 是任务执行超时（TASK_TIMEOUT_MINUTES=1440；0=禁用）。
	//
	// 与 relay-06 §8.3 的注记一致：这是**插件任务执行**超时，与 MJ 的 1h、
	// MaxRequestTaskDurationSeconds 的计费乘数上界都不是同一语义，不得互引。
	DefaultTaskTimeout = 1440 * time.Minute
	// DefaultPluginReloadInterval 是插件快照刷新周期。
	//
	// 轮询器必须看到管理端新激活的版本，否则「插件修好了但任务还在用旧
	// 解析器」会一直解析失败到超时退款。30s 与 T3 登记的轮询周期同值。
	DefaultPluginReloadInterval = 30 * time.Second
	// DefaultReconcileLimit 是单轮补偿扫描的行数上限。
	DefaultReconcileLimit = 200
)

// PluginSnapshotter 是轮询器对插件运行时的只读依赖（生产 *plugin.Manager）。
type PluginSnapshotter interface {
	Snapshot() *plugin.Snapshot
	Reload(ctx context.Context) error
}

// PendingRepo 是轮询/补偿所需的存储面（生产 *Repo）。
type PendingRepo interface {
	ListPending(ctx context.Context, limit int) ([]*Task, error)
	ListReconcilable(ctx context.Context, limit int) ([]*Task, error)
	GetChannel(ctx context.Context, id int64) (*ChannelTarget, error)
	ApplyPoll(ctx context.Context, id int64, from State, u PollUpdate) (bool, error)
	UpdateBilling(ctx context.Context, id int64, quota *int64, private map[string]any) error
}

// BillingHandoff 是计费侧的异步交接收口（contract.BillingHandoff 的两个
// ByRequestID 方法；Detach 属于提交路径，轮询器不需要）。
type BillingHandoff interface {
	TerminateByRequestID(requestID string, fact contract.UsageFact) error
	RefundByRequestID(requestID string) error
}

// PollerConfig 是轮询器的依赖与可调参数。零值字段使用冻结默认值。
type PollerConfig struct {
	Repo    PendingRepo
	Plugins PluginSnapshotter
	Billing BillingHandoff
	// Decrypt 解开渠道 credentials 密文并返回 api_key；生产由 platform 用
	// 与渠道管理同一个 SecretSealer 装配。
	Decrypt func(sealed []byte) (string, error)
	Client  *http.Client
	Logger  *slog.Logger

	// 测试接缝：Now / Sleep 可注入；生产 Now=time.Now、Sleep=定时器。
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	Interval       time.Duration
	TaskGap        time.Duration
	QueryLimit     int
	MaxFailures    int
	Timeout        time.Duration
	ReloadInterval time.Duration
	ReconcileLimit int
	// AllowPrivate 只允许测试注入 httptest（127.0.0.1）时置 true。
	AllowPrivate bool
}

// Poller 持有轮询依赖，可安全并发调用 Run 之外的单轮方法（测试）。
type Poller struct {
	repo    PendingRepo
	plugins PluginSnapshotter
	billing BillingHandoff
	decrypt func([]byte) (string, error)
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	interval       time.Duration
	taskGap        time.Duration
	queryLimit     int
	maxFailures    int
	timeout        time.Duration
	reloadInterval time.Duration
	reconcileLimit int
	allowPrivate   bool
}

// NewPoller 构造轮询器。Repo/Plugins/Billing 是硬依赖：缺任一无法推进任务，
// 静默降级只会让预扣永久冻结。
func NewPoller(cfg PollerConfig) (*Poller, error) {
	if cfg.Repo == nil || cfg.Plugins == nil || cfg.Billing == nil {
		return nil, errors.New("tasks: poller requires repo, plugins and billing handoff")
	}
	p := &Poller{
		repo:    cfg.Repo,
		plugins: cfg.Plugins,
		billing: cfg.Billing,
		decrypt: cfg.Decrypt,
		client:  cfg.Client,
		log:     cfg.Logger,
		now:     cfg.Now,
		sleep:   cfg.Sleep,
	}
	if p.client == nil {
		p.client = NewOutboundClient()
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = sleepContext
	}
	p.interval = orDefaultDuration(cfg.Interval, DefaultPollInterval)
	p.taskGap = orDefaultDuration(cfg.TaskGap, DefaultTaskGap)
	p.reloadInterval = orDefaultDuration(cfg.ReloadInterval, DefaultPluginReloadInterval)
	p.queryLimit = orDefaultInt(cfg.QueryLimit, DefaultQueryLimit)
	p.reconcileLimit = orDefaultInt(cfg.ReconcileLimit, DefaultReconcileLimit)
	p.maxFailures = cfg.MaxFailures
	if p.maxFailures == 0 {
		p.maxFailures = DefaultMaxPollFailures
	}
	p.timeout = cfg.Timeout
	if p.timeout == 0 {
		p.timeout = DefaultTaskTimeout
	}
	p.allowPrivate = cfg.AllowPrivate
	return p, nil
}

// Run 运行轮询循环直到 ctx 取消（停机不是错误）。
func (p *Poller) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	p.runCycle(ctx)
	pollTicker := time.NewTicker(p.interval)
	defer pollTicker.Stop()
	reloadTicker := time.NewTicker(p.reloadInterval)
	defer reloadTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTicker.C:
			p.runCycle(ctx)
		case <-reloadTicker.C:
			if err := p.plugins.Reload(ctx); err != nil {
				p.log.Warn("tasks: plugin reload failed; polling with previous snapshot", "error", err)
			}
		}
	}
}

// RunOnce 执行一轮轮询 + 补偿（测试与运维手动触发用）。
func (p *Poller) RunOnce(ctx context.Context) {
	p.runCycle(ctx)
}

func (p *Poller) runCycle(ctx context.Context) {
	p.pollOnce(ctx)
	p.reconcileOnce(ctx)
}

// pollOnce 拉取未完成任务并按渠道分组串行处理。
func (p *Poller) pollOnce(ctx context.Context) {
	tasks, err := p.repo.ListPending(ctx, p.queryLimit)
	if err != nil {
		// 扫描失败 ≠ 无未完成任务：不更新任何水位，下一轮重试。
		p.log.Error("tasks: list pending tasks failed; retrying next round", "error", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	// 同渠道内串行是冻结语义（relay-06 §8.1）：同一渠道的上游限流/连接
	// 复用由单线程顺序消化，避免并发轮询把渠道打出 429。
	channelOrder := make([]int64, 0, len(tasks))
	byChannel := make(map[int64][]*Task)
	for _, task := range tasks {
		id := taskChannelID(task)
		if _, seen := byChannel[id]; !seen {
			channelOrder = append(channelOrder, id)
		}
		byChannel[id] = append(byChannel[id], task)
	}
	for _, channelID := range channelOrder {
		group := byChannel[channelID]
		for i, task := range group {
			if ctx.Err() != nil {
				return
			}
			if i > 0 {
				if err := p.sleep(ctx, p.taskGap); err != nil {
					return
				}
			}
			p.processTask(ctx, task)
		}
	}
}

// processTask 推进单个任务：超时检查 → 插件构建查询 → 出站 → 分类处理。
func (p *Poller) processTask(ctx context.Context, task *Task) {
	if task == nil || task.State.Terminal() {
		return
	}
	if p.timedOut(task) {
		p.failAndRefund(ctx, task, "task_timeout")
		return
	}
	channelID := taskChannelID(task)
	if channelID == 0 {
		p.recordFailure(ctx, task, "channel_missing")
		return
	}
	channel, err := p.repo.GetChannel(ctx, channelID)
	if err != nil {
		// 渠道被删/读失败：累计失败（最终超时退款），不立即判失败——
		// 瞬时 DB 抖动不应该把还在上游成功产出的任务判死。
		p.recordFailure(ctx, task, "channel_lookup_failed")
		return
	}
	compiled, ok := p.plugins.Snapshot().Plugin(task.PluginKey)
	if !ok {
		// 插件被禁用/卸载：与 401/403 同向——状态不变、累计失败、不退款。
		p.recordFailure(ctx, task, "plugin_unavailable")
		return
	}
	if !compiled.HasHook("buildQueryRequest") || !compiled.HasHook("parseTaskResult") {
		p.recordFailure(ctx, task, "plugin_hook_missing")
		return
	}
	apiKey := ""
	if p.decrypt != nil && len(channel.Credentials) > 0 {
		key, decErr := p.decrypt(channel.Credentials)
		if decErr != nil {
			p.recordFailure(ctx, task, "channel_credential_invalid")
			return
		}
		apiKey = key
	}
	queryCtx := buildQueryContext(task, channel, apiKey)
	rawDesc, err := compiled.Call(ctx, "buildQueryRequest", queryCtx)
	if err != nil {
		p.recordFailure(ctx, task, "query_request_failed")
		return
	}
	desc, err := DecodeRequestDescriptor(rawDesc)
	if err != nil {
		p.recordFailure(ctx, task, "query_request_invalid")
		return
	}
	resp, err := DoOutbound(ctx, p.client, desc, AllowedHosts(channel.BaseURL, compiled.Manifest().AllowedHosts), p.allowPrivate)
	if err != nil {
		// 传输错误（含超时/白名单拒绝/重定向）：累计失败，最终超时退款。
		p.recordFailure(ctx, task, "query_transport_failed")
		return
	}
	p.classifyResponse(ctx, task, compiled, queryCtx, resp)
}

// classifyResponse 按 relay-06 §8.2 的冻结表分类上游响应。
func (p *Poller) classifyResponse(ctx context.Context, task *Task, compiled *plugin.CompiledPlugin, queryCtx map[string]any, resp *ResponseSnapshot) {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		// 2xx 但解析失败/未识别也必须累计：否则一个永远返回坏 JSON 的
		// 上游会让任务停在非终态并在 1440 分钟后才超时退款。
		if !p.applyParsed(ctx, task, compiled, queryCtx, resp) {
			p.recordFailure(ctx, task, "parse_failed")
		}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// 404/410：上游明确表示任务不存在/已丢失 → 立即失败并退款。
		p.failAndRefund(ctx, task, "upstream_task_not_found")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 401/403：**状态不变、不退款**，只累计 + 告警（乙类怪癖，逐条保留）。
		// 渠道凭据可能只是临时失效，退款会让「恢复后任务其实成功」变成白送。
		p.log.Warn("tasks: poll rejected by upstream auth; task state unchanged and no refund",
			"task_id", task.PublicTaskID, "plugin", task.PluginKey,
			"status", resp.StatusCode)
		p.recordFailure(ctx, task, "upstream_auth")
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		p.recordFailure(ctx, task, "upstream_retryable")
	default:
		// 其它 4xx：先尝试解析；仍非终态则视为未识别，累计失败。
		if p.applyParsed(ctx, task, compiled, queryCtx, resp) {
			return
		}
		p.recordFailure(ctx, task, "upstream_unrecognized")
	}
}

// applyParsed 调用 parseTaskResult 并推进任务。返回 false 表示解析失败/未识别
// （调用方决定是否累计失败）。
func (p *Poller) applyParsed(ctx context.Context, task *Task, compiled *plugin.CompiledPlugin, queryCtx map[string]any, resp *ResponseSnapshot) bool {
	rawParsed, err := compiled.Call(ctx, "parseTaskResult", queryCtx, resp.Body, map[string]any{
		"status":  resp.StatusCode,
		"headers": resp.Headers,
	})
	if err != nil {
		p.log.Warn("tasks: parseTaskResult failed", "task_id", task.PublicTaskID, "status", resp.StatusCode, "error", err)
		return false
	}
	parsed, ok := rawParsed.(map[string]any)
	if !ok {
		return false
	}
	rawState := toString(parsed["state"])
	state := ParseState(rawState)
	if state == StateUnknown {
		// UNKNOWN 是合法持久化态但**不是终态**：计入连续失败（relay-06 §6.4）。
		// 返回 true 表示「已处理」——失败计数在 applyUnknown 里做过，
		// 其它 4xx 分支不得再累加一次。
		p.applyUnknown(ctx, task, rawState, p.mergedData(task, parsed, compiled))
		return true
	}

	progress := task.ProgressPercent
	if v, ok := parseProgress(parsed["progress"]); ok {
		progress = v
	} else if v, ok := parseProgress(parsed["progressPercent"]); ok {
		progress = v
	}
	data := p.mergedData(task, parsed, compiled)
	if state.Terminal() {
		reason := toString(parsed["failReason"])
		data = p.extractCompletionUsage(ctx, task, compiled, queryCtx, parsed, data)
		p.finalize(ctx, task, state, rawState, progress, data, reason, compiled)
		return true
	}
	update := PollUpdate{
		State:    state,
		RawState: rawState,
		Progress: progress,
		Data:     data,
		// 非终态且有效 → 连续失败清零（relay-06 §8.2）。
		Private: map[string]any{"poll_failures": 0},
	}
	if _, err := p.repo.ApplyPoll(ctx, task.ID, task.State, update); err != nil {
		p.log.Error("tasks: persist poll progress failed", "task_id", task.PublicTaskID, "error", err)
	}
	return true
}

// applyUnknown 记录 UNKNOWN 状态并累计连续失败。
func (p *Poller) applyUnknown(ctx context.Context, task *Task, rawState string, data map[string]any) {
	failures := privateInt(task.PrivateData["poll_failures"]) + 1
	update := PollUpdate{
		State:    StateUnknown,
		RawState: rawState,
		Progress: task.ProgressPercent,
		Data:     data,
		Private:  map[string]any{"poll_failures": failures},
	}
	won, err := p.repo.ApplyPoll(ctx, task.ID, task.State, update)
	if err != nil {
		p.log.Error("tasks: persist unknown state failed", "task_id", task.PublicTaskID, "error", err)
		return
	}
	if !won {
		return
	}
	if p.maxFailures > 0 && failures >= p.maxFailures {
		task.State = StateUnknown
		p.failAndRefund(ctx, task, "poll_failures_exceeded")
	}
}

// mergedData 生成 data 增量：上游快照 + （成功终态时的）usage 投影。
//
// 只合并、不替换：一次只带 progress 的响应不能把之前写入的 artifacts 冲掉。
// usage 只保留插件声明字段并做上界钳制（只数量、不价格）。
func (p *Poller) mergedData(task *Task, parsed map[string]any, compiled *plugin.CompiledPlugin) map[string]any {
	delta := map[string]any{}
	if td, ok := parsed["taskData"].(map[string]any); ok {
		for k, v := range td {
			delta[k] = v
		}
	}
	if d, ok := parsed["data"].(map[string]any); ok {
		for k, v := range d {
			delta[k] = v
		}
	}
	rawUsage, hasUsage := parsed["usage"]
	if !hasUsage {
		rawUsage = delta["usage"]
	}
	usage := normalizeUsage(rawUsage, compiled.Manifest())
	if usage != nil {
		delta["usage"] = usage
	} else {
		// 未经声明的 usage 一律不落库：即使一个字段都没声明，也不能让上游
		// 快照里的 cost/quota 等内部字段借 task.data 泄露给客户端。
		delete(delta, "usage")
	}
	return delta
}

// finalize 把任务推进到终态（CAS）并在获胜后触发结算/退款。
func (p *Poller) finalize(ctx context.Context, task *Task, state State, rawState string, progress int, data map[string]any, failReason string, compiled *plugin.CompiledPlugin) {
	now := p.now().UTC()
	update := PollUpdate{
		State:    state,
		RawState: rawState,
		Progress: progress,
		Data:     data,
		Private: map[string]any{
			// billing_pending 与终态在同一 UPDATE 落库：覆盖「状态已终态、
			// 结算还没跑就崩溃」的窗口，补偿扫描据此发现待办。
			"billing_pending": true,
			"poll_failures":   0,
		},
		FinishTime: &now,
	}
	if failReason != "" {
		update.FailReason = &failReason
	}
	won, err := p.repo.ApplyPoll(ctx, task.ID, task.State, update)
	if err != nil {
		p.log.Error("tasks: finalize task failed", "task_id", task.PublicTaskID, "error", err)
		return
	}
	if !won {
		// 另一个执行者已推进终态；结算由 CAS 赢家负责，本执行者必须退出。
		return
	}
	task.State = state
	task.RawState = rawState
	task.ProgressPercent = progress
	task.FinishTime = &now
	task.Data = mergeMaps(task.Data, data)
	if task.PrivateData == nil {
		task.PrivateData = map[string]any{}
	}
	task.PrivateData["billing_pending"] = true
	p.settleTerminal(ctx, task, compiled)
}

// extractCompletionUsage 调用插件的 extractUsageOnComplete（若导出）并把
// 结果合并进 data.usage。
//
// extractUsageOnComplete 的签名是 (ctx, result, data)；返回值只允许用量数量，
// 宿主按声明字段过滤 + 钳制后再落库，绝不把额度/价格写进任务行。
func (p *Poller) extractCompletionUsage(ctx context.Context, task *Task, compiled *plugin.CompiledPlugin, queryCtx, parsed, data map[string]any) map[string]any {
	if compiled == nil || !compiled.HasHook("extractUsageOnComplete") {
		return data
	}
	rawUsage, err := compiled.Call(ctx, "extractUsageOnComplete", queryCtx, parsed, data)
	if err != nil {
		// 用量钩子失败不改变终态：任务本身成功；usage 投影缺失由运维可见。
		p.log.Warn("tasks: extractUsageOnComplete failed", "task_id", task.PublicTaskID, "error", err)
		return data
	}
	if usage := normalizeUsage(rawUsage, compiled.Manifest()); usage != nil {
		if data == nil {
			data = map[string]any{}
		}
		data["usage"] = usage
	}
	return data
}

// failAndRefund 以当前状态为 from 推进 FAILURE。CAS 失败表示别人已推进，放弃。
func (p *Poller) failAndRefund(ctx context.Context, task *Task, reason string) {
	if task.State.Terminal() {
		return
	}
	p.finalize(ctx, task, StateFailure, reason, task.ProgressPercent, nil, reason, nil)
}

// recordFailure 累计连续轮询失败；达到上限时转 FAILURE + 退款。
func (p *Poller) recordFailure(ctx context.Context, task *Task, reason string) {
	if task.State.Terminal() {
		return
	}
	failures := privateInt(task.PrivateData["poll_failures"]) + 1
	won, err := p.repo.ApplyPoll(ctx, task.ID, task.State, PollUpdate{
		State:    task.State,
		RawState: task.RawState,
		Progress: task.ProgressPercent,
		Private:  map[string]any{"poll_failures": failures},
	})
	if err != nil {
		p.log.Error("tasks: record poll failure failed", "task_id", task.PublicTaskID, "error", err)
		return
	}
	if !won {
		return
	}
	if p.maxFailures > 0 && failures >= p.maxFailures {
		// 连续失败上限 → FAILURE 走退款链（relay-06 §8.3）。
		p.failAndRefund(ctx, task, "poll_failures_exceeded")
		return
	}
	p.log.Warn("tasks: poll attempt failed", "task_id", task.PublicTaskID, "reason", reason,
		"poll_failures", failures, "max_failures", p.maxFailures)
}

func (p *Poller) timedOut(task *Task) bool {
	if p.timeout <= 0 {
		return false
	}
	start := task.SubmitTime
	if start == nil {
		start = &task.CreatedAt
	}
	if start == nil || start.IsZero() {
		return false
	}
	return !p.now().Before(start.Add(p.timeout))
}

// buildQueryContext 组装 TaskQueryContext（relay-06 §3.2）。
//
// taskId 用**上游** ID：插件查询的是上游任务，public_task_id 只属于网关。
func buildQueryContext(task *Task, channel *ChannelTarget, apiKey string) map[string]any {
	model := toString(task.Properties["OriginModelName"])
	upstreamModel := toString(task.Properties["UpstreamModelName"])
	if upstreamModel == "" {
		upstreamModel = model
	}
	authHeader, auth := "", ""
	if apiKey != "" {
		authHeader, auth = "Authorization", "Bearer "+apiKey
	}
	return map[string]any{
		"model":         model,
		"upstreamModel": upstreamModel,
		"baseUrl":       channel.BaseURL,
		"apiKey":        apiKey,
		"authHeader":    authHeader,
		"auth":          auth,
		"taskId":        task.UpstreamTaskID,
	}
}

// taskChannelID 返回任务锁定的渠道；remix/续写锁原渠道时优先 locked_channel_id。
func taskChannelID(task *Task) int64 {
	if task.LockedChannelID > 0 {
		return task.LockedChannelID
	}
	return task.ChannelID
}

// normalizeUsage 只保留插件声明字段，并按 manifest 声明的单位做上界钳制。
//
// 键 = usageSchema 声明的字段名（不是单位枚举）；值只接受数值与字符串。
// 上界（relay-06 §9.3）：
//   - second → MaxRequestTaskDurationSeconds（3600）
//   - count  → 图像张数上界 128（契约 §10.1 MaxImageN）
//   - token/credit → 有限非负；小数保留（不在此转 int）
//
// 非法值（负数/NaN/Inf/越界）钳制到上界或丢弃，并**不**在任务 data 里保留
// 原始值——这里只产数量事实，不是审计表。
func normalizeUsage(raw any, m *plugin.Manifest) map[string]any {
	obj, ok := raw.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil
	}
	units := usageFieldUnits(m)
	if len(units) == 0 {
		return nil
	}
	out := make(map[string]any, len(obj))
	for name, value := range obj {
		unit, declared := units[name]
		if !declared {
			continue
		}
		if num, ok := numberValue(value); ok {
			out[name] = clampUsageNumber(num, unit)
			continue
		}
		if str, ok := value.(string); ok {
			out[name] = str
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// usageFieldUnits 汇总 usageSchema / usageProfiles 的字段 → 单位。
//
// 同一字段在不同 profile 里单位不同属于插件声明缺陷（加载期已由 T3 校验
// schema 形状）；这里取首次出现的声明，不猜。
func usageFieldUnits(m *plugin.Manifest) map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	for name, field := range m.UsageSchema {
		out[name] = field.Unit
	}
	for _, profile := range m.UsageProfiles {
		for name, field := range profile.Schema {
			if _, ok := out[name]; !ok {
				out[name] = field.Unit
			}
		}
	}
	return out
}

// 任务用量上界。MaxImageN 的唯一所有者是 relay-04 的 multimodal 包（契约
// §10.1 冻结 128）；tasks 包不 import 它（那会把整条 relay 适配器依赖带进
// 数据面），此处以同值常量承接并在测试中断言与 relay/multimodal.MaxImageN
// 一致的计划外风险登记在 T6b 报告。
const maxTaskImageCount = 128

// clampUsageNumber 按单位钳制用量数量：
//   - second：MaxRequestTaskDurationSeconds（3600，契约 §7.2）；
//   - count：128（MaxImageN）；
//   - token/credit/未声明：仅保证有限非负（小数保留）。
func clampUsageNumber(v float64, unit string) float64 {
	if v != v || v < 0 { // NaN 或负数
		return 0
	}
	switch unit {
	case "second":
		if v > contract.MaxRequestTaskDurationSeconds {
			return contract.MaxRequestTaskDurationSeconds
		}
	case "count":
		if v > maxTaskImageCount {
			return maxTaskImageCount
		}
	}
	if v > 1e18 { // +Inf 或荒谬大值：钳到可表示的有限上界
		return 1e18
	}
	return v
}

func parseProgress(v any) (int, bool) {
	num, ok := numberValue(v)
	if !ok {
		return 0, false
	}
	if num < 0 {
		num = 0
	}
	if num > 100 {
		num = 100
	}
	return int(num), true
}

// numberValue 把各来源的数值统一成 float64。
//
// 必须覆盖 int64：sobek 的 Export 对整数值返回 int64（不是 float64），
// 而数据库 jsonb 解码返回 float64。只认 float64 会让「插件返回的整数用量」
// 被静默丢弃（轮询成功但 usage 为空）。
func numberValue(v any) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func privateInt(v any) int {
	switch typed := v.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	}
	return 0
}

func mergeMaps(base, delta map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(delta))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range delta {
		out[k] = v
	}
	return out
}

func orDefaultDuration(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

func orDefaultInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
