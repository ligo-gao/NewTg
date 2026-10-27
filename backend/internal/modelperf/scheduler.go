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
}

// Scheduler 是定时性能探测的调度器。
//
// 字段名固定：Task 7 的 step 直接读写 schedule/logger/locker/slotTTL/
// tickInterval/nextDue。构造后除 nextDue 外全部只读。
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
	nextDue      time.Time
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
