package platform

// 模型性能定时实测的 worker 装配（spec 2026-10-27 §5.4/§5.5）。
//
// 只在 worker 角色装配：整轮会对全部公开对话模型发起真实出站请求，不应与
// 中继流量争抢 gateway 进程。装配失败只降级为「任务不可用」，不影响计费与
// 轮询——测速不是交易路径。

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/modelperf"
	"newtg/gateway/internal/store"
)

// perfProbeTimeout 与手工测速同一上限：覆盖「建连 + 整段生成」（R-M4-14）。
const perfProbeTimeout = 30 * time.Second

// newModelPerfScheduler 装配模型性能定时实测（spec 2026-10-27 §5.4）。
//
// 依赖全部在启动期构造一次：仓储与探测器都无状态，执行时才触库/出站。时区
// 与时刻表在 config 校验阶段已解析过一遍，这里重新解析是为了让装配函数自洽
// （不依赖调用方的校验顺序），解析失败仍视为装配错误。
func newModelPerfScheduler(cfg *config.Config, db *sql.DB, rdb *redis.Client,
	log *slog.Logger) (*modelperf.Scheduler, error) {
	loc, err := time.LoadLocation(cfg.PerfScheduleTimezone)
	if err != nil {
		return nil, fmt.Errorf("model perf: load timezone %q: %w", cfg.PerfScheduleTimezone, err)
	}
	schedule, err := modelperf.ParseTimes(cfg.PerfScheduleTimes, loc)
	if err != nil {
		return nil, fmt.Errorf("model perf: parse schedule times: %w", err)
	}
	// 与 gateway 的手工测速共用同一出站客户端与凭据解封面，不新增第二套
	// 上游策略。
	runner, err := channelhealth.NewPerfRunner(channelhealth.ProberConfig{
		Opener:  httpapi.NewSecretSealer(cfg.CredentialEncryptionKey),
		Client:  httpapi.NewChannelProbeClient(),
		Timeout: perfProbeTimeout,
	})
	if err != nil {
		return nil, err
	}
	return modelperf.NewScheduler(modelperf.SchedulerConfig{
		Models:      store.NewPublicPricingRepo(db),
		Channels:    store.NewChannelRepo(db),
		Performer:   runner.Run,
		Tests:       store.NewSQLModelPerformanceStore(db),
		Schedule:    schedule,
		Runs:        cfg.PerfScheduleRuns,
		Concurrency: cfg.PerfScheduleConcurrency,
		Locker:      channelhealth.NewRedisLocker(rdb).WithLogger(log),
		Logger:      log,
	})
}

// startModelPerfLoop 启动定时循环，返回 cancel+Wait 的停止函数。
//
// enabled=false（PERF_SCHEDULE_ENABLED=false）时不启动任何 goroutine——
// 手动触发仍可经 RunOnce 使用。scheduler 为 nil（装配失败）时同样返回
// no-op：停机序列可以无条件调用返回值，不必再判空。
//
// onRound 是轮次结束后补写运行记录的回调（见 model_perf_runs.go）。它只在
// 循环启动前注入：step 之后并发读取该字段，启动后再改写会有数据竞争。nil
// （装配失败）表示不留运行记录，调度本身照常推进。
func startModelPerfLoop(parent context.Context, scheduler *modelperf.Scheduler,
	enabled bool, log *slog.Logger,
	onRound func(context.Context, time.Time, modelperf.Stats, error)) func() {
	if !enabled || scheduler == nil {
		return func() {}
	}
	if onRound != nil {
		scheduler.SetOnRound(onRound)
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := scheduler.RunLoop(ctx); err != nil && ctx.Err() == nil {
			log.Error("worker: model perf loop exited with error", "error", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
