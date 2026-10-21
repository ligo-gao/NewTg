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
// 只用于日志：A0 不定义重试/失败计数语义，那属于 MJ-b 的调度策略。
type MJTaskHandler func(ctx context.Context, task *Task) error

// MJPollerConfig 是 MJ 领取器的依赖与可调参数；零值字段使用冻结默认值。
type MJPollerConfig struct {
	Repo    PendingRepo
	Handler MJTaskHandler
	Logger  *slog.Logger

	Interval   time.Duration
	QueryLimit int
}

// MJPoller 只领取 family='mj' 的非终态任务。
//
// 与插件 Poller 一样，单副本由部署保证；跨副本的恰好一次推进仍由
// ApplyPoll 的 (id, from_state) CAS 承担（Handler 内部使用）。
type MJPoller struct {
	repo       PendingRepo
	handler    MJTaskHandler
	log        *slog.Logger
	interval   time.Duration
	queryLimit int
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
		interval:   orDefaultDuration(cfg.Interval, DefaultPollInterval),
		queryLimit: orDefaultInt(cfg.QueryLimit, DefaultQueryLimit),
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p, nil
}

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
		return
	}
	if err := p.handler(ctx, task); err != nil {
		p.log.Warn("tasks: mj task handler failed",
			"task_id", task.PublicTaskID, "state", task.State, "error", err)
	}
}
