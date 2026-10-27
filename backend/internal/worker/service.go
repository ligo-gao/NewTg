package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// 管理面错误：handler 据此映射 404/403/409，绝不模糊匹配任务名。
var (
	ErrTaskNotFound         = errors.New("worker: task not found")
	ErrTaskManualNotAllowed = errors.New("worker: task does not allow manual trigger")
	ErrTaskAlreadyRunning   = errors.New("worker: task is already running")
	ErrTaskDisabled         = errors.New("worker: task is disabled")
)

// TaskBlockedError 表示任务被运行时谓词阻断（不是运营停用）。Code 是冻结的
// 管理面错误码（如 log_partition_drop_disabled），handler 直接映射 409。
type TaskBlockedError struct {
	Code string
}

func (e *TaskBlockedError) Error() string {
	if e == nil || e.Code == "" {
		return "worker: task is blocked"
	}
	return "worker: task is blocked: " + e.Code
}

// BlockedCodeOf 返回 TaskBlockedError 的固定码；其他错误返回空串。
func BlockedCodeOf(err error) string {
	var blocked *TaskBlockedError
	if errors.As(err, &blocked) && blocked != nil {
		return blocked.Code
	}
	return ""
}

// BlockedCodeWorkerUnavailable 是「没有存活 worker 实例」的固定错误码。
//
// 网关进程不绑定任务的 Run，手动触发写下的 pending 行只能由 worker 进程
// 认领；没有 worker 在线时该行永远不会被执行。这个码同时用于两处：触发被
// 拒时的 409 problem type 后缀，以及摘要里的 blocked_reason。
const BlockedCodeWorkerUnavailable = "worker_unavailable"

// WakeupChannel 是「管理员触发后立即唤醒调度 pass」的 Redis 频道。
//
// 网关进程与 worker 进程分离，进程内 channel 无法跨副本；用 pub/sub 让
// 触发在 ≤1 个 pass 的极小延迟内被看到。订阅失败/消息丢失由 15s 周期
// pass 兜底——唤醒只是延迟优化，不是正确性依赖。
const WakeupChannel = "newtg:worker:wakeup"

// PublishWakeup 发布一次唤醒（best-effort）。
func PublishWakeup(ctx context.Context, client *redis.Client, taskType string) error {
	if client == nil {
		return nil
	}
	return client.Publish(ctx, WakeupChannel, taskType).Err()
}

// ServiceConfig 是管理面服务的装配输入。
type ServiceConfig struct {
	Registry *Registry
	Store    Store
	Lease    Lease
	Logger   *slog.Logger
	// Wakeup 可选：手动触发成功后发布唤醒信号（nil 时只靠周期 pass）。
	Wakeup func(ctx context.Context, taskType string) error
	// RuntimeAvailability 可选：报告「能执行手动任务的主体（worker 进程）是否在线」。
	// nil 表示本进程就是执行者（worker 角色），恒为可用。
	RuntimeAvailability func(ctx context.Context) (available bool, reason string)
}

// Service 实现列表/详情/触发（billing-06 §13.5.1）。
type Service struct {
	registry         *Registry
	store            Store
	lease            Lease
	log              *slog.Logger
	wakeup           func(ctx context.Context, taskType string) error
	runtimeAvailable func(ctx context.Context) (bool, string)
}

// NewService 构造服务。
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Registry == nil {
		return nil, errors.New("worker: registry is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("worker: store is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lease := cfg.Lease
	if lease == nil {
		lease = nopLease{}
	}
	return &Service{
		registry:         cfg.Registry,
		store:            cfg.Store,
		lease:            lease,
		log:              logger,
		wakeup:           cfg.Wakeup,
		runtimeAvailable: cfg.RuntimeAvailability,
	}, nil
}

// runtimeAvailability 返回「能执行手动任务的主体是否在线」。
//
// 未注入探针意味着本进程就是执行者（worker 角色），恒可用；不可用但探针
// 没给原因时回退固定码，保证 TaskBlockedError.Code 永远非空——handler 直接
// 把它拼进 problem type，空码会退化成 task_disabled 之外的模糊 409。
func (s *Service) runtimeAvailability(ctx context.Context) (bool, string) {
	if s.runtimeAvailable == nil {
		return true, ""
	}
	available, reason := s.runtimeAvailable(ctx)
	if !available && reason == "" {
		reason = BlockedCodeWorkerUnavailable
	}
	return available, reason
}

// List 返回注册表全集的摘要分页（含无调度行者的默认值）。
func (s *Service) List(ctx context.Context, page, pageSize int) (TaskPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	tasks := s.registry.Tasks()
	states, err := s.store.ListTaskStates(ctx)
	if err != nil {
		return TaskPage{}, err
	}
	stateByName := make(map[string]TaskState, len(states))
	for _, state := range states {
		stateByName[state.TaskType] = state
	}

	// 可用性探针每页只问一次：列表有 30+ 个任务，逐个探 Redis 会让一次
	// 管理面刷新打出一串 SCAN/HGETALL。同一请求内结果也应当一致。
	available, reason := s.runtimeAvailability(ctx)

	items := make([]TaskSummary, 0, len(tasks))
	for _, task := range tasks {
		state, ok := stateByName[task.Name]
		summary, summaryErr := s.summary(ctx, task, state, ok, available, reason)
		if summaryErr != nil {
			return TaskPage{}, summaryErr
		}
		items = append(items, summary)
	}
	total := len(items)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return TaskPage{Items: items[start:end], Total: total, Page: page, PageSize: pageSize}, nil
}

// Detail 返回摘要 + 最近 N 次运行。
func (s *Service) Detail(ctx context.Context, name string, page, pageSize int) (TaskDetail, error) {
	task, ok := s.registry.Get(name)
	if !ok {
		return TaskDetail{}, ErrTaskNotFound
	}
	page, pageSize = normalizePage(page, pageSize)
	state, hasState, err := s.store.GetTaskState(ctx, name)
	if err != nil {
		return TaskDetail{}, err
	}
	// 与 List 同口径：详情也只探一次。
	available, reason := s.runtimeAvailability(ctx)
	summary, err := s.summary(ctx, task, state, hasState, available, reason)
	if err != nil {
		return TaskDetail{}, err
	}
	runs, total, err := s.store.ListRuns(ctx, name, pageSize, (page-1)*pageSize)
	if err != nil {
		return TaskDetail{}, err
	}
	if runs == nil {
		runs = []Run{}
	}
	return TaskDetail{
		Summary: summary,
		Runs:    RunPage{Items: runs, Total: total, Page: page, PageSize: pageSize},
	}, nil
}

// TriggerResult 是 202 响应体。
type TriggerResult struct {
	RunID    int64  `json:"run_id"`
	TaskType string `json:"task_type"`
	Trigger  string `json:"trigger"`
}

// Trigger 只入队（写 pending 行 + 唤醒），绝不旁路执行。
//
// 顺序固定：名称 → 白名单 → 停用 → 运行中 → 入队。任何一步失败都不写行，
// 保证 403/404/409 的副作用为零（§13.5.1 与 TC-F-08-21-07）。
func (s *Service) Trigger(ctx context.Context, name string) (TriggerResult, error) {
	task, ok := s.registry.Get(name)
	if !ok {
		return TriggerResult{}, ErrTaskNotFound
	}
	if !task.ManualTriggerAllowed {
		return TriggerResult{}, ErrTaskManualNotAllowed
	}
	// 网关进程不绑定 Run：写下的 pending 行只能由 worker 认领。worker 不在线
	// 时入队只会留下一条永远不会执行的 pending 行（§13.5 明确禁止这种误导），
	// 所以必须在写行之前返回，保证 409 的副作用为零。
	if task.Run == nil {
		if available, reason := s.runtimeAvailability(ctx); !available {
			return TriggerResult{}, &TaskBlockedError{Code: reason}
		}
	}
	// 运行时谓词（如 log.partition_drop_enabled）为假时，触发必须当场 409：
	// 入队一条永远不会执行的 pending 行会让管理员误以为任务已排期。
	if task.Enabled != nil && !task.Enabled() {
		code := task.BlockedCode
		if code == "" {
			code = "task_disabled"
		}
		return TriggerResult{}, &TaskBlockedError{Code: code}
	}
	state, hasState, err := s.store.GetTaskState(ctx, name)
	if err != nil {
		return TriggerResult{}, err
	}
	if hasState && !state.Enabled {
		return TriggerResult{}, ErrTaskDisabled
	}
	running, err := s.isRunning(ctx, name)
	if err != nil {
		return TriggerResult{}, err
	}
	if running {
		return TriggerResult{}, ErrTaskAlreadyRunning
	}
	// 已有一条 pending 行也视为「已入队」：允许重复触发会让同一任务在队列里
	// 堆积多条手动运行，调度 pass 逐条执行后表现为「点一次跑了多次」。
	pending, err := s.store.EarliestPendingRun(ctx, name)
	if err != nil {
		return TriggerResult{}, err
	}
	if pending != nil {
		return TriggerResult{}, ErrTaskAlreadyRunning
	}
	runID, err := s.store.CreateRun(ctx, CreateRunInput{
		TaskType: name,
		Status:   RunPending,
		Trigger:  TriggerManual,
	})
	if err != nil {
		return TriggerResult{}, err
	}
	if s.wakeup != nil {
		if wakeErr := s.wakeup(ctx, name); wakeErr != nil {
			// 唤醒失败不改写入结果：pending 行仍在，下一个 15s pass 会认领。
			s.log.Warn("worker: wakeup publish failed", "task_type", name, "error", wakeErr)
		}
	}
	return TriggerResult{RunID: runID, TaskType: name, Trigger: TriggerManual}, nil
}

func (s *Service) summary(ctx context.Context, task Task, state TaskState, hasState bool, available bool, reason string) (TaskSummary, error) {
	interval := int(task.Interval / time.Second)
	summary := TaskSummary{
		TaskType:             task.Name,
		Description:          task.Description,
		Enabled:              true,
		IntervalSeconds:      interval,
		ManualTriggerAllowed: task.ManualTriggerAllowed,
		Concurrency:          string(task.Concurrency),
		// 只有「网关有按钮、但执行者在另一进程」的任务才需要运行期可用性：
		// 手动白名单 + 本进程未绑定 Run。其它任务根本不可手动触发，标成
		// 不可用只会误导管理员去追一个不存在的故障。
		Available: true,
	}
	if task.ManualTriggerAllowed && task.Run == nil {
		summary.Available = available
		if !available {
			summary.BlockedReason = reason
		}
	}
	if hasState {
		summary.Enabled = state.Enabled
		summary.IntervalSeconds = state.IntervalSeconds
		summary.LastRunAt = state.LastRunAt
		summary.LastStatus = state.LastStatus
		summary.NextDueAt = state.NextDueAt
		summary.LastDurationMS = state.LastDurationMS
		if state.HolderID != "" {
			summary.HolderID = truncateHolder(state.HolderID)
		}
	}
	running, err := s.isRunning(ctx, task.Name)
	if err != nil {
		return TaskSummary{}, err
	}
	summary.Running = running
	// 只展示「当前确实持有租约」的持有者；过期的 holder 会让管理员误以为
	// 任务仍在运行（与 running=false 自相矛盾）。
	if !running {
		summary.HolderID = ""
	}
	return summary, nil
}

// isRunning 以 Redis 租约为第一判据，DB `lease_expires_at > now()` 作
// Redis 不可用/过期窗口内的回退。两处都不使用进程时钟：跨副本时钟快几秒
// 会把已释放的租约误判成「仍在运行」，让合法触发持续 409。
func (s *Service) isRunning(ctx context.Context, name string) (bool, error) {
	active, err := s.lease.Active(ctx, name)
	if err == nil && active {
		return true, nil
	}
	if err != nil {
		s.log.Warn("worker: lease probe failed; falling back to DB lease", "task_type", name, "error", err)
	}
	dbActive, dbErr := s.store.LeaseActive(ctx, name)
	if dbErr != nil {
		s.log.Warn("worker: DB lease probe failed", "task_type", name, "error", dbErr)
		return false, nil
	}
	return dbActive, nil
}

func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

// truncateHolder 截断节点标识：完整实例 ID 可能含主机名/进程信息，列表只
// 需要足够区分（设计 §13.5.1「holder_id 仅 admin 可见、截断展示」）。
func truncateHolder(holder string) string {
	const max = 64
	if len(holder) <= max {
		return holder
	}
	return holder[:max]
}
