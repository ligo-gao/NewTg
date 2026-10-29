package platform

// 模型性能定时实测的 worker 装配（spec 2026-10-27 §5.4/§5.5）。
//
// 只在 worker 角色装配：整轮会对全部公开对话模型发起真实出站请求，不应与
// 中继流量争抢 gateway 进程。装配失败只降级为「任务不可用」，不影响计费与
// 轮询——测速不是交易路径。

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/modelperf"
	"newtg/gateway/internal/perfsettings"
	"newtg/gateway/internal/store"
)

// perfProbeTimeout 与手工测速同一上限：覆盖「建连 + 整段生成」（R-M4-14）。
const perfProbeTimeout = 30 * time.Second

// newPerfSettingsReader 把 config 里的 PERF_SCHEDULE_* 解释成 perfsettings 的
// Defaults。全仓只此一处映射：worker 的调度器与 gateway 的 config 端点必须
// 对「页面未保存时按什么跑」给出同一个答案，第二份映射迟早漂移。
func newPerfSettingsReader(cfg *config.Config, center *configcenter.Center, log *slog.Logger) *perfsettings.Reader {
	return perfsettings.New(center, perfsettings.Defaults{
		Enabled:     cfg.PerfScheduleEnabled,
		Times:       cfg.PerfScheduleTimes,
		Timezone:    cfg.PerfScheduleTimezone,
		Runs:        cfg.PerfScheduleRuns,
		Concurrency: cfg.PerfScheduleConcurrency,
	}, log)
}

// newModelPerfScheduler 装配模型性能定时实测（spec 2026-10-27 §5.4）。
//
// 依赖全部在启动期构造一次：仓储与探测器都无状态，执行时才触库/出站。
// 时区与时刻表**不在这里解析**：perfsettings.Reader 每次读取时按「页面设置
// 优先、env 默认兜底」解释，解析失败回落 UTC / 00:00,12:00 并告警，而不是
// 让装配失败——节奏配置脏了最多回到默认节奏，不能让 worker 起不来。
// 启动期仍由 config.Validate 校验 env 取值，拼写错误在启动时就暴露。
//
// 返回的 reader 与调度器共用同一个 *perfsettings.Reader：控制面的 config
// 端点复用它，页面展示的「生效值/来源」与 worker 的真实节奏因此出自同一份
// 读取逻辑，不会各读各的。PERF_SCHEDULE_* 只作为 Defaults（未落库时才生效）。
func newModelPerfScheduler(cfg *config.Config, db *sql.DB, rdb *redis.Client,
	center *configcenter.Center, log *slog.Logger) (*modelperf.Scheduler, *perfsettings.Reader, error) {
	perfReader := newPerfSettingsReader(cfg, center, log)
	// 与 gateway 的手工测速共用同一出站客户端与凭据解封面，不新增第二套
	// 上游策略。
	runner, err := channelhealth.NewPerfRunner(channelhealth.ProberConfig{
		Opener:  httpapi.NewSecretSealer(cfg.CredentialEncryptionKey),
		Client:  httpapi.NewChannelProbeClient(channelEgressGuard(center)),
		Timeout: perfProbeTimeout,
	})
	if err != nil {
		return nil, nil, err
	}
	scheduler, err := modelperf.NewScheduler(modelperf.SchedulerConfig{
		Models:    store.NewPublicPricingRepo(db),
		Channels:  store.NewChannelRepo(db),
		Performer: runner.Run,
		Tests:     store.NewSQLModelPerformanceStore(db),
		// 两个闭包是「设置 → 调度器」的唯一接缝：NewScheduler 只做首次计算，
		// step 每个 tick 重新调用一次，运维保存后无需重启即可生效。
		ScheduleFor: func(now time.Time) modelperf.Schedule {
			schedule, _ := perfReader.Schedule(now)
			return schedule
		},
		Settings: func() modelperf.RoundSettings {
			settings := perfReader.Settings(time.Now())
			return modelperf.RoundSettings{
				Enabled:     settings.Enabled,
				Runs:        settings.Runs,
				Concurrency: settings.Concurrency,
			}
		},
		Locker: channelhealth.NewRedisLocker(rdb).WithLogger(log),
		Logger: log,
	})
	if err != nil {
		return nil, nil, err
	}
	return scheduler, perfReader, nil
}

// startModelPerfLoop 启动定时循环，返回 cancel+Wait 的停止函数。
//
// 循环**总是**启动：启停开关在控制台可以随时保存，若沿用启动期的
// PERF_SCHEDULE_ENABLED 短路 goroutine，打开开关就必须重启 worker；关闭
// （Settings().Enabled=false）由每个 tick 判定并跳过轮次。scheduler 为 nil
// （装配失败）时返回 no-op：停机序列可以无条件调用返回值，不必再判空。
// 定时停用期间手动触发仍可经 RunOnce 使用——那条路径不经过本循环。
//
// onRound 是轮次结束后补写运行记录的回调（见 model_perf_runs.go）。它只在
// 循环启动前注入：step 之后并发读取该字段，启动后再改写会有数据竞争。nil
// （装配失败）表示不留运行记录，调度本身照常推进。
func startModelPerfLoop(parent context.Context, scheduler *modelperf.Scheduler,
	log *slog.Logger,
	onRound func(context.Context, time.Time, modelperf.Stats, error)) func() {
	if scheduler == nil {
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
