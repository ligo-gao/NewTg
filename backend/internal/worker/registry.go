// Package worker 实现后台任务框架：静态任务注册表、租约调度、运行记录与
// 管理面服务（billing-06 §2 / §13.5）。
//
// 【本包的能力边界】
//   - 注册表只登记**已经在运行**的后台循环（platform.runWorker 的裸 goroutine）
//     与本次新增的两个维护任务；不为统一而重写既有 worker——既有循环的
//     ticker、锁与失败语义保持原样，注册表只负责让它们可见、可读、可审计。
//   - 调度器只执行**显式绑定 Run 的类型**（Scheduled 或 admin 手动触发）。
//     外部循环（Scheduled=false 且 Run=nil）只出现在目录里，调度器不碰它们。
//
// 【为什么静态注册表而不是 DB 目录】任务名、周期、超时与白名单是代码契约
// （billing-06 §13.5.1 冻结的 5 个 + spec 逐项追加的 holiday_sync、
// upstream_usage_reconcile）。允许 DB 增删任务等于开放
// 「任意任务名执行」入口，与 §13.5「不提供任意 SQL/命令/函数名」冲突。
package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ConcurrencyPolicy 描述同一任务类型允许的并发度。
//
// 本框架只有 single 一种：worker_task_locks 的 task_type 主键与 Redis
// SET NX PX 都表达「同类型至多一个执行者」（billing-06 §2.3）。保留字段
// 是为了让注册表显式声明策略，而不是让调用方从实现里反推。
type ConcurrencyPolicy string

// ConcurrencySingle 是当前唯一策略。
const ConcurrencySingle ConcurrencyPolicy = "single"

// Task 是静态注册表的一项。
//
// Scheduled 与 Run 分离：既有的裸 goroutine 有自己的 ticker（Scheduled=false,
// Run=nil），但白名单维护任务可能由既有循环周期执行、同时允许管理员手动
// 触发一次（Scheduled=false, Run!=nil）。调度器只周期执行 Scheduled=true
// 的任务；手动触发只要求 Run!=nil。
type Task struct {
	Name        string
	Description string
	// Interval 是周期。0 表示非周期（仅手动触发）。对 Scheduled=false 的
	// 任务，该值只用于管理面展示既有循环的实际节奏。
	Interval time.Duration
	// Timeout 是单次执行上限；0 使用 DefaultTaskTimeout。
	Timeout time.Duration
	// ManualTriggerAllowed 是 billing-06 §13.5.1 冻结的白名单标记。
	ManualTriggerAllowed bool
	// Scheduled 表示由本包的调度器按 Interval 周期执行。
	Scheduled bool
	// Concurrency 是并发策略；空值按 single 处理。
	Concurrency ConcurrencyPolicy
	// Enabled 是运行时谓词（§2.6）。nil 视为启用；返回 false 时该 pass
	// 跳过且**不建 run 行、不推进 last_run_at**（空闲不建行）。
	// 手动触发同样受它约束：谓词为假时返回 409（BlockedCode），而不是入队
	// 一条永远不会执行的 pending 行。
	Enabled func() bool
	// BlockedCode 是谓词为假时暴露给管理面的固定错误码（如
	// log_partition_drop_disabled）。空值回退 task_disabled。
	BlockedCode string
	// Run 是一次执行。nil 表示该任务由既有循环独占执行，调度器与手动触发
	// 都不可用（避免出现「触发成功但永远不执行」的 pending 行）。
	Run func(ctx context.Context) (Result, error)
}

// Result 是写入 worker_task_runs.result 的汇总。只允许计数与枚举，不允许
// 上游响应体、凭据或请求载荷（运行历史保留 30 天）。
type Result map[string]any

// DefaultTaskTimeout 是未声明超时任务的单次执行上限。
//
// 维护类任务（清理/预建/快照）在正常规模下远小于 10 分钟；上限的作用是
// 让「卡死的执行」在租约过期前主动收敛，而不是占着租约到心跳失败。
const DefaultTaskTimeout = 10 * time.Minute

// ErrDuplicateTask 表示注册表出现同名任务：启动期 fail-fast，避免
// 管理面列表与调度器各看到一份不同的元数据。
var ErrDuplicateTask = errors.New("worker: duplicate task name")

// Registry 是静态任务表，构造后只读（Run 绑定在构造时完成）。
type Registry struct {
	order []string
	tasks map[string]Task
}

// NewRegistry 校验并构造注册表。空名字、重复名字、周期为负都返回错误。
func NewRegistry(tasks ...Task) (*Registry, error) {
	r := &Registry{tasks: make(map[string]Task, len(tasks))}
	for _, task := range tasks {
		if task.Name == "" {
			return nil, errors.New("worker: task name is required")
		}
		if task.Interval < 0 {
			return nil, fmt.Errorf("worker: task %q has negative interval", task.Name)
		}
		if task.Timeout < 0 {
			return nil, fmt.Errorf("worker: task %q has negative timeout", task.Name)
		}
		if task.Concurrency == "" {
			task.Concurrency = ConcurrencySingle
		}
		if task.Concurrency != ConcurrencySingle {
			return nil, fmt.Errorf("worker: task %q has unsupported concurrency policy %q", task.Name, task.Concurrency)
		}
		if _, exists := r.tasks[task.Name]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateTask, task.Name)
		}
		r.tasks[task.Name] = task
		r.order = append(r.order, task.Name)
	}
	sort.Strings(r.order)
	return r, nil
}

// Tasks 按名字升序返回一份副本（调用方不得依赖内部 map）。
func (r *Registry) Tasks() []Task {
	if r == nil {
		return nil
	}
	out := make([]Task, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tasks[name])
	}
	return out
}

// Names 按名字升序返回任务名。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

// Get 返回任务与「是否注册」。
func (r *Registry) Get(name string) (Task, bool) {
	if r == nil {
		return Task{}, false
	}
	task, ok := r.tasks[name]
	return task, ok
}

// SetEnabled 给已注册任务绑定运行时谓词与固定阻断码。
//
// 生产用它表达「开关关闭时手动触发必须 409」：仅靠 worker_tasks.enabled
// 是调度目录的运营意图，无法表达 log.partition_drop_enabled 这类运行时配置。
func (r *Registry) SetEnabled(name string, enabled func() bool, blockedCode string) error {
	if r == nil {
		return errors.New("worker: registry is nil")
	}
	task, ok := r.tasks[name]
	if !ok {
		return fmt.Errorf("worker: cannot configure unregistered task %q", name)
	}
	task.Enabled = enabled
	task.BlockedCode = blockedCode
	r.tasks[name] = task
	return nil
}

// BindRun 给已注册任务绑定执行函数（生产在 platform.runWorker 中调用）。
//
// 只允许绑定注册表中已声明的名字：绑定未声明的任务会制造「列表里看不到、
// 却能被触发」的隐藏执行面。
func (r *Registry) BindRun(name string, run func(ctx context.Context) (Result, error)) error {
	if r == nil {
		return errors.New("worker: registry is nil")
	}
	task, ok := r.tasks[name]
	if !ok {
		return fmt.Errorf("worker: cannot bind unregistered task %q", name)
	}
	if run == nil {
		return fmt.Errorf("worker: run function for %q is nil", name)
	}
	task.Run = run
	r.tasks[name] = task
	return nil
}

// DefaultRegistry 返回生产静态注册表（未绑定 Run）。
//
// 名称、周期与可手动触发标记以 billing-06 §3 的任务清单与 §13.5.1 的白名单
// 为准；`model_update`（上游模型变更检测）本系统**没有实现**，刻意不登记：
// 让列表出现一个永不运行的任务等于伪造状态（R-W17-06）。
func DefaultRegistry() (*Registry, error) {
	return NewRegistry(
		// ---- A 类：框架调度型（既有循环，Scheduled=false） ----
		Task{
			Name:        "channel_test",
			Description: "Channel health probing and automatic disable/enable",
			Interval:    10 * time.Minute,
		},
		Task{
			Name:        "async_task_poll",
			Description: "Poll unfinished plugin-family tasks upstream",
			Interval:    15 * time.Second,
		},
		Task{
			Name:        "midjourney_poll",
			Description: "Poll unfinished Midjourney-family tasks upstream",
			Interval:    15 * time.Second,
		},
		Task{
			Name:        "log_cleanup",
			Description: "Administrator-triggered log cleanup (manual only)",
			Interval:    0,
			Timeout:     30 * time.Minute,
			// 白名单第 1 项：只删过期日志，幂等、无破坏性业务数据。
			ManualTriggerAllowed: true,
		},
		// ---- B 类：既有独立循环（全部 Scheduled=false） ----
		Task{
			Name:        "preconsume_reconcile",
			Description: "Reconcile abandoned pre-consumption holds",
			Interval:    5 * time.Minute,
		},
		Task{
			Name:        "subscription_expiry",
			Description: "Expire subscriptions and reset subscription quotas",
			Interval:    time.Minute,
		},
		Task{
			Name:        "subscription_quota_cleanup",
			Description: "Clean up subscription pre-consume idempotency records",
			Interval:    30 * time.Minute,
		},
		Task{
			Name:        "auto_renew",
			Description: "Charge subscriptions whose renewal window is due",
			Interval:    time.Minute,
		},
		Task{
			Name:        "order_expiry",
			Description: "Expire unpaid orders and poll pending Alipay payments",
			Interval:    time.Minute,
		},
		Task{
			Name:        "log_partition_precreate",
			Description: "Pre-create usage_logs monthly partitions",
			Interval:    24 * time.Hour,
			// 白名单第 2 项：CREATE ... IF NOT EXISTS 幂等。
			ManualTriggerAllowed: true,
		},
		Task{
			Name:        "log_partition_drop",
			Description: "Drop usage_logs partitions older than the retention window",
			Interval:    24 * time.Hour,
		},
		Task{
			Name:        "audit_log_retention",
			Description: "Delete expired audit rows and erase old IP addresses",
			Interval:    24 * time.Hour,
		},
		Task{
			Name:        "task_retention",
			Description: "Delete terminal tasks beyond the retention window",
			Interval:    24 * time.Hour,
		},
		Task{
			Name:        "log_batch_flush",
			Description: "Drain the usage_logs stream into the logs database",
			Interval:    5 * time.Second,
		},
		Task{
			Name:        "rankings_snapshot",
			Description: "Aggregate and persist rankings snapshots",
			Interval:    5 * time.Minute,
			Timeout:     10 * time.Minute,
			// 白名单第 3 项：append-only 快照，重复执行只多一条 run。
			ManualTriggerAllowed: true,
		},
		Task{
			Name:        "deploy_billing_reconcile",
			Description: "Reconcile GPU deployment billing segments",
			Interval:    60 * time.Second,
		},
		Task{
			Name:        "webhook_delivery",
			Description: "Send and retry notification webhooks",
			Interval:    time.Minute,
		},
		Task{
			Name:        "email_delivery",
			Description: "Send and retry notification emails",
			Interval:    time.Minute,
		},
		Task{
			Name:        "balance_low_scan",
			Description: "Scan for low balances and enqueue notifications",
			Interval:    5 * time.Minute,
		},
		Task{
			Name:        "instance_heartbeat",
			Description: "Publish per-replica instance heartbeats",
			Interval:    30 * time.Second,
		},
		Task{
			Name:        "cache_refresh",
			Description: "Reload per-replica caches from the configuration center",
			Interval:    5 * time.Minute,
		},
		Task{
			Name:        "redemption_expiry",
			Description: "Delete expired unused redemption codes",
			Interval:    24 * time.Hour,
			// 白名单第 4 项：条件删除（status=unused AND expires_at < cutoff），幂等。
			ManualTriggerAllowed: true,
		},
		Task{
			Name:        "worker_history_cleanup",
			Description: "Delete worker task run history beyond the retention window",
			Interval:    24 * time.Hour,
			Timeout:     10 * time.Minute,
			Scheduled:   true,
			// 白名单第 5 项：按时间物理删除历史，幂等且无业务副作用。
			ManualTriggerAllowed: true,
		},
		// ---- C 类：W18 定价时段策略新增（框架调度型） ----
		Task{
			Name:        "holiday_sync",
			Description: "Sync the Chinese public holiday calendar (holiday-cn)",
			// spec §5.2.1 要求每天一次；调度器按 last_run_at + Interval 判定
			// 到期，因此新建任务行（last_run_at 为空）会在启动后首个 pass
			// 立即执行——这同时满足「启动时若当年没有数据立即补一次」。
			Interval:  24 * time.Hour,
			Timeout:   2 * time.Minute,
			Scheduled: true,
			// spec §5.2.1：手工触发沿用现有任务触发端点与权限。只拉取并整年
			// 替换日历，幂等；数据源不可达时保留旧行。
			ManualTriggerAllowed: true,
		},
		// ---- D 类：上游用量对账（框架调度型，设计 §11） ----
		Task{
			Name:        "upstream_usage_reconcile",
			Description: "Reconcile local usage logs against upstream console snapshots",
			// 每天一次：对账窗口按 lookback_days 覆盖最近若干自然日，重跑是
			// 追加新 run（旧 run 不可变），因此周期只需小于窗口长度。
			Interval: 24 * time.Hour,
			// 单账号聚合 + 逐条比较 + 落库；5 分钟覆盖正常规模，超时由调度器
			// 主动收敛，不让卡死的执行占着租约。
			Timeout:   5 * time.Minute,
			Scheduled: true,
			// 管理员可补跑：每次执行生成新 run，幂等且不改写历史判定。
			ManualTriggerAllowed: true,
		},
		// ---- E 类：模型性能定时实测（spec 2026-10-27 §5.5） ----
		Task{
			Name: "model_perf_test",
			// 周期真源是性能页的 perf.schedule.*（间隔小时数或每日时刻表），
			// 不是本目录的 Interval：12h 只是出厂时的展示默认值，首次启动经
			// EnsureTaskRows 写进 worker_task_states.interval_seconds，之后以
			// 库值为准。改这个常量只对全新库生效，升级库会继续显示旧值，
			// 因此文案写「以设置为准」而不是动态渲染周期。
			Description: "Scheduled model performance probes for all public chat models; cadence follows the performance settings page",
			// 仅用于管理面展示节奏；真实时刻由 perf.schedule.interval_hours /
			// PERF_SCHEDULE_TIMES 决定，周期由 modelperf.RunLoop 自己持有，
			// 因此 Scheduled=false。
			Interval: 12 * time.Hour,
			// 一轮要覆盖全部公开对话模型（每个 3 次探测），远超 10 分钟默认
			// 上限；手动触发由框架按租约执行，超时过短会把长轮次截断。
			Timeout:              3 * time.Hour,
			ManualTriggerAllowed: true,
		},
	)
}
