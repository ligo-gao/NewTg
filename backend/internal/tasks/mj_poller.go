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

// MJTaskHandler 是 MJ 任务推进的接缝（A0 的生产装配不注入）。
//
// MJ-b 在这里接上游查询/状态映射；MJ-a2/d 在这里接终态结算。返回 error
// 会由 MJPoller 计入连续失败（W9-B4 起与插件族同口径），达到配置上限时
// 转 FAILURE；上游查询的具体重试/退避策略仍属于 MJ-b。
type MJTaskHandler func(ctx context.Context, task *Task) error

// MJPollerConfig 是 MJ 领取器的依赖与可调参数；零值字段使用冻结默认值。
type MJPollerConfig struct {
	Repo    PendingRepo
	Handler MJTaskHandler
	Logger  *slog.Logger

	// Now 是测试接缝；nil → time.Now（与插件 Poller 的 Now 同口径）。
	Now func() time.Time

	Interval   time.Duration
	QueryLimit int
	// MaxFailures / Timeout 与插件 Poller 同口径（W9-B4 / R-W9B-04）：
	// nil = 未配置 → 设计默认；&0 = 禁用；&正值 = 按值。
	//
	// 两族共用同一对配置键（task.poll_timeout_minutes / task.poll_max_failures），
	// 因为运营语义是「轮询兜底要不要生效」，而不是「插件族和 MJ 族各调一套」；
	// 默认值与插件族一致（20 / 1440m）也保证未配置时两族行为可预期。
	MaxFailures *int
	Timeout     *time.Duration
}

// MJPoller 只领取 family='mj' 的非终态任务。
//
// 与插件 Poller 一样，单副本由部署保证；跨副本的恰好一次推进仍由
// ApplyPoll 的 (id, from_state) CAS 承担（Handler 内部使用）。
type MJPoller struct {
	repo        PendingRepo
	handler     MJTaskHandler
	log         *slog.Logger
	now         func() time.Time
	interval    time.Duration
	queryLimit  int
	maxFailures int
	timeout     time.Duration
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
		log:        cfg.Logger,
		now:        cfg.Now,
		interval:   orDefaultDuration(cfg.Interval, DefaultPollInterval),
		queryLimit: orDefaultInt(cfg.QueryLimit, DefaultQueryLimit),
	}
	if p.now == nil {
		p.now = time.Now
	}
	// 三态默认化与 NewPoller 完全相同：nil 才回落默认，&0 保留为禁用。
	p.maxFailures = DefaultMaxPollFailures
	if cfg.MaxFailures != nil {
		p.maxFailures = *cfg.MaxFailures
	}
	p.timeout = DefaultTaskTimeout
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
// 传到了 MJ 领取器；0 = 禁用，未配置时是 1440m/20。
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
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		p.processTask(ctx, task)
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
	won, err := p.repo.ApplyPoll(ctx, task.ID, from, PollUpdate{
		State:      StateFailure,
		RawState:   task.RawState,
		Progress:   task.ProgressPercent,
		FailReason: &reason,
		FinishTime: &now,
		// 终态清零计数，与插件 finalize 同形状；补偿扫描据 billing_pending
		// 发现待结算行。
		Private: map[string]any{"billing_pending": true, "poll_failures": 0},
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
// 与插件 recordFailure 的差异只在结算动作：插件族在这里立即退款，MJ 族
// 把 billing_pending 留给 MJ-a2/d。上限 <= 0 表示禁用（设计冻结语义）。
func (p *MJPoller) recordFailure(ctx context.Context, task *Task, from State, cause error) {
	failures := privateInt(task.PrivateData["poll_failures"]) + 1
	won, err := p.repo.ApplyPoll(ctx, task.ID, from, PollUpdate{
		State:    from,
		RawState: task.RawState,
		Progress: task.ProgressPercent,
		Private:  map[string]any{"poll_failures": failures},
	})
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
// 只清「已有计数」的行；CAS 失败说明 Handler 已自行推进状态，计数由
// Handler 的新状态承担（best-effort，与插件族「有效响应清零」同语义）。
func (p *MJPoller) clearFailures(ctx context.Context, task *Task, from State) {
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
