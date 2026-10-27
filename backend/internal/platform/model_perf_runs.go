package platform

// 定时性能探测轮次的运行记录（spec 2026-10-27 复审 final-fix-1）。
//
// 【为什么需要本文件】定时轮次由 modelperf.Scheduler.RunLoop 自己驱动，绕开了
// internal/worker 的任务框架（框架只执行 Scheduled 任务与手动触发）。结果是
// worker_task_runs 里只有手动触发的记录、worker_tasks.last_run_at / last_status
// 永远为空：管理面「系统任务」把 model_perf_test 显示成「从未运行」，而 worker
// 按 PERF_SCHEDULE_TIMES（或性能页保存的间隔）真实在跑。首页性能表整列变成「—」时，管理员的
// 第一诊断入口给出的正是错误信息。这里把每轮结果按框架的行形状补写进去。
//
// 【为什么直接写终态行】框架的 scheduled 路径先建 running 行、执行后再 FinishRun。
// 但本钩子在 RunOnce 返回后才被调用，轮次已经结束；若仍分两步写，第二步失败会
// 留下一条永远 running 的行——model_perf_test 不使用 worker_task_locks 租约，
// ExpireRunningRuns 收不回来。一次 CreateRun 直接写终态，终态行与框架写出的
// 行逐字段等价（见 newModelPerfRoundRecorder 的字段口径）。

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/modelperf"
	"newtg/gateway/internal/worker"
)

const (
	// modelPerfTaskType 必须与注册表里的任务名逐字一致：运行记录与目录行
	// 靠 task_type 关联（管理面按它查询历史）。
	modelPerfTaskType = "model_perf_test"
	// modelPerfRunWriteTimeout 是补写运行记录的上限。停机时父 ctx 已被取消，
	// 但连接池要等 stopModelPerf 返回后才关闭；给写库一个独立的有界 ctx，
	// 停机窗口里也能落一行终态。
	modelPerfRunWriteTimeout = 5 * time.Second
)

// newModelPerfRoundRecorder 返回写入 worker_task_runs 与 worker_tasks 的回调。
// 记录失败只告警：测速轮次本身已经完成，不能因为管理面写库失败让调度循环
// 或停机流程受影响。
//
// 字段口径与任务框架自己写出的行一致（worker.Scheduler.execute +
// bindWorkerTaskRuns 的 model_perf_test 绑定）：
//   - status：succeeded / failed（worker.RunSucceeded / worker.RunFailed）；
//   - trigger：schedule（定时轮次不是手动触发）；
//   - node_id：与调度器同源（httpapi.ResolveProcessInstanceID）。此前定时行
//     留空，运行历史里同一条任务的手动行有节点、定时行没有，管理员无法判断
//     这一轮是哪台 worker 跑的；补上与手动行一致后，两行才真正可比；
//   - error 列：只放固定分类（timeout / cancelled / execution_error），原始
//     err 文本可能带上游响应，绝不落库；
//   - result：models / tested / skipped_no_channel / persist_failed 四个计数，
//     与手动触发绑定完全相同，否则同一任务在历史里会出现两种形状；
//     失败时不写 result（与手动绑定的 `if err != nil { return nil, err }` 一致）。
func newModelPerfRoundRecorder(db *sql.DB, log *slog.Logger,
	nextDue func(time.Time) time.Time) func(context.Context, time.Time, modelperf.Stats, error) {
	if log == nil {
		log = slog.Default()
	}
	store, err := worker.NewSQLStore(db)
	if err != nil {
		// 装配错误只可能是 db 为 nil（编程错误）：记录器不可用，但不能因此
		// 阻止定时探测本身运行，所以只记错误并返回 nil（hook 变 no-op）。
		log.Error("worker: model perf run recorder disabled", "error", err)
		return nil
	}
	// 节点标识在构造时解析一次：任务框架装配（worker_tasks.go）也是启动时
	// 解析一次并交给调度器，同一进程写出的所有行应带同一个值。解析不到时为空
	// 串，CreateRun 的 NULLIF 会写成 NULL，与旧行为一致，不会产生空字符串行。
	nodeID := httpapi.ResolveProcessInstanceID()
	return func(ctx context.Context, startedAt time.Time, stats modelperf.Stats, roundErr error) {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelPerfRunWriteTimeout)
		defer cancel()

		finishedAt := time.Now().UTC()
		started := startedAt.UTC()
		status := worker.RunSucceeded
		errClass := ""
		var result worker.Result
		if roundErr != nil {
			status = worker.RunFailed
			errClass = modelPerfErrClass(ctx, roundErr)
		} else {
			result = worker.Result{
				"models":             stats.Models,
				"tested":             stats.Tested,
				"skipped_no_channel": stats.SkippedNoChannel,
				"persist_failed":     stats.PersistFailed,
			}
		}

		if _, err := store.CreateRun(writeCtx, worker.CreateRunInput{
			TaskType:   modelPerfTaskType,
			Status:     status,
			Trigger:    worker.TriggerSchedule,
			NodeID:     nodeID,
			StartedAt:  &started,
			FinishedAt: &finishedAt,
			Error:      errClass,
			Result:     result,
		}); err != nil {
			log.Warn("worker: record model perf round failed",
				"task_type", modelPerfTaskType, "status", status, "error", err)
		}

		// next_due_at 按真实时刻表回填（nextDue(finishedAt)），不能用注册表里
		// 展示用的 12h 周期推算——PERF_SCHEDULE_TIMES 可能是任意时刻。
		var next *time.Time
		if nextDue != nil {
			if due := nextDue(finishedAt); !due.IsZero() {
				next = &due
			}
		}
		if err := store.UpdateTaskAfterRun(writeCtx, modelPerfTaskType, started, status, next); err != nil {
			log.Warn("worker: update model perf task state failed",
				"task_type", modelPerfTaskType, "error", err)
		}
	}
}

// modelPerfErrClass 把一轮的 error 映射成运行历史允许的固定分类，规则与
// worker.executeTask 相同：超时/取消按 ctx 判定，其余一律 execution_error。
// 只写分类、不写原始 err 文本（可能带上游响应/凭据）。
func modelPerfErrClass(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return worker.ErrClassTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return worker.ErrClassCancelled
	default:
		return worker.ErrClassExecution
	}
}
