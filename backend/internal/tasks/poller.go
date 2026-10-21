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
//
// 两个列表方法都必须带 family：插件 poller 固定传 FamilyPlugin，MJ poller
// 固定传 FamilyMJ。两族共用 tasks 表，漏掉 family 会让一族领取/结算另一族
// 的行（R-MJ-04），这是资金正确性问题而不是性能问题。
type PendingRepo interface {
	ListPendingByFamily(ctx context.Context, family string, limit int) ([]*Task, error)
	ListReconcilable(ctx context.Context, family string, limit int) ([]*Task, error)
	GetChannel(ctx context.Context, id int64) (*ChannelTarget, error)
	ApplyPoll(ctx context.Context, id int64, from State, u PollUpdate) (bool, error)
	UpdateBilling(ctx context.Context, id int64, quota *int64, private map[string]any) error
}

// BillingHandoff 是计费侧的异步交接收口（token-aware）。
//
// 必须带 tokenID：提交时的 PreConsume 在同一事务里扣了钱包 hold 与
// api_tokens.remain_quota；后台退款/结算若不传令牌身份，钱包退回但令牌额度
// 永久少扣（Refund 的 tokenDeducted 条件恒假）。
type BillingHandoff interface {
	TerminateByRequestIDForToken(requestID string, tokenID int64, fact contract.UsageFact) error
	// TerminateByRequestIDForTokenCost 按**已求值**的任务成本结算（T5）。
	//
	// 为什么需要它：任务计价表达式（u("字段")）的结果是积分，不经过
	// 模型的 token/倍率重算；若沿用 TerminateByRequestID 的空 fact 会按
	// 预扣额结清（差额 0），配置了表达式也不会生效。该方法让「表达式求值」
	// 与「资金收敛」解耦，同时复用同一条 hold CAS / 结算锁路径。
	TerminateByRequestIDForTokenCost(requestID string, tokenID, cost int64, fact contract.UsageFact) error
	RefundByRequestIDForToken(requestID string, tokenID int64) error
	// RefreshInflightLeases 续租订阅请求的 in-flight 租约（R-W9B-02）。
	//
	// 轮询器是任务 Detach 之后的守护者：单轮可能处理上千个任务、任务间隔
	// 1s，整轮远超租约 TTL。必须在轮批开始与每个任务处理前续租，否则兜底
	// 扫描会把仍在排队的活跃任务当成崩溃请求退款，任务随后成功即免费。
	// 钱包任务/非订阅来源是 no-op。
	RefreshInflightLeases(ctx context.Context, requestIDs []string) error
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

	Interval   time.Duration
	TaskGap    time.Duration
	QueryLimit int
	// MaxFailures / Timeout 用指针表达三态（W9-B4 / R-W9B-04）：
	//   nil   = 未配置 → 使用设计默认（20 / 1440m，见 NewPoller）；
	//   &0    = 显式禁用该判定（设计冻结的 `<=0` 禁用语义）；
	//   &正值 = 按配置值。
	// 旧签名用裸 int/Duration，0 同时被解释为「未设置」和「禁用」，正是
	// G-M6D-27 的语义相反缺陷；指针让两种意图在类型层面不可混淆。
	MaxFailures    *int
	Timeout        *time.Duration
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
	// 任务上限的三态默认化（W9-B4 / R-W9B-04）：只有「未配置」（nil）回落
	// 设计默认；显式 0 必须原样保留，供 timedOut/recordFailure 的 `<=0` /
	// `>0` 判定解释成「禁用」。不得再写 `if x == 0 { x = default }`——
	// 那会把运营的「禁用」静默变成「20 次/1440 分钟后强制退款」。
	p.maxFailures = DefaultMaxPollFailures
	if cfg.MaxFailures != nil {
		p.maxFailures = *cfg.MaxFailures
	}
	p.timeout = DefaultTaskTimeout
	if cfg.Timeout != nil {
		p.timeout = *cfg.Timeout
	}
	p.allowPrivate = cfg.AllowPrivate
	return p, nil
}

// TimeoutLimit / MaxFailuresLimit 返回已生效（三态默认化之后）的上限。
//
// 只读观测面：平台装配的接线测试与运维日志需要看到「配置是否真的传进来」，
// 而不是只看构造函数的入参。0 明确表示禁用，与「未配置」回落后得到的
// 1440m/20 可区分。
func (p *Poller) TimeoutLimit() time.Duration { return p.timeout }

// MaxFailuresLimit 见 TimeoutLimit。
func (p *Poller) MaxFailuresLimit() int { return p.maxFailures }

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

// pollOnce 拉取本族未完成任务并按渠道分组串行处理。
//
// 固定 family='plugin'：本 poller 只解释插件族的 Action、只调用插件钩子、
// 只按插件 1440m 超时推进。MJ 行由 MJPoller 领取（R-MJ-04）；SQL 过滤是
// 第一道隔离，processTask 还有第二道（纵深防御）。
func (p *Poller) pollOnce(ctx context.Context) {
	tasks, err := p.repo.ListPendingByFamily(ctx, FamilyPlugin, p.queryLimit)
	if err != nil {
		// 扫描失败 ≠ 无未完成任务：不更新任何水位，下一轮重试。
		p.log.Error("tasks: list pending tasks failed; retrying next round", "error", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	// R-W9B-02：轮批开始先给本批所有非终态任务续一次租约。这样即使某个
	// 任务排在很长的单轮末尾，它的租约也不会在排队期间先过期；随后
	// processTask 还会在真正处理前再续一次（覆盖单任务耗时/轮内长时间间隔）。
	p.renewInflightLeases(ctx, tasks)
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

// renewInflightLeases 为一批未终态任务续租（R-W9B-02）。
//
// 只收集有 request_id 的非终态任务：终态任务的资金已经/即将由结算/退款
// 收敛，续租只会与终态 CAS 竞争；没有 request_id 的行无法结算，续租也
// 没有意义。失败只记录，调用方继续轮询。
func (p *Poller) renewInflightLeases(ctx context.Context, tasks []*Task) {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task == nil || task.State.Terminal() {
			continue
		}
		if id := toString(task.PrivateData["request_id"]); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := p.billing.RefreshInflightLeases(ctx, ids); err != nil {
		p.log.Warn("tasks: refresh inflight leases for poll batch failed; settlement direction unchanged",
			"tasks", len(ids), "error", err)
	}
}

// processTask 推进单个任务：超时检查 → 插件构建查询 → 出站 → 分类处理。
func (p *Poller) processTask(ctx context.Context, task *Task) {
	if task == nil || task.State.Terminal() {
		return
	}
	// 【纵深防御 R-MJ-04】SQL 已按 family='plugin' 过滤，但轮询还有其它入口
	// （测试 fake、将来新增的领取路径、回滚期间的旧二进制），任何一条漏掉
	// family 都会让插件 poller 对 MJ 行累计 plugin_unavailable 并最终
	// FAILURE + 退款。这里在动任何状态之前直接退出：不改 state、不加
	// poll_failures、不结算——未识别的行保持原样比错误推进安全。
	if task.Family != FamilyPlugin {
		p.log.Warn("tasks: plugin poller received a non-plugin task; skipping (family isolation)",
			"task_id", task.PublicTaskID, "family", task.Family)
		return
	}
	// R-W9B-02：真正处理当前任务前再续一次租约。轮批次量续租覆盖不了
	// 「单轮内任务间隔 1s × 队列长度」超过 TTL 的情况——排队靠后的任务在
	// 轮到它时租约可能已过 TTL。给当前任务的 requestID 续租后，兜底扫描
	// 才不会在它运行期间把它退掉（否则任务完成后结算即免费）。
	if requestID := toString(task.PrivateData["request_id"]); requestID != "" {
		if err := p.billing.RefreshInflightLeases(ctx, []string{requestID}); err != nil {
			// 续租失败只记录：不能因此停止轮询或改变结算方向；租约过期后
			// 扫描可能抢先退款，终态 CAS 会阻止后续结算造成双花。
			p.log.Warn("tasks: refresh inflight lease before processing failed; settlement direction unchanged",
				"task_id", task.PublicTaskID, "request_id", requestID, "error", err)
		}
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
		// 其它 4xx：先尝试解析；**仍非终态视为未识别并累计失败**（relay-06
		// §8.2）。不能因为 body 能解析成 IN_PROGRESS 就清零计数——否则一个
		// 持续返回 400/409 的上游能让任务拖到 1440 分钟超时，hold 被多锁。
		if p.applyParsed(ctx, task, compiled, queryCtx, resp) {
			// UNKNOWN 已在 applyUnknown 内累计、终态已由上分支 finalize。
			if task.State == StateUnknown || task.State.Terminal() {
				return
			}
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
	won, err := p.repo.ApplyPoll(ctx, task.ID, task.State, update)
	if err != nil {
		p.log.Error("tasks: persist poll progress failed", "task_id", task.PublicTaskID, "error", err)
		return true
	}
	if won {
		// 同步内存快照：调用方（如「其它 4xx 仍非终态」分支）紧接着还要用
		// 新状态做 CAS 累计失败；不同步会让第二次 CAS 因 from 过期而丢失。
		task.State = state
		task.RawState = rawState
		task.ProgressPercent = progress
		task.Data = mergeMaps(task.Data, data)
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
	return taskTimedOut(task, p.timeout, p.now())
}

// taskTimedOut 是插件/MJ 两族共用的外层超时判定（W9-B4）。
//
// 抽成包级函数是为了让 MJPoller 与插件 Poller 使用**同一份**边界语义，
// 避免两族各自实现后在边界上漂移。
//
// 边界方向按设计原文与基线实现冻结为**严格大于**
// （`now - 上限*60 > submit_time`）：恰好等于阈值仍不算超时，超过一秒才
// 算。timeout <= 0 表示禁用（设计冻结的 `<=0` 语义），未配置由构造器先
// 填默认值。
func taskTimedOut(task *Task, timeout time.Duration, now time.Time) bool {
	if task == nil || timeout <= 0 {
		return false
	}
	start := task.SubmitTime
	if start == nil {
		start = &task.CreatedAt
	}
	if start == nil || start.IsZero() {
		return false
	}
	return now.After(start.Add(timeout))
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

// 任务用量上界。maxTaskImageCount 与契约 §10.1 的 MaxImageN 同源（同值 128）；
// 唯一所有者是 relay-04 的 multimodal 包，tasks 包不 import 它（那会把整条
// relay 适配器依赖带进数据面），因此在此复写同值常量，并由钳制测试覆盖。
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
