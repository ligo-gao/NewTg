package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 运行状态与触发来源的冻结枚举。与迁移里的 CHECK 逐字对应（schema 集成
// 测试会用同一组常量对账）。
const (
	RunPending   = "pending"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"

	// 失败分类只写固定值；原始 err 文本可能含上游响应/凭据，绝不落库。
	ErrClassTimeout      = "timeout"
	ErrClassLeaseExpired = "task lease expired"
	ErrClassExecution    = "execution_error"
	ErrClassCancelled    = "cancelled"
	ErrClassPanic        = "panic"
	ErrClassInternal     = "internal_error"
)

// TaskState 是 worker_tasks 行与最近一次运行/当前租约的合并投影。
type TaskState struct {
	TaskType        string
	IntervalSeconds int
	Enabled         bool
	Shard           int
	LastRunAt       *time.Time
	LastStatus      *string
	NextDueAt       *time.Time

	// 租约投影（worker_task_locks）；无行时 HolderID 为空、Generation=0。
	HolderID       string
	LeaseExpiresAt *time.Time
	HeartbeatAt    *time.Time
	Generation     int64

	// 最近一次运行的耗时（毫秒）；没有完成过运行时为 nil。
	LastDurationMS *int64
}

// Run 是 worker_task_runs 的一行。
type Run struct {
	ID         int64
	TaskType   string
	Status     string
	NodeID     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	Error      string
	Result     Result
	Trigger    string
}

// TaskPage / RunPage 是管理面分页信封（与 OpenAPI Page<T> 同形）。
type TaskPage struct {
	Items    []TaskSummary `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

// RunPage 是运行历史分页。
type RunPage struct {
	Items    []Run `json:"items"`
	Total    int   `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

// TaskSummary 是 GET /api/system/tasks 的列表项（billing-06 §13.5.1）。
type TaskSummary struct {
	TaskType             string     `json:"task_type"`
	Description          string     `json:"description"`
	Enabled              bool       `json:"enabled"`
	IntervalSeconds      int        `json:"interval_seconds"`
	LastRunAt            *time.Time `json:"last_run_at"`
	LastStatus           *string    `json:"last_status"`
	NextDueAt            *time.Time `json:"next_due_at"`
	Running              bool       `json:"running"`
	HolderID             string     `json:"holder_id,omitempty"`
	LastDurationMS       *int64     `json:"last_duration_ms"`
	ManualTriggerAllowed bool       `json:"manual_trigger_allowed"`
	Concurrency          string     `json:"concurrency"`
}

// TaskDetail 是 GET /api/system/tasks/{name} 的响应。
type TaskDetail struct {
	Summary TaskSummary `json:"summary"`
	Runs    RunPage     `json:"runs"`
}

// 数据层哨兵错误。
var (
	// ErrTaskStateNotFound：worker_tasks 无该行（注册表有、调度行尚未创建）。
	ErrTaskStateNotFound = errors.New("worker: task state not found")
	// ErrLeaseNotHeld：续约/释放时 CAS 未命中（租约已被抢占或已过期）。
	ErrLeaseNotHeld = errors.New("worker: task lease is not held by this generation")
)

// Store 是任务框架的持久化接缝。生产实现是 SQLStore；单测可注入内存实现。
type Store interface {
	EnsureTaskRows(ctx context.Context, tasks []Task) error
	ListTaskStates(ctx context.Context) ([]TaskState, error)
	GetTaskState(ctx context.Context, taskType string) (TaskState, bool, error)

	CreateRun(ctx context.Context, in CreateRunInput) (int64, error)
	MarkRunRunning(ctx context.Context, id int64, nodeID string, startedAt time.Time) error
	FinishRun(ctx context.Context, id int64, status, errClass string, result Result, finishedAt time.Time) error
	ListRuns(ctx context.Context, taskType string, limit, offset int) ([]Run, int, error)
	EarliestPendingRun(ctx context.Context, taskType string) (*Run, error)

	ClaimLock(ctx context.Context, taskType, holder string, ttl time.Duration, now time.Time) (int64, bool, error)
	RenewLock(ctx context.Context, taskType, holder string, generation int64, ttl time.Duration, now time.Time) (bool, error)
	ReleaseLock(ctx context.Context, taskType, holder string, generation int64, now time.Time) error
	GetLock(ctx context.Context, taskType string) (TaskState, bool, error)

	UpdateTaskAfterRun(ctx context.Context, taskType string, startedAt time.Time, status string, nextDueAt *time.Time) error
	ExpireRunningRuns(ctx context.Context, now time.Time) (int64, error)
	DeleteRunsBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// CreateRunInput 是一次运行行的创建输入。
type CreateRunInput struct {
	TaskType   string
	Status     string
	Trigger    string
	NodeID     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	Error      string
	Result     Result
}

// SQLStore 是 Store 的 PostgreSQL 实现。
type SQLStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLStore 构造仓储。db 必填；now 仅测试注入。
func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("worker: database handle is required")
	}
	return &SQLStore{db: db, now: time.Now}, nil
}

func (s *SQLStore) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// EnsureTaskRows 把注册表同步到 worker_tasks：缺行则建，已有行只刷新周期。
// enabled 不覆盖——它是运营意图，重启不应把管理员停用的任务重新打开。
func (s *SQLStore) EnsureTaskRows(ctx context.Context, tasks []Task) error {
	for _, task := range tasks {
		seconds := int(task.Interval / time.Second)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO worker_tasks (task_type, interval_seconds, enabled, shard, created_at, updated_at)
			VALUES ($1, $2, true, 0, now(), now())
			ON CONFLICT (task_type) DO UPDATE
			   SET interval_seconds = EXCLUDED.interval_seconds,
			       updated_at = now()`, task.Name, seconds); err != nil {
			return fmt.Errorf("worker: ensure task row %q: %w", task.Name, err)
		}
	}
	return nil
}

const taskStateSelect = `
	SELECT t.task_type, t.interval_seconds, t.enabled, t.shard,
	       t.last_run_at, t.last_status, t.next_due_at,
	       COALESCE(l.holder_id, ''), l.lease_expires_at, COALESCE(l.generation, 0),
	       r.started_at, r.finished_at
	  FROM worker_tasks t
	  LEFT JOIN worker_task_locks l ON l.task_type = t.task_type
	  LEFT JOIN LATERAL (
	       SELECT started_at, finished_at
	         FROM worker_task_runs
	        WHERE task_type = t.task_type AND status IN ('succeeded','failed')
	        ORDER BY id DESC
	        LIMIT 1
	  ) r ON true`

func scanTaskState(row interface{ Scan(...any) error }) (TaskState, error) {
	var state TaskState
	var lease, lastRun, nextDue sql.NullTime
	var lastStatus, holder sql.NullString
	var lastStarted, lastFinished sql.NullTime
	err := row.Scan(&state.TaskType, &state.IntervalSeconds, &state.Enabled, &state.Shard,
		&lastRun, &lastStatus, &nextDue, &holder, &lease, &state.Generation,
		&lastStarted, &lastFinished)
	if err != nil {
		return TaskState{}, err
	}
	if lastRun.Valid {
		t := lastRun.Time
		state.LastRunAt = &t
	}
	if lastStatus.Valid {
		v := lastStatus.String
		state.LastStatus = &v
	}
	if nextDue.Valid {
		t := nextDue.Time
		state.NextDueAt = &t
	}
	if holder.Valid {
		state.HolderID = holder.String
	}
	if lease.Valid {
		t := lease.Time
		state.LeaseExpiresAt = &t
	}
	if lastStarted.Valid && lastFinished.Valid && !lastFinished.Time.Before(lastStarted.Time) {
		ms := lastFinished.Time.Sub(lastStarted.Time).Milliseconds()
		state.LastDurationMS = &ms
	}
	return state, nil
}

// ListTaskStates 返回全部调度行（含租约与最近耗时投影）。
func (s *SQLStore) ListTaskStates(ctx context.Context) ([]TaskState, error) {
	rows, err := s.db.QueryContext(ctx, taskStateSelect+` ORDER BY t.task_type ASC`)
	if err != nil {
		return nil, fmt.Errorf("worker: list task states: %w", err)
	}
	defer rows.Close()
	var out []TaskState
	for rows.Next() {
		state, err := scanTaskState(rows)
		if err != nil {
			return nil, fmt.Errorf("worker: scan task state: %w", err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker: iterate task states: %w", err)
	}
	return out, nil
}

// GetTaskState 返回单个调度行。
func (s *SQLStore) GetTaskState(ctx context.Context, taskType string) (TaskState, bool, error) {
	state, err := scanTaskState(s.db.QueryRowContext(ctx, taskStateSelect+` WHERE t.task_type = $1`, taskType))
	if errors.Is(err, sql.ErrNoRows) {
		return TaskState{}, false, nil
	}
	if err != nil {
		return TaskState{}, false, fmt.Errorf("worker: get task state %q: %w", taskType, err)
	}
	return state, true, nil
}

// CreateRun 插入一行运行记录（pending 或 running）。
func (s *SQLStore) CreateRun(ctx context.Context, in CreateRunInput) (int64, error) {
	var result any
	if len(in.Result) > 0 {
		raw, err := json.Marshal(in.Result)
		if err != nil {
			return 0, fmt.Errorf("worker: marshal run result: %w", err)
		}
		result = raw
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO worker_task_runs
			(task_type, status, node_id, started_at, finished_at, error, result, trigger, created_at, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7::jsonb, $8, now(), now())
		RETURNING id`,
		in.TaskType, in.Status, in.NodeID, in.StartedAt, in.FinishedAt, in.Error, result, in.Trigger).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("worker: create run for %q: %w", in.TaskType, err)
	}
	return id, nil
}

// MarkRunRunning 把 pending 行推进到 running（管理员触发的执行路径）。
func (s *SQLStore) MarkRunRunning(ctx context.Context, id int64, nodeID string, startedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_task_runs
		   SET status = 'running', node_id = $2, started_at = $3, updated_at = now()
		 WHERE id = $1 AND status = 'pending'`, id, nodeID, startedAt)
	if err != nil {
		return fmt.Errorf("worker: mark run %d running: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("worker: mark run %d running rows: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("worker: run %d is not pending", id)
	}
	return nil
}

// FinishRun 写入终态；只接受 succeeded/failed。
func (s *SQLStore) FinishRun(ctx context.Context, id int64, status, errClass string, result Result, finishedAt time.Time) error {
	if status != RunSucceeded && status != RunFailed {
		return fmt.Errorf("worker: invalid terminal status %q", status)
	}
	var payload any
	if len(result) > 0 {
		raw, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("worker: marshal run result: %w", err)
		}
		payload = raw
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE worker_task_runs
		   SET status = $2, finished_at = $3, error = NULLIF($4, ''),
		       result = COALESCE($5::jsonb, result), updated_at = now()
		 WHERE id = $1`, id, status, finishedAt, truncateError(errClass), payload); err != nil {
		return fmt.Errorf("worker: finish run %d: %w", id, err)
	}
	return nil
}

// truncateError 给 error 列兜底（CHECK 500 字符）；分类值都很短。
func truncateError(value string) string {
	const max = 500
	if len(value) <= max {
		return value
	}
	return value[:max]
}

// ListRuns 返回某任务的运行历史（新→旧）与总数。
func (s *SQLStore) ListRuns(ctx context.Context, taskType string, limit, offset int) ([]Run, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM worker_task_runs WHERE task_type = $1`, taskType).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("worker: count runs for %q: %w", taskType, err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_type, status, COALESCE(node_id, ''), started_at, finished_at,
		       COALESCE(error, ''), result, trigger
		  FROM worker_task_runs
		 WHERE task_type = $1
		 ORDER BY COALESCE(started_at, created_at) DESC, id DESC
		 LIMIT $2 OFFSET $3`, taskType, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("worker: list runs for %q: %w", taskType, err)
	}
	defer rows.Close()
	out := make([]Run, 0, limit)
	for rows.Next() {
		var run Run
		var started, finished sql.NullTime
		var rawResult []byte
		if err := rows.Scan(&run.ID, &run.TaskType, &run.Status, &run.NodeID,
			&started, &finished, &run.Error, &rawResult, &run.Trigger); err != nil {
			return nil, 0, fmt.Errorf("worker: scan run: %w", err)
		}
		if started.Valid {
			t := started.Time
			run.StartedAt = &t
		}
		if finished.Valid {
			t := finished.Time
			run.FinishedAt = &t
		}
		if len(rawResult) > 0 {
			_ = json.Unmarshal(rawResult, &run.Result)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("worker: iterate runs: %w", err)
	}
	return out, total, nil
}

// EarliestPendingRun 返回最早一条 pending run（管理员触发队列）。
func (s *SQLStore) EarliestPendingRun(ctx context.Context, taskType string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, task_type, status, COALESCE(node_id, ''), started_at, finished_at,
		       COALESCE(error, ''), result, trigger
		  FROM worker_task_runs
		 WHERE task_type = $1 AND status = 'pending'
		 ORDER BY id ASC
		 LIMIT 1`, taskType)
	var run Run
	var started, finished sql.NullTime
	var rawResult []byte
	err := row.Scan(&run.ID, &run.TaskType, &run.Status, &run.NodeID,
		&started, &finished, &run.Error, &rawResult, &run.Trigger)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: earliest pending run for %q: %w", taskType, err)
	}
	if started.Valid {
		t := started.Time
		run.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		run.FinishedAt = &t
	}
	if len(rawResult) > 0 {
		_ = json.Unmarshal(rawResult, &run.Result)
	}
	return &run, nil
}

// ClaimLock 是 §2.3.1 的 CAS 认领：租约已过期或持有者是自己时抢占成功。
// 返回新的 generation；第二返回值为 false 表示被其他持有者占用。
func (s *SQLStore) ClaimLock(ctx context.Context, taskType, holder string, ttl time.Duration, now time.Time) (int64, bool, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO worker_task_locks
			(task_type, holder_id, lease_expires_at, heartbeat_at, generation, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, now(), now())
		ON CONFLICT (task_type) DO UPDATE
		   SET holder_id = EXCLUDED.holder_id,
		       lease_expires_at = EXCLUDED.lease_expires_at,
		       heartbeat_at = EXCLUDED.heartbeat_at,
		       generation = worker_task_locks.generation + 1,
		       updated_at = now()
		 WHERE worker_task_locks.lease_expires_at IS NULL
		    OR worker_task_locks.lease_expires_at < $4
		    OR worker_task_locks.holder_id = $2
		RETURNING generation`,
		taskType, holder, now.Add(ttl), now).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("worker: claim lock %q: %w", taskType, err)
	}
	return generation, true, nil
}

// RenewLock 续约：必须同时匹配 holder 与 generation，且租约尚未过期。
func (s *SQLStore) RenewLock(ctx context.Context, taskType, holder string, generation int64, ttl time.Duration, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_task_locks
		   SET lease_expires_at = $4, heartbeat_at = $5, updated_at = now()
		 WHERE task_type = $1 AND holder_id = $2 AND generation = $3
		   AND lease_expires_at > $5`, taskType, holder, generation, now.Add(ttl), now)
	if err != nil {
		return false, fmt.Errorf("worker: renew lock %q: %w", taskType, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("worker: renew lock %q rows: %w", taskType, err)
	}
	return affected == 1, nil
}

// ReleaseLock 释放：把到期时间置为 now（§2.3.1），下一周期可立即认领。
func (s *SQLStore) ReleaseLock(ctx context.Context, taskType, holder string, generation int64, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE worker_task_locks
		   SET lease_expires_at = $4, updated_at = now()
		 WHERE task_type = $1 AND holder_id = $2 AND generation = $3`,
		taskType, holder, generation, now); err != nil {
		return fmt.Errorf("worker: release lock %q: %w", taskType, err)
	}
	return nil
}

// GetLock 返回租约投影（复用 TaskState 的锁字段）。
func (s *SQLStore) GetLock(ctx context.Context, taskType string) (TaskState, bool, error) {
	var state TaskState
	var lease, heartbeat sql.NullTime
	var holderText sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT task_type, holder_id, lease_expires_at, heartbeat_at, generation
		  FROM worker_task_locks WHERE task_type = $1`, taskType).Scan(
		&state.TaskType, &holderText, &lease, &heartbeat, &state.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskState{}, false, nil
	}
	if err != nil {
		return TaskState{}, false, fmt.Errorf("worker: get lock %q: %w", taskType, err)
	}
	if holderText.Valid {
		state.HolderID = holderText.String
	}
	if lease.Valid {
		t := lease.Time
		state.LeaseExpiresAt = &t
	}
	if heartbeat.Valid {
		t := heartbeat.Time
		state.HeartbeatAt = &t
	}
	return state, true, nil
}

// UpdateTaskAfterRun 更新调度目录的 last_run_at / last_status / next_due_at。
func (s *SQLStore) UpdateTaskAfterRun(ctx context.Context, taskType string, startedAt time.Time, status string, nextDueAt *time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE worker_tasks
		   SET last_run_at = $2,
		       -- 认领时只推进 last_run_at；终态由执行结束回写。空状态保留旧值，
		       -- 避免为「running」新增一个 CHECK 允许值（设计只有三种终态）。
		       last_status = COALESCE(NULLIF($3, ''), last_status),
		       next_due_at = $4, updated_at = now()
		 WHERE task_type = $1`, taskType, startedAt, status, nextDueAt); err != nil {
		return fmt.Errorf("worker: update task %q after run: %w", taskType, err)
	}
	return nil
}

// ExpireRunningRuns 把「租约已过期但仍标记 running」的运行标记失败。
//
// 这是崩溃/强杀后的收敛点：执行者来不及写终态，只有过期租约能证明它已
// 不再持有执行权。固定错误文案与设计 §2.3.1 一致。
func (s *SQLStore) ExpireRunningRuns(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_task_runs r
		   SET status = 'failed', finished_at = $1, error = $2, updated_at = now()
		 WHERE r.status = 'running'
		   AND EXISTS (
		       SELECT 1 FROM worker_task_locks l
		        WHERE l.task_type = r.task_type
		          AND (l.lease_expires_at IS NULL OR l.lease_expires_at < $1))`,
		now, ErrClassLeaseExpired)
	if err != nil {
		return 0, fmt.Errorf("worker: expire running runs: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("worker: expire running runs rows: %w", err)
	}
	return affected, nil
}

// DeleteRunsBefore 删除 started_at（或 pending 的 created_at）早于 cutoff 的行。
func (s *SQLStore) DeleteRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM worker_task_runs
		 WHERE COALESCE(started_at, created_at) < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("worker: delete old runs: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("worker: delete old runs rows: %w", err)
	}
	return affected, nil
}
