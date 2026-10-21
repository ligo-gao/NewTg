package tasks

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// 本文件是绘图任务族（family='mj'）的后台领取入口（W8/MJ-A0 数据面）。
//
// 【为什么必须是独立 poller】两族共用同一张 tasks 表，但轮询语义完全不同：
// 插件族走 JS 钩子 buildQueryRequest/parseTaskResult、超时 1440m；MJ 族走
// midjourney-proxy 的 HTTP 查询形状、卡住兜底 1h（relay-05 §超时表）。
// 把 MJ 行塞进插件 poller 会让它在 20 次 plugin_unavailable 后 FAILURE +
// 退款（R-MJ-04）——上游可能仍在产出，退款是直接的资金损失。
//
// 【A0 冻结范围】本实现只提供「按 family='mj' 领取 + family 防御」骨架：
// 领取后默认不做任何推进（Handler 为 nil），也不做补偿扫描。原因是 MJ 的
// 查询驱动（MJ-b）与计费时机（MJ-a2/d，R-MJ-01）尚未接线，在 A0 阶段对
// MJ 行做任何状态/资金动作都会超出已冻结的语义。领取路径本身必须存在，
// 才能保证「MJ 行有唯一归属的 poller」，而不是靠插件 poller 的 SQL 过滤
// 单点防护。

// MJ 族的轮询上限冻结值（relay-05 §6.1）。
//
// 【为什么与插件族分开】设计明文：MJ 的卡住兜底是 **1h**，插件是 1440m，
// 两族非同族、勿互引。把两者接到同一个 task.poll_* 键上会让「调 MJ 兜底」
// 连带改变插件任务的执行超时（1440m → 60m 会把仍在生成的长视频任务提前
// 失败退款）。因此 MJ 有独立键与独立默认值。
const (
	DefaultMJTaskTimeout     = 60 * time.Minute
	DefaultMJMaxPollFailures = 20
)

// MJTaskHandler 是 MJ 任务推进的接缝。
//
// MJ-b 在这里接上游查询/状态映射与终态结算。返回 error 会由 MJPoller 计入
// 连续失败（W9-B4 起与插件族同口径），达到配置上限时转 FAILURE；上游查询
// 的具体重试/退避策略属于驱动实现。
type MJTaskHandler func(ctx context.Context, task *Task) error

// MJTaskSettler 是 MJ 族的补偿结算接缝（生产 tasks.Settler）。
//
// 与 Handler 分开：Handler 只在「非终态 → 查询上游」路径使用；补偿扫描
// 处理的是**已终态**且带 billing_pending 的行，只需要结算/退款，绝不能再
// 查上游或改状态。把两者混成一个函数会让补偿路径意外复活已终态任务。
type MJTaskSettler interface {
	SettleTerminal(ctx context.Context, task *Task)
}

// MJPollerConfig 是 MJ 领取器的依赖与可调参数；零值字段使用冻结默认值。
type MJPollerConfig struct {
	Repo    PendingRepo
	Handler MJTaskHandler
	// Settler 为 nil 时关闭补偿扫描（A0 冻结语义：只领取、不结算）。
	Settler MJTaskSettler
	// Billing 用于在飞租约续期（R-W9B-02 / B2）。nil 时跳过续租（仅测试
	// 或没有订阅来源的装配）；生产必须注入计费交接面。
	Billing BillingHandoff
	Logger  *slog.Logger

	// Now 是测试接缝；nil → time.Now（与插件 Poller 的 Now 同口径）。
	Now func() time.Time

	Interval   time.Duration
	QueryLimit int
	// TaskGap 是同一渠道内相邻任务之间的串行间隔（relay-05 §5/§6.2：与插件
	// 族同一调度语义，避免并发轮询把同一上游渠道打出 429）。默认 1s。
	TaskGap time.Duration
	// Sleep 是测试接缝；nil → 上下文感知定时器。
	Sleep func(ctx context.Context, d time.Duration) error
	// LeaseKeeperInterval 是单任务处理期间的续租间隔（默认 30s）。
	LeaseKeeperInterval time.Duration
	// RetentionDeadline 计算终态保留截止（relay-05 §9）。nil = 不写
	// retention_deadline（与 `mj.retention_terminal_days=0` 同义）。
	// 超时/失败上限推进的终态也必须写期限，否则这类行永远不会被清理。
	RetentionDeadline func(now time.Time) *time.Time
	// MaxFailures / Timeout 的三态语义与插件 Poller 相同（W9-B4 / R-W9B-04）：
	// nil = 未配置 → MJ 专属默认（60m / 20）；&0 = 禁用；&正值 = 按值。
	//
	// 键是 MJ 专属的 mj.poll_timeout_minutes / mj.poll_max_failures：设计
	// 明确 MJ 兜底 1h、插件 1440m（relay-05 §6.1），共用 task.* 会让调整
	// 一族连带改变另一族的执行超时。
	MaxFailures *int
	Timeout     *time.Duration
}

// MJPoller 只领取 family='mj' 的非终态任务，并补偿本族的待结算终态行。
//
// 与插件 Poller 一样，单副本由部署保证；跨副本的恰好一次推进仍由
// ApplyPoll 的 (id, from_state) CAS 承担（Handler 内部使用），结算侧再叠加
// 按 request_id 的计费幂等（第二道保证）。
type MJPoller struct {
	repo         PendingRepo
	handler      MJTaskHandler
	settler      MJTaskSettler
	billing      BillingHandoff
	log          *slog.Logger
	now          func() time.Time
	interval     time.Duration
	taskGap      time.Duration
	sleep        func(context.Context, time.Duration) error
	queryLimit   int
	maxFailures  int
	timeout      time.Duration
	leaseKeepGap time.Duration
	retention    func(time.Time) *time.Time
}

// NewMJPoller 构造 MJ 领取器。Repo 是硬依赖：没有存储面就无法领取，
// 静默返回「没有任务」会让 MJ 任务永远停在非终态。
func NewMJPoller(cfg MJPollerConfig) (*MJPoller, error) {
	if cfg.Repo == nil {
		return nil, errors.New("tasks: mj poller requires repo")
	}
	p := &MJPoller{
		repo:       cfg.Repo,
		handler:    cfg.Handler,
		settler:    cfg.Settler,
		billing:    cfg.Billing,
		log:        cfg.Logger,
		now:        cfg.Now,
		interval:   orDefaultDuration(cfg.Interval, DefaultPollInterval),
		taskGap:    orDefaultDuration(cfg.TaskGap, DefaultTaskGap),
		sleep:      cfg.Sleep,
		queryLimit: orDefaultInt(cfg.QueryLimit, DefaultQueryLimit),
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = sleepContext
	}
	p.leaseKeepGap = orDefaultDuration(cfg.LeaseKeeperInterval, DefaultLeaseKeeperInterval)
	p.retention = cfg.RetentionDeadline
	// 三态默认化与 NewPoller 相同：nil 才回落 MJ 专属默认（60m/20），
	// &0 保留为禁用。绝不能复用插件的 1440m（relay-05 §6.1 明示两族不同）。
	p.maxFailures = DefaultMJMaxPollFailures
	if cfg.MaxFailures != nil {
		p.maxFailures = *cfg.MaxFailures
	}
	p.timeout = DefaultMJTaskTimeout
	if cfg.Timeout != nil {
		p.timeout = *cfg.Timeout
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p, nil
}

// TimeoutLimit / MaxFailuresLimit 返回三态默认化之后生效的上限。
//
// 与插件 Poller 同名方法同语义：只读观测面，供平台接线测试确认配置真的
// 传到了 MJ 领取器；0 = 禁用，未配置时是 MJ 默认 60m/20。
func (p *MJPoller) TimeoutLimit() time.Duration { return p.timeout }

// MaxFailuresLimit 见 TimeoutLimit。
func (p *MJPoller) MaxFailuresLimit() int { return p.maxFailures }

// Run 运行领取循环直到 ctx 取消（停机不是错误）。
func (p *MJPoller) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	p.runCycle(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.runCycle(ctx)
		}
	}
}

// RunOnce 执行一轮领取（测试与运维手动触发用）。
func (p *MJPoller) RunOnce(ctx context.Context) {
	p.runCycle(ctx)
}

func (p *MJPoller) runCycle(ctx context.Context) {
	p.pollOnce(ctx)
	p.reconcileOnce(ctx)
}

// reconcileOnce 处理本族「已终态但结算未落地」的行（billing_pending /
// settlement_pending / refund_pending）。
//
// 这是「恰好一次」的补偿半边：终态 CAS 赢家负责第一次结算；如果结算调用
// 失败（DB/账本抖动），Settler 会留下 pending 标记，本扫描在后续轮次重试。
// 计费按 request_id 幂等，因此重复尝试不会二次扣费/退款。
//
// settlement_pending 只在 MJ 族的**结算失败**上出现；插件 poller 的补偿
// 扫描固定 family='plugin'，两族不会互相替对方动钱（R-MJ-04）。
func (p *MJPoller) reconcileOnce(ctx context.Context) {
	if p.settler == nil {
		// A0 语义：只领取、不结算。没有 Settler 时绝不猜金额。
		return
	}
	rows, err := p.repo.ListReconcilable(ctx, FamilyMJ, DefaultReconcileLimit)
	if err != nil {
		p.log.Error("tasks: mj list reconcilable failed; retrying next round", "error", err)
		return
	}
	for _, task := range rows {
		if ctx.Err() != nil {
			return
		}
		// 与插件补偿同口径：结算前补一次续租，避免兜底扫描在结算落地前
		// 抢先退款（R-W9B-02）。
		renewTaskLease(ctx, p.billing, p.log, task)
		p.settler.SettleTerminal(ctx, task)
	}
}

// pollOnce 固定领取 family='mj'。
//
// 这里没有渠道分组/间隔：MJ 的查询驱动在 MJ-b 才接入，A0 不向上游发请求，
// 因而不存在「同渠道限流」需要串行化的问题；接入驱动时应复用插件的
// 同渠道串行语义，而不是在 A0 提前虚构一个不需要的睡眠。
func (p *MJPoller) pollOnce(ctx context.Context) {
	tasks, err := p.repo.ListPendingByFamily(ctx, FamilyMJ, p.queryLimit)
	if err != nil {
		// 扫描失败 ≠ 没有未完成任务：不更新任何水位，下一轮重试。
		p.log.Error("tasks: mj list pending failed; retrying next round", "error", err)
		return
	}
	// B2 兼容：轮批开始先给本批所有非终态任务续一次在飞租约。MJ 行若来自
	// 订阅来源，Detach 之后只靠这条续租避免兜底扫描把仍在轮询的任务当成
	// 崩溃请求退款（那会把随后成功的任务变成免费）。
	renewInflightLeases(ctx, p.billing, p.log, tasks)
	// 同渠道内串行（relay-05 §5/§6.2）：同一上游渠道的限流/连接复用由单线程
	// 顺序消化，避免并发轮询把渠道打出 429。跨渠道仍按领取顺序推进。
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

// processTask 是 MJ 领取器的单任务入口。
func (p *MJPoller) processTask(ctx context.Context, task *Task) {
	if task == nil || task.State.Terminal() {
		return
	}
	// 纵深防御：即使领取 SQL 被改错/被 fake 绕过，也只允许 family='mj'。
	// 与插件 poller 的对称防御一起，保证两族在任何一条入口上都不会互相
	// 推进（R-MJ-04）。
	if task.Family != FamilyMJ {
		p.log.Warn("tasks: mj poller received a non-mj task; skipping (family isolation)",
			"task_id", task.PublicTaskID, "family", task.Family)
		return
	}
	if p.handler == nil {
		// A0：只完成「领取」。不写状态、不计失败、不结算——MJ 的查询与
		// 计费时机尚未接线，任何推进都会超出冻结语义。
		//
		// W9-B4：即使任务已超过配置的超时/失败上限，这里也**不得改状态**。
		// 超时的后果是 FAILURE + 退款，而 MJ 的退款时机由 MJ-a2/d 接线；
		// 在 Handler 为空时推进终态会造出「已失败但无人结算」的行。
		return
	}
	// Handler 已注入 = 调用方接管了 MJ 推进（MJ-b/MJ-a2/d）。此时超时与
	// 连续失败上限按与插件族相同的配置生效（W9-B4 的「同一约束」）。
	from := task.State
	// B2：处理当前任务前续租一次，并在处理期间由 keeper 周期续租。MJ 单次
	// 查询 15s + 结算，远小于租约 TTL，一次续租 + keeper 足以覆盖。
	renewTaskLease(ctx, p.billing, p.log, task)
	stopKeeper := startTaskLeaseKeeper(ctx, p.billing, p.log, task, p.leaseKeepGap)
	defer stopKeeper()
	if taskTimedOut(task, p.timeout, p.now()) {
		p.failTask(ctx, task, from, "task_timeout")
		return
	}
	if err := p.handler(ctx, task); err != nil {
		p.recordFailure(ctx, task, from, err)
		return
	}
	p.clearFailures(ctx, task, from)
}

// failTask 把 MJ 行以 (id, from_state) CAS 推进到 FAILURE。
//
// 与插件 poller 的 finalize 一样写 billing_pending=true：终态与「结算待办」
// 必须同批落库，否则「状态已失败、退款还没跑就崩溃」的行会被永久忽略。
// 这里不直接退款——MJPoller 没有计费依赖，MJ 的结算时机由 MJ-a2/d 负责，
// W9-B4 只负责把配置的判定接进来，不改变结算语义。
func (p *MJPoller) failTask(ctx context.Context, task *Task, from State, reason string) {
	now := p.now().UTC()
	var retention *time.Time
	if p.retention != nil {
		retention = p.retention(now)
	}
	won, err := p.repo.ApplyPoll(ctx, task.ID, from, PollUpdate{
		State: StateFailure,
		// 网关自身判定的终态没有上游原文：raw_state 必须清空，让
		// ProjectStatus 回 "FAILURE"。若沿用旧 raw（如 IN_PROGRESS），
		// fetch 的 status 会永远回 "IN_PROGRESS"（I-2）。
		RawState:   "",
		Progress:   task.ProgressPercent,
		FailReason: &reason,
		FinishTime: &now,
		// 终态清零计数，与插件 finalize 同形状；补偿扫描据 billing_pending
		// 发现待结算行。
		Private:           map[string]any{"billing_pending": true, "poll_failures": 0},
		RetentionDeadline: retention,
	})
	if err != nil {
		p.log.Error("tasks: mj fail task failed", "task_id", task.PublicTaskID, "reason", reason, "error", err)
		return
	}
	if !won {
		return
	}
	p.log.Warn("tasks: mj task failed by poll limit",
		"task_id", task.PublicTaskID, "reason", reason)
}

// recordFailure 累计 MJ 连续轮询失败；达到上限时转 FAILURE。
//
// 计数用仓储的原子递增（M-6）：并发轮询两个执行者都读到旧值再写回时，
// 只会留下一次 +1，失败上限永远打不到，hold 被无限期锁住。
//
// 与插件 recordFailure 的差异只在结算动作：插件族在这里立即退款，MJ 族
// 把 billing_pending 留给补偿扫描。上限 <= 0 表示禁用（设计冻结语义）。
func (p *MJPoller) recordFailure(ctx context.Context, task *Task, from State, cause error) {
	failures, won, err := p.repo.IncrementPollFailures(ctx, task.ID, from)
	if err != nil {
		p.log.Error("tasks: mj record poll failure failed", "task_id", task.PublicTaskID, "error", err)
		return
	}
	if !won {
		return
	}
	if p.maxFailures > 0 && failures >= p.maxFailures {
		p.failTask(ctx, task, from, "poll_failures_exceeded")
		return
	}
	p.log.Warn("tasks: mj poll attempt failed", "task_id", task.PublicTaskID, "error", cause,
		"poll_failures", failures, "max_failures", p.maxFailures)
}

// clearFailures 在 Handler 成功推进后清零连续失败计数。
//
// 【unknown 不得清零（I-3）】R-MJ-15 规定未知状态不累计失败，但「不累计」
// 不等于「清零已有计数」：若上游连续返回未知状态，清零会让失败上限永远
// 打不到，任务拖到超时才失败。只有已知非 unknown 状态才清零。
//
// 只清「已有计数」的行；CAS 失败说明 Handler 已自行推进状态，计数由
// Handler 的新状态承担（best-effort，与插件族「有效响应清零」同语义）。
// Handler 必须把观测结果同步回 task.State（MJ 驱动这么做），否则这里读到
// 的是旧快照。
func (p *MJPoller) clearFailures(ctx context.Context, task *Task, from State) {
	if task.State == StateUnknown {
		return
	}
	if task.PrivateData == nil || privateInt(task.PrivateData["poll_failures"]) == 0 {
		return
	}
	won, err := p.repo.ApplyPoll(ctx, task.ID, from, PollUpdate{
		State:    from,
		RawState: task.RawState,
		Progress: task.ProgressPercent,
		Private:  map[string]any{"poll_failures": 0},
	})
	if err != nil {
		p.log.Warn("tasks: mj clear poll failures failed", "task_id", task.PublicTaskID, "error", err)
		return
	}
	if won {
		task.PrivateData["poll_failures"] = 0
	}
}
