package modelperf

// 定时性能探测的一轮执行引擎（Scheduler.RunOnce）。
//
// 职责边界：
//   - 本文件只负责「跑一轮」：取公开对话模型 → 每模型选一个渠道 → 探测 →
//     聚合 → 落库，并把计数交给调用方；
//   - 时刻表推进、时段锁与 tick 循环属于 Task 7 的 RunLoop/step，RunOnce
//     自身不加锁，因此可以被手工触发或测试直接调用。
//
// 所有外部依赖都收敛成接口/函数接缝（ModelSource、ChannelPicker、
// Performer、TestStore、Locker），单测无需数据库与上游。

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

const (
	// minRuns/maxRuns 是单模型探测次数的钳制范围：少于 1 次没有样本，
	// 多于 5 次只会放大上游配额消耗（与手工测速的 1..10 不同：定时任务
	// 全量跑，按模型数放大，故上界更保守）。
	minRuns = 1
	maxRuns = 5
	// minConcurrency/maxConcurrency 是单轮并发模型数的钳制范围。
	// 上界 8 用来避免一轮探测同时占满连接池/上游限流配额。
	minConcurrency = 1
	maxConcurrency = 8
	// defaultTickInterval 是调度循环的基准 tick（Task 7 使用）。
	defaultTickInterval = 30 * time.Second
	// defaultSlotTTL 是时段锁的兜底租约（Task 7 使用）。
	defaultSlotTTL = 24 * time.Hour
)

// ModelSource 提供本轮需要探测的模型名（生产 *store.SQLPublicPricingRepo）。
// 口径是「公开目录可见 + 具备对话/流式端点」，与首页性能表一致。
type ModelSource interface {
	ListPublicChatModels(ctx context.Context) ([]string, error)
}

// ChannelPicker 为单个模型选出路由优先级最高的启用渠道（生产 *store.ChannelRepo）。
// ok=false 表示当前没有可用渠道——该模型本轮跳过，不算失败。
type ChannelPicker interface {
	PickTop(ctx context.Context, model string) (*store.Channel, bool, error)
}

// Performer 执行单模型的 runs 次流式探测（生产 (*channelhealth.PerfRunner).Run）。
// 单次 run 失败只体现在结果的 Success/ErrorClass 上，不返回 error。
type Performer func(ctx context.Context, ch *store.Channel, model string, runs int) []channelhealth.PerfRunResult

// TestStore 原子落库一次测速的汇总行与明细行（生产 *store.SQLModelPerformanceStore）。
type TestStore interface {
	Create(ctx context.Context, test store.ModelPerformanceTest, runs []store.ModelPerformanceRun) (store.ModelPerformanceTest, error)
}

// Locker 是时段租约锁的接缝（生产 *channelhealth.RedisLocker）。
//
// RunOnce 不直接使用它；它在启动期被校验为必填，因为 Task 7 的多副本
// 去重完全依赖这把锁——缺失必须在启动时就暴露，而不是等到同时探测。
type Locker interface {
	TryLock(ctx context.Context, key string, ttl time.Duration) (release func(), acquired bool, err error)
}

// 编译期断言：生产实现与上述接缝同签名，避免装配层的隐式漂移。
var (
	_ ModelSource   = (*store.SQLPublicPricingRepo)(nil)
	_ ChannelPicker = (*store.ChannelRepo)(nil)
	_ TestStore     = (*store.SQLModelPerformanceStore)(nil)
	_ Performer     = (*channelhealth.PerfRunner)(nil).Run
	_ Locker        = (*channelhealth.RedisLocker)(nil)
)

// Stats 是一轮探测的计数（可观测性与测试断言共用）。
//
// 口径：Models 是模型总数；Tested 是成功落库的模型数；SkippedNoChannel
// 是无可用渠道而跳过的模型数；PersistFailed 是探测完成但落库失败的模型数。
type Stats struct {
	Models           int
	Tested           int
	SkippedNoChannel int
	PersistFailed    int
	Duration         time.Duration
}

// SchedulerConfig 是调度器的显式依赖与参数。
type SchedulerConfig struct {
	// Models/Channels/Performer/Tests/Locker 必填：缺任何一项都无法完成
	// 「取模型 → 选渠道 → 探测 → 落库」，且时段锁是跨副本去重的唯一手段。
	Models    ModelSource
	Channels  ChannelPicker
	Performer Performer
	Tests     TestStore
	Locker    Locker
	// Schedule 必填（至少一个时刻）：零值会让循环永远不到点。
	Schedule Schedule
	// Runs/Concurrency 超界时钳制（见 minRuns..maxRuns / minConcurrency..maxConcurrency）。
	Runs        int
	Concurrency int
	// Logger 为 nil 时使用 slog.Default。
	Logger *slog.Logger
	// Now 是时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// TickInterval/SlotTTL 供 Task 7 的调度循环使用；<=0 取默认值。
	TickInterval time.Duration
	SlotTTL      time.Duration
	// OnRound 在每个定时轮次结束后调用（仅在 RunLoop 的 step 路径触发）。
	// 手动触发走 RunOnce，由任务框架自行写运行记录，不能再记一次。
	// nil 表示不关心轮次结果（测试与不需要运行历史的装配）。
	OnRound func(ctx context.Context, startedAt time.Time, stats Stats, err error)
}

// Scheduler 是定时性能探测的调度器。
//
// 字段名固定：Task 7 的 step 直接读写 schedule/logger/locker/slotTTL/
// tickInterval/nextDue。构造后除 nextDue 与启动前注入的 onRound 外全部只读。
type Scheduler struct {
	models       ModelSource
	channels     ChannelPicker
	performer    Performer
	tests        TestStore
	schedule     Schedule
	runs         int
	concurrency  int
	locker       Locker
	logger       *slog.Logger
	now          func() time.Time
	tickInterval time.Duration
	slotTTL      time.Duration
	// onRound 由装配层在 RunLoop 启动前经 SetOnRound 注入，之后只读；
	// 只在 step 的执行分支里调用，锁错误/被占用/未到点都不写运行记录。
	onRound func(ctx context.Context, startedAt time.Time, stats Stats, err error)
	// nextDue 只由 RunLoop 所在 goroutine 的 step 读写；RunOnce（含手动触发）
	// 不得触碰，否则会与调度循环产生数据竞争并打乱时段推进。
	nextDue time.Time
}

// NewScheduler 校验依赖、钳制参数并计算第一个触发时刻。
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Models == nil {
		return nil, errors.New("modelperf: scheduler requires a model source")
	}
	if cfg.Channels == nil {
		return nil, errors.New("modelperf: scheduler requires a channel picker")
	}
	if cfg.Performer == nil {
		return nil, errors.New("modelperf: scheduler requires a performer")
	}
	if cfg.Tests == nil {
		return nil, errors.New("modelperf: scheduler requires a test store")
	}
	if cfg.Locker == nil {
		return nil, errors.New("modelperf: scheduler requires a slot locker")
	}
	if len(cfg.Schedule.dayTimes) == 0 {
		return nil, errors.New("modelperf: scheduler requires a schedule with at least one time")
	}

	s := &Scheduler{
		models:       cfg.Models,
		channels:     cfg.Channels,
		performer:    cfg.Performer,
		tests:        cfg.Tests,
		schedule:     cfg.Schedule,
		runs:         clamp(cfg.Runs, minRuns, maxRuns),
		concurrency:  clamp(cfg.Concurrency, minConcurrency, maxConcurrency),
		locker:       cfg.Locker,
		logger:       cfg.Logger,
		now:          cfg.Now,
		tickInterval: cfg.TickInterval,
		slotTTL:      cfg.SlotTTL,
		onRound:      cfg.OnRound,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.tickInterval <= 0 {
		s.tickInterval = defaultTickInterval
	}
	if s.slotTTL <= 0 {
		s.slotTTL = defaultSlotTTL
	}
	s.nextDue = s.schedule.Next(s.now())
	return s, nil
}

// clamp 把 v 收敛到 [lo, hi]。
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SetOnRound 在构造后注入轮次回调。
//
// 为什么不在 NewScheduler 里一次配好：生产装配必须先拿到 *Scheduler 才能用
// NextDue 构造记录器（真实的下次时刻由时刻表决定），存在构造顺序上的环。
// 只允许在 RunLoop 启动前调用一次；启动后 step 会并发读取该字段。
func (s *Scheduler) SetOnRound(fn func(ctx context.Context, startedAt time.Time, stats Stats, err error)) {
	s.onRound = fn
}

// NextDue 返回 now 之后的第一个时段（按时刻表时区解释），供运行记录回填
// next_due_at：管理面显示的周期是固定 12h，而真实的下一次可能是
// PERF_SCHEDULE_TIMES 配出来的任意时刻。
func (s *Scheduler) NextDue(now time.Time) time.Time {
	return s.schedule.Next(now)
}

// RunOnce 执行一轮全量探测：所有公开对话模型各测一次（每模型 runs 次请求）。
//
// 单模型的失败（选渠道报错、落库报错）只记日志并计入 Stats，不中止其他
// 模型；只有取模型列表失败才返回 error。RunOnce 不取时段锁——锁属于
// 调度循环，手工/测试触发的单轮执行不应被锁竞争挡住。
func (s *Scheduler) RunOnce(ctx context.Context) (Stats, error) {
	start := s.now()
	var stats Stats

	names, err := s.models.ListPublicChatModels(ctx)
	if err != nil {
		return Stats{}, err
	}
	stats.Models = len(names)
	if len(names) == 0 {
		stats.Duration = s.now().Sub(start)
		return stats, nil
	}

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	// 信号量在**派发侧**获取：循环本身因此被限流，最多只有 concurrency
	// 个模型同时在探测，而不只是限制 goroutine 数量。
	sem := make(chan struct{}, s.concurrency)

dispatch:
	for _, model := range names {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func(model string) {
			defer wg.Done()
			defer func() { <-sem }()
			// 探测实现（未来可能换成子进程/插件）里的 panic 不能把整个 worker
			// 进程带走：只记日志并按「未测试」处理，不引入新的 Stats 计数
			// （worker.Result 的键集合由 Task 10 固定）。
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("modelperf: probe panicked", "model", model, "panic", r)
				}
			}()

			ch, ok, pickErr := s.channels.PickTop(ctx, model)
			if pickErr != nil {
				s.logger.Warn("modelperf: pick channel failed", "model", model, "error", pickErr)
				return
			}
			if !ok {
				mu.Lock()
				stats.SkippedNoChannel++
				mu.Unlock()
				return
			}

			results := s.performer(ctx, ch, model, s.runs)
			test, runRows := Aggregate(ch, model, s.runs, results, nil)
			if _, createErr := s.tests.Create(ctx, test, runRows); createErr != nil {
				s.logger.Warn("modelperf: persist performance test failed",
					"model", model, "channel_id", ch.ID, "error", createErr)
				mu.Lock()
				stats.PersistFailed++
				mu.Unlock()
				return
			}
			mu.Lock()
			stats.Tested++
			mu.Unlock()
		}(model)
	}
	// ctx 取消后仍要等已派发的模型收尾，否则会与调用方的下一轮重叠。
	wg.Wait()

	stats.Duration = s.now().Sub(start)
	return stats, nil
}

// slotKey 返回一个时段在 Redis 中的锁键，形如
// "model_perf_schedule:2026-10-27T12:00"。
//
// 时段一律按 schedule.Location() 格式化再入键：同一瞬间在不同时区的副本
// 必须落到同一个字符串上，所以既不能用 slot 自身的时区，也不能固定用 UTC
// ——只有时刻表的时区是跨副本一致的语义锚点。
func (s *Scheduler) slotKey(slot time.Time) string {
	return "model_perf_schedule:" + slot.In(s.schedule.Location()).Format("2006-01-02T15:04")
}

// step 处理一个 tick：到点才取锁，取到锁才跑一轮。
//
// step 至多阻塞一次 RunOnce（锁与探测都受 ctx 控制），因此 ticker 的后续
// tick 最多延后一轮，不会堆积执行。测试直接调用它以绕开真实时钟。
func (s *Scheduler) step(ctx context.Context) {
	now := s.now()
	if now.Before(s.nextDue) {
		return
	}
	slot := s.nextDue
	key := s.slotKey(slot)

	// release 故意丢弃：时段锁靠 TTL 自然过期。本轮执行期间，TTL 内的重复
	// tick 及其它副本必须对同一时段继续 no-op；提前释放会让它们重新拿到锁
	// 并重复探测，锁就形同虚设。
	_, acquired, err := s.locker.TryLock(ctx, key, s.slotTTL)
	switch {
	case err != nil:
		s.logger.Warn("modelperf: acquire slot lock failed; will retry",
			"slot", slot.Format(time.RFC3339), "error", err)
		// 仍在当前时段内：保持 nextDue，下个 tick 重试。若已越过下一个时段
		// （错过整整一个周期，例如 Redis 长时间不可用），直接跳到未来时段，
		// 避免恢复后连续补跑多个时段。
		if next := s.schedule.Next(slot); !now.Before(next) {
			s.nextDue = s.schedule.Next(now)
		}
	case !acquired:
		s.logger.Info("modelperf: scheduled round skipped",
			"slot", slot.Format(time.RFC3339), "skipped_locked", true)
		s.nextDue = s.schedule.Next(now)
	default:
		// startedAt 取 RunOnce 之前：运行记录的开始时间是轮次真实起点，而不是
		// 钩子触发时刻；平台层还用它回填 worker_tasks.last_run_at。
		startedAt := s.now()
		stats, runErr := s.RunOnce(ctx)
		if runErr != nil {
			s.logger.Warn("modelperf: scheduled round failed",
				"slot", slot.Format(time.RFC3339), "error", runErr)
		} else {
			s.logger.Info("modelperf: scheduled round complete",
				"slot", slot.Format(time.RFC3339), "models", stats.Models,
				"tested", stats.Tested, "skipped_no_channel", stats.SkippedNoChannel,
				"persist_failed", stats.PersistFailed, "duration", stats.Duration)
		}
		// 只有定时轮次写运行记录：手动触发直接调 RunOnce，由任务框架自己写
		// 一行，这里再记一次会让同一次执行在历史里出现两行。
		s.notifyRound(ctx, startedAt, stats, runErr)
		// 以轮次结束时刻（而非触发时刻）计算下一个时段：一轮跑得久时，
		// 已过去的时段直接跳过，不会排队补跑。
		s.nextDue = s.schedule.Next(s.now())
	}
}

// notifyRound 调用 OnRound 钩子。钩子是外部代码（平台层的写库回调），它的
// panic 不能把调度循环带走——与 RunOnce 里对 probe 的处理同一原则：只记日志，
// 循环继续按时刻表推进。recover 放在本函数内，step 的时段推进逻辑因此不会被
// 钩子实现的 panic 打断。
func (s *Scheduler) notifyRound(ctx context.Context, startedAt time.Time, stats Stats, err error) {
	if s.onRound == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("modelperf: round hook panicked", "panic", r)
		}
	}()
	s.onRound(ctx, startedAt, stats, err)
}

// RunLoop 按 tickInterval 轮询 step，直到 ctx 取消；取消是正常退出，返回 nil。
func (s *Scheduler) RunLoop(ctx context.Context) error {
	s.logger.Info("modelperf: scheduler started", "next_due", s.nextDue.Format(time.RFC3339))
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.step(ctx)
		}
	}
}
