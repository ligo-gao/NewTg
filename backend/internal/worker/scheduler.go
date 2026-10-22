package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// 调度器默认参数（billing-06 §2.2 / §2.3.1）。
//
// 设计原文是 15s pass / 60s TTL / 20s 心跳 / 30s 过期清理；派发单里的
// 「60s 周期扫描 + 20s 租约」按设计数值解释为 TTL 60s + 心跳 20s（扫描
// 周期仍取设计的 15s）。差异已在任务报告登记。
const (
	DefaultPassInterval      = 15 * time.Second
	DefaultLeaseTTL          = 60 * time.Second
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultCleanupInterval   = 30 * time.Second
)

// SchedulerConfig 是调度器装配输入。
type SchedulerConfig struct {
	Registry *Registry
	Store    Store
	Lease    Lease
	// NodeID 是持照者标识（生产为进程实例 ID）。
	NodeID string
	Logger *slog.Logger
	Now    func() time.Time

	PassInterval      time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	CleanupInterval   time.Duration
}

// Scheduler 按「上次执行 + 周期」调度注册表中 Scheduled 的任务，并执行
// 管理员触发写入的 pending 行。
type Scheduler struct {
	registry *Registry
	store    Store
	lease    Lease
	nodeID   string
	log      *slog.Logger
	now      func() time.Time

	passInterval      time.Duration
	leaseTTL          time.Duration
	heartbeatInterval time.Duration
	cleanupInterval   time.Duration

	wake chan struct{}

	mu       sync.Mutex
	inFlight map[string]struct{}
	wg       sync.WaitGroup
}

// NewScheduler 构造调度器。
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Registry == nil {
		return nil, errors.New("worker: registry is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("worker: store is required")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("worker: node id is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	lease := cfg.Lease
	if lease == nil {
		lease = nopLease{}
	}
	pass := cfg.PassInterval
	if pass <= 0 {
		pass = DefaultPassInterval
	}
	ttl := cfg.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	heartbeat := cfg.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeatInterval
	}
	cleanup := cfg.CleanupInterval
	if cleanup <= 0 {
		cleanup = DefaultCleanupInterval
	}
	return &Scheduler{
		registry:          cfg.Registry,
		store:             cfg.Store,
		lease:             lease,
		nodeID:            cfg.NodeID,
		log:               logger,
		now:               now,
		passInterval:      pass,
		leaseTTL:          ttl,
		heartbeatInterval: heartbeat,
		cleanupInterval:   cleanup,
		wake:              make(chan struct{}, 1),
		inFlight:          make(map[string]struct{}),
	}, nil
}

// Run 阻塞运行调度循环，直到 ctx 结束并等待在飞执行收尾。
func (s *Scheduler) Run(ctx context.Context) error {
	passTicker := time.NewTicker(s.passInterval)
	defer passTicker.Stop()
	cleanupTicker := time.NewTicker(s.cleanupInterval)
	defer cleanupTicker.Stop()

	// 启动即跑一轮：重启后不必等第一个周期。
	s.Pass(ctx)
	s.CleanupExpired(ctx)

	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return ctx.Err()
		case <-passTicker.C:
			s.Pass(ctx)
		case <-cleanupTicker.C:
			s.CleanupExpired(ctx)
		case <-s.wake:
			s.Pass(ctx)
		}
	}
}

// Wake 请求尽快执行一次 pass；已有待处理唤醒时不再堆积。
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Wait 等待本轮已启动的执行结束（测试与停机用）。
func (s *Scheduler) Wait() { s.wg.Wait() }

// Pass 执行一次调度扫描：清理过期租约之外的「认领 + 执行」两步。
//
// 步骤与 §2.5 对齐：到期判定 → Redis 互斥 → DB CAS 认领 → 写 running 行
// 并更新 last_run_at → 每类型独立 goroutine 执行。Pass 自身不阻塞等待
// 执行业务（长任务不阻塞其它类型）。
func (s *Scheduler) Pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := s.now().UTC()
	for _, task := range s.registry.Tasks() {
		if task.Run == nil {
			continue
		}
		// §2.6 Enabled() 谓词：为假时整个 pass 跳过，既不建 run 行也不推进
		// last_run_at（轮询类任务空闲时不留运行记录）。手动 pending 行同样
		// 留到谓词恢复后由调度 pass 认领，不旁路执行。
		if task.Enabled != nil && !task.Enabled() {
			continue
		}
		if s.isInFlight(task.Name) {
			continue
		}
		pending, err := s.store.EarliestPendingRun(ctx, task.Name)
		if err != nil {
			s.log.Warn("worker: list pending runs failed", "task_type", task.Name, "error", err)
			continue
		}
		due := false
		if pending == nil && task.Scheduled && task.Interval > 0 {
			state, ok, stateErr := s.store.GetTaskState(ctx, task.Name)
			if stateErr != nil {
				s.log.Warn("worker: read task state failed", "task_type", task.Name, "error", stateErr)
				continue
			}
			if ok && !state.Enabled {
				continue
			}
			due = !ok || state.LastRunAt == nil || !now.Before(state.LastRunAt.Add(task.Interval))
		}
		if pending == nil && !due {
			continue
		}
		s.claimAndStart(ctx, task, pending)
	}
}

// claimAndStart 在通过两层互斥后启动一次执行。
func (s *Scheduler) claimAndStart(ctx context.Context, task Task, pending *Run) {
	// 第一层：Redis SET NX PX。Redis 故障不阻断（DB CAS 是权威）。
	acquired, err := s.lease.Acquire(ctx, task.Name, s.nodeID, s.leaseTTL)
	if err != nil {
		s.log.Warn("worker: redis lease unavailable; falling back to DB CAS",
			"task_type", task.Name, "error", err)
	} else if !acquired {
		return
	}

	now := s.now().UTC()
	generation, ok, err := s.store.ClaimLock(ctx, task.Name, s.nodeID, s.leaseTTL, now)
	if err != nil {
		s.log.Warn("worker: claim DB lease failed", "task_type", task.Name, "error", err)
		s.releaseLease(ctx, task.Name, 0)
		return
	}
	if !ok {
		// 另一副本在自己的 Redis 租约过期窗口内抢到了 DB 租约。
		s.releaseLease(ctx, task.Name, 0)
		return
	}

	var runID int64
	if pending != nil {
		// 管理员触发：把既有 pending 行推进 running，不新建第二行。
		if err := s.store.MarkRunRunning(ctx, pending.ID, s.nodeID, now); err != nil {
			s.log.Warn("worker: claim pending run failed", "task_type", task.Name, "run_id", pending.ID, "error", err)
			s.releaseLease(ctx, task.Name, generation)
			return
		}
		runID = pending.ID
	} else {
		started := now
		id, err := s.store.CreateRun(ctx, CreateRunInput{
			TaskType:  task.Name,
			Status:    RunRunning,
			Trigger:   TriggerSchedule,
			NodeID:    s.nodeID,
			StartedAt: &started,
		})
		if err != nil {
			s.log.Warn("worker: create scheduled run failed", "task_type", task.Name, "error", err)
			s.releaseLease(ctx, task.Name, generation)
			return
		}
		runID = id
	}

	// 立即推进 last_run_at，防止下一 pass 重复排队（§2.5 第 3 步）。
	nextDue := nextDueAt(now, task.Interval)
	if err := s.store.UpdateTaskAfterRun(ctx, task.Name, now, "", nextDue); err != nil {
		s.log.Warn("worker: update task after claim failed", "task_type", task.Name, "error", err)
	}

	s.mu.Lock()
	s.inFlight[task.Name] = struct{}{}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.execute(ctx, task, runID, generation, now)
		s.mu.Lock()
		delete(s.inFlight, task.Name)
		s.mu.Unlock()
	}()
}

// execute 运行一次任务并写终态。
func (s *Scheduler) execute(parent context.Context, task Task, runID, generation int64, startedAt time.Time) {
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = DefaultTaskTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// 心跳：续 Redis 与 DB 两层；任一层明确「已被他人持有」就取消 ctx，
	// 让长任务主动收敛（§2.3.1）。Redis 报错不取消（DB 仍是权威）。
	stopHeartbeat := make(chan struct{})
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ok, err := s.lease.Renew(ctx, task.Name, s.nodeID, s.leaseTTL); err != nil {
					s.log.Warn("worker: redis lease renew failed; DB lease remains authoritative",
						"task_type", task.Name, "run_id", runID, "error", err)
				} else if !ok {
					s.log.Warn("worker: redis lease was taken over; cancelling task",
						"task_type", task.Name, "run_id", runID, "generation", generation)
					cancel()
					return
				}
				if ok, err := s.store.RenewLock(ctx, task.Name, s.nodeID, generation, s.leaseTTL, s.now().UTC()); err != nil {
					s.log.Warn("worker: DB lease renew failed", "task_type", task.Name, "run_id", runID, "error", err)
				} else if !ok {
					s.log.Warn("worker: DB lease was taken over; cancelling task",
						"task_type", task.Name, "run_id", runID, "generation", generation)
					cancel()
					return
				}
			}
		}
	}()

	result, runErr, errClass := s.invoke(ctx, task)
	close(stopHeartbeat)
	heartbeatWG.Wait()

	finishedAt := s.now().UTC()
	status := RunSucceeded
	if runErr != nil {
		status = RunFailed
		s.log.Warn("worker: task failed",
			"task_type", task.Name, "run_id", runID, "error_class", errClass)
	}
	// 终态写入必须脱离任务 ctx：超时/心跳接管/停机都会取消它，若复用同一个
	// ctx，running 行会卡在终态之前（只剩过期租约清理兜底），运行历史就
	// 丢失了「失败还是成功」的事实。WithoutCancel + 有界超时保证任何出口
	// 都能落一行终态。
	finalizeCtx, finalizeCancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	if err := s.store.FinishRun(finalizeCtx, runID, status, errClass, result, finishedAt); err != nil {
		// 写终态失败（连接已关/停机窗口）：留 running 行给过期租约清理收敛。
		s.log.Error("worker: finish run failed", "task_type", task.Name, "run_id", runID, "error", err)
	}
	nextDue := nextDueAt(startedAt, task.Interval)
	if err := s.store.UpdateTaskAfterRun(finalizeCtx, task.Name, startedAt, status, nextDue); err != nil {
		s.log.Warn("worker: update task after run failed", "task_type", task.Name, "error", err)
	}
	finalizeCancel()
	s.releaseLease(context.WithoutCancel(parent), task.Name, generation)
}

// invoke 执行任务函数并把错误映射成固定分类。
func (s *Scheduler) invoke(ctx context.Context, task Task) (result Result, err error, class string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.log.Error("worker: task panicked", "task_type", task.Name, "panic", recovered)
			result = nil
			err = errors.New(ErrClassPanic)
			class = ErrClassPanic
		}
	}()
	result, err = task.Run(ctx)
	if err == nil {
		return result, nil, ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return result, err, ErrClassTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return result, err, ErrClassCancelled
	default:
		return result, err, ErrClassExecution
	}
}

// CleanupExpired 把过期租约对应的 running 运行标记为失败（§2.3.1）。
func (s *Scheduler) CleanupExpired(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	count, err := s.store.ExpireRunningRuns(ctx, s.now().UTC())
	if err != nil {
		s.log.Warn("worker: expire running runs failed", "error", err)
		return
	}
	if count > 0 {
		s.log.Warn("worker: marked runs failed after lease expiry", "count", count)
	}
}

func (s *Scheduler) releaseLease(ctx context.Context, taskType string, generation int64) {
	if err := s.lease.Release(ctx, taskType, s.nodeID); err != nil {
		s.log.Warn("worker: redis lease release failed", "task_type", taskType, "error", err)
	}
	if generation > 0 {
		if err := s.store.ReleaseLock(ctx, taskType, s.nodeID, generation, s.now().UTC()); err != nil {
			s.log.Warn("worker: DB lease release failed", "task_type", taskType, "error", err)
		}
	}
}

func (s *Scheduler) isInFlight(taskType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inFlight[taskType]
	return ok
}

// nextDueAt 返回派生列 next_due_at：非周期任务为 NULL。
func nextDueAt(startedAt time.Time, interval time.Duration) *time.Time {
	if interval <= 0 {
		return nil
	}
	due := startedAt.Add(interval).UTC()
	return &due
}
