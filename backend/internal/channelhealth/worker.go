package channelhealth

// W7/M4-b：渠道健康检查 worker（modules-06 F-06-12；设计 relay-01 §10）。
//
// 职责：按周期扫描 enabled + auto_disabled 渠道 → 共享探测（probe.go）→
// 写 last_test_* → 失败按 R-M4-01 判定是否自动禁用 → 成功且开关开启时自动
// 恢复（R-M4-09/15）。默认常开（R-M4-07）：没有任何「启用体检」开关，因此
// 部署后**默认产生后台出站流量**（每 10 分钟探测全部可探测渠道）。
//
// 调度（R-M4-13）：1 分钟基准 tick + nextDue（每轮重读 interval）+ 启动后
// 等待一个 interval + 0–60s jitter。为什么要等一个完整间隔：多副本/滚动
// 重启时若启动即探测，会在同一时刻对全部渠道发起一轮风暴；jitter 进一步
// 打散重启副本。
//
// 互斥：每轮用 Redis `SET NX PX` 抢占；拿不到锁或 Redis 不可用都跳过本轮
// （fail-closed：宁可少测一轮，也不并发探测/重复写状态）。

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/store"
)

const (
	// defaultTickInterval 是调度基准 tick（1 分钟）。nextDue 存在分钟级精度：
	// 改 interval 配置最迟一个 tick 生效，无需重启（R-M4-13）。
	defaultTickInterval = time.Minute
	// defaultJitterMax 是启动延迟的随机上限（0–60s，R-M4-13）。
	defaultJitterMax = time.Minute
	// workerStoreTimeout 是单条状态写/结果写的上限。与 reporter 同口径：
	// 后台写不得无限期占用连接池。
	workerStoreTimeout = 5 * time.Second
	// probeSlotOverhead 是每批探测的固定余量（建立/关闭连接、写 last_test_*）。
	// 用于计算锁 TTL 的最坏单轮预算。
	probeSlotOverhead = 5 * time.Second
)

// WorkerStore 是健康 worker 的存储面（生产 *store.ChannelRepo）。
type WorkerStore interface {
	// ListForHealthCheck 返回 status IN (1,3) 的全量渠道（R-M4-09/12）。
	ListForHealthCheck(ctx context.Context) ([]store.Channel, error)
	// AutoDisable 是 `WHERE status=1 AND auto_ban=true` 的条件迁移。
	AutoDisable(ctx context.Context, id int64, reason string) (*store.Channel, bool, error)
	// AutoEnable 是 `WHERE status=3` 的条件迁移（人工禁用永不恢复）。
	AutoEnable(ctx context.Context, id int64) (*store.Channel, bool, error)
	// UpdatePeriodicTestResult 只写 last_test_*，不推进 updated_at（R-M4-08）。
	UpdatePeriodicTestResult(ctx context.Context, id int64, at time.Time, success bool) error
}

// ProbeRunner 是探测接缝（生产 *Prober；测试注入确定性结果）。
type ProbeRunner interface {
	Probe(ctx context.Context, ch *store.Channel, model string) ProbeResult
}

// WorkerConfig 是健康 worker 的显式依赖。
type WorkerConfig struct {
	// Store/Prober/Locker 必填：缺任何一项都无法安全运行（无锁会并发探测，
	// 无探测则整轮空转，无状态写则判定无法落地）。
	Store  WorkerStore
	Config SettingsSource
	Prober ProbeRunner
	Locker RoundLocker
	// Audit/Notifier 可选（测试/降级）；生产必须注入以留下 system 审计。
	Audit    AuditWriter
	Notifier AutoDisableNotifier
	Logger   *slog.Logger
	// Now 是轮次时间戳的时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// ChannelsURL 是禁用通知里的跳转链接。
	ChannelsURL string

	// TickInterval 覆盖 1 分钟基准 tick（仅测试；<=0 使用默认值）。
	TickInterval time.Duration
	// IntervalOverride 覆盖 rl.health.test_interval_minutes（仅测试）。
	// 生产必须为 0：间隔必须每轮从配置中心重读才能即时生效（R-M4-13）。
	IntervalOverride time.Duration
	// Jitter 覆盖启动抖动（仅测试；nil 时随机 0–60s）。
	Jitter func() time.Duration
}

// Worker 是周期健康检查循环。
type Worker struct {
	store       WorkerStore
	config      SettingsSource
	prober      ProbeRunner
	locker      RoundLocker
	audit       AuditWriter
	notifier    AutoDisableNotifier
	log         *slog.Logger
	now         func() time.Time
	channelsURL string

	tickInterval     time.Duration
	intervalOverride time.Duration
	jitterFn         func() time.Duration
}

// WorkerStats 是一轮体检的计数（可观测性/测试断言）。
//
// 计数口径：Tested = OK + Failed；SkippedUnsupported/NoModel/Invalid 是
// 「无法探测、未写 last_test_*」的类别；SkippedLocked 表示本轮被其它副本持有。
type WorkerStats struct {
	Tested             int
	OK                 int
	Failed             int
	Disabled           int
	Enabled            int
	SkippedUnsupported int
	SkippedNoModel     int
	SkippedInvalid     int
	SkippedLocked      bool
	LockError          bool
}

// NewWorker 构造健康 worker。
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Store == nil {
		return nil, errors.New("channelhealth: health worker requires a store")
	}
	if cfg.Prober == nil {
		return nil, errors.New("channelhealth: health worker requires a prober")
	}
	if cfg.Locker == nil {
		return nil, errors.New("channelhealth: health worker requires a round locker")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = defaultTickInterval
	}
	return &Worker{
		store:            cfg.Store,
		config:           cfg.Config,
		prober:           cfg.Prober,
		locker:           cfg.Locker,
		audit:            cfg.Audit,
		notifier:         cfg.Notifier,
		log:              log,
		now:              now,
		channelsURL:      cfg.ChannelsURL,
		tickInterval:     tick,
		intervalOverride: cfg.IntervalOverride,
		jitterFn:         cfg.Jitter,
	}, nil
}

// RunLoop 阻塞运行健康检查循环，直到 ctx 取消。
//
// 停机语义：ctx 取消后不再发起新探测；进行中的探测随 ctx 取消；调用方
// （platform.runWorker）在关闭 Redis/DB 之前 cancel + Wait。
func (w *Worker) RunLoop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	interval := w.currentInterval()
	firstDelay := interval + w.jitter()
	w.log.Info("channelhealth: health worker started",
		"interval", interval, "first_run_delay", firstDelay, "concurrency", LoadWorkerSettings(w.config).Concurrency)

	ticker := time.NewTicker(w.tickInterval)
	defer ticker.Stop()
	initial := time.NewTimer(firstDelay)
	defer initial.Stop()

	nextDue := w.now().Add(firstDelay)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-initial.C:
			w.runRound(ctx)
			nextDue = w.now().Add(w.currentInterval())
		case <-ticker.C:
			// 基准 tick 只负责「到点检查」：interval 每轮重读，因此管理员
			// 改配置后最迟一个 tick 生效（R-M4-13 的 B 方案）。
			if w.now().Before(nextDue) {
				continue
			}
			w.runRound(ctx)
			nextDue = w.now().Add(w.currentInterval())
		}
	}
}

// currentInterval 每轮重新读取设置；测试用 override 保持确定性。
func (w *Worker) currentInterval() time.Duration {
	if w.intervalOverride > 0 {
		return w.intervalOverride
	}
	return LoadWorkerSettings(w.config).Interval
}

// jitter 返回启动延迟的随机抖动（0–60s）。
func (w *Worker) jitter() time.Duration {
	if w.jitterFn != nil {
		return w.jitterFn()
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(buf[:]) % uint64(defaultJitterMax))
}

// runRound 执行一轮并记录结果。任何失败只记录，下一轮重试（体检不是交易
// 路径，worker 不得因为一次 DB/Redis 抖动退出）。
func (w *Worker) runRound(ctx context.Context) {
	stats, err := w.RunOnce(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("channelhealth: health check round failed", "error", err)
		}
		return
	}
	switch {
	case stats.LockError:
		w.log.Warn("channelhealth: health check lock unavailable; round skipped")
	case stats.SkippedLocked:
		w.log.Debug("channelhealth: health check round skipped (lock held by another replica)")
	default:
		w.log.Info("channelhealth: health check round finished",
			"tested", stats.Tested, "ok", stats.OK, "failed", stats.Failed,
			"disabled", stats.Disabled, "enabled", stats.Enabled,
			"skipped_unsupported", stats.SkippedUnsupported,
			"skipped_no_model", stats.SkippedNoModel,
			"skipped_invalid", stats.SkippedInvalid,
		)
	}
}

// RunOnce 执行一轮体检（生产由 RunLoop 调用；测试直接调用以获得确定性）。
//
// 顺序：读设置 → 扫描集 → 抢锁 → 有界并发探测 → 逐渠道写结果/判定/恢复。
// 扫描在抢锁之前：扫描是只读的，先扫描才能按渠道数计算锁 TTL；两个副本
// 同时扫描的代价可忽略，真正互斥的是探测与写入。
func (w *Worker) RunOnce(ctx context.Context) (WorkerStats, error) {
	if w == nil {
		return WorkerStats{}, nil
	}
	settings := LoadWorkerSettings(w.config)
	channels, err := w.store.ListForHealthCheck(ctx)
	if err != nil {
		return WorkerStats{}, err
	}
	if len(channels) == 0 {
		return WorkerStats{}, nil
	}

	release, acquired, err := w.locker.TryLock(ctx, ChannelHealthCheckLockKey, roundTTL(settings, len(channels)))
	if err != nil {
		// 锁不可用 → 本轮跳过（fail-closed：绝不并发探测）。错误只告警，
		// 不退出循环，下一轮重试。
		w.log.Warn("channelhealth: acquire health check lock failed", "error", err)
		return WorkerStats{LockError: true}, nil
	}
	if !acquired {
		return WorkerStats{SkippedLocked: true}, nil
	}
	defer release()

	return w.probeAll(ctx, channels, settings), nil
}

// probeAll 用有界并发探测扫描集，返回汇总计数。
//
// 并发上限来自设置（默认 5）；每个 goroutine 只处理自己的渠道，计数用
// atomic 汇总，避免在探测路径上引入共享锁。
func (w *Worker) probeAll(ctx context.Context, channels []store.Channel, settings WorkerSettings) WorkerStats {
	concurrency := settings.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var counters workerCounters
	// 同一轮的所有 last_test_at 使用同一时间戳：便于日志对账，也让测试
	// 能断言「本轮写入」。状态迁移的 updated_at 仍由数据库 now() 产生。
	now := w.now()

	for i := range channels {
		if ctx.Err() != nil {
			// 停机：不再发起新探测（进行中的探测随 ctx 取消）。
			break
		}
		channel := channels[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			w.probeChannel(ctx, &channel, settings, now, &counters)
		}()
	}
	wg.Wait()
	return counters.snapshot()
}

// probeChannel 处理单个渠道：探测 → 写 last_test_* → 禁用/恢复判定。
func (w *Worker) probeChannel(ctx context.Context, ch *store.Channel,
	settings WorkerSettings, now time.Time, counters *workerCounters) {
	model := probeModel(ch)
	if model == "" {
		// 没有可测模型（supported_models 为空且未配 test_model）：与无 adapter
		// 同类，整轮跳过、不改 last_test_*（避免把「无法探测」写成失败）。
		counters.skippedNoModel.Add(1)
		return
	}

	result := w.prober.Probe(ctx, ch, model)
	switch {
	case result.Unsupported:
		// R-M4-06：无 adapter 的渠道类型整轮跳过，不改 last_test_*、不判失败。
		counters.skippedUnsupported.Add(1)
		return
	case result.CredentialInvalid, result.BuildFailed:
		// 本地配置问题（GC-8）：不是上游健康信号，跳过。
		counters.skippedInvalid.Add(1)
		return
	}

	if ctx.Err() != nil {
		// 停机竞态：探测已随 ctx 取消，不写结果、不改状态。
		return
	}

	success := ProbeSucceeded(result, settings)
	if err := w.writePeriodicResult(ctx, ch.ID, now, success); err != nil {
		// 写入失败仍继续判定：状态迁移与 last_test_* 是独立事实，不能因为
		// 一次结果回写失败就漏掉禁用/恢复。
		w.log.Warn("channelhealth: write periodic test result failed",
			"channel_id", ch.ID, "error", err)
	}
	counters.tested.Add(1)
	if success {
		counters.ok.Add(1)
	} else {
		counters.failed.Add(1)
	}

	if decision, ok := DecideProbeFailure(result, ch.AutoBan, settings); ok {
		if _, changed, err := applyAutoDisable(ctx, w.store, w.audit, w.notifier,
			w.log, w.channelsURL, ch.ID, decision); err != nil {
			// applyAutoDisable 已记录 Warn；本轮继续处理其它渠道。
		} else if changed {
			counters.disabled.Add(1)
		}
	}

	// 自动恢复（R-M4-09/15）：
	//   - 只有**周期体检**的成功探测能恢复；手工测试走 Reporter，不调用本函数；
	//   - 开关默认 false；
	//   - 扫描集含 status=3，且 AutoEnable 的 WHERE status=3 是第二道守卫
	//     （即便 manual_disabled 被误送进来也不会被启用）。
	if success && settings.AutoEnableEnabled && ch.Status == 3 {
		changed, err := applyAutoEnable(ctx, w.store, w.audit, w.log, ch.ID)
		if err != nil {
			// 已 Warn；下一轮重试。
		} else if changed {
			counters.enabled.Add(1)
		}
	}
}

// writePeriodicResult 用独立且有界的 context 写 last_test_*。
//
// 为什么要 WithoutCancel：外层 ctx 可能在探测后立刻取消（停机），但这条
// 结果属于已经完成的事实；用取消的 ctx 写会得到 driver 层错误并把一次正常
// 探测记录成失败日志。写入本身有 5s 上限，不会拖长停机。
func (w *Worker) writePeriodicResult(ctx context.Context, id int64, at time.Time, success bool) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerStoreTimeout)
	defer cancel()
	return w.store.UpdatePeriodicTestResult(writeCtx, id, at, success)
}

// roundTTL 计算单轮锁的 TTL（R-M4-13：TTL ≥ 最坏单轮时长）。
//
// 最坏单轮 = ceil(渠道数/并发) 批 × (单次探测 15s + 固定余量) + 1 分钟。
// 再与 interval 取较大值：interval 短于单轮时，TTL 至少覆盖一个周期，避免
// 锁在上一轮尚未结束时过期。渠道数超过并发×预算的极端规模下 TTL 会不足，
// 此时第二个副本可能重复探测——probe 是只读的，状态迁移仍是条件更新，
// 损失仅为重复出站请求（可接受；渠道规模在 M4 是数十～数百）。
func roundTTL(settings WorkerSettings, channelCount int) time.Duration {
	concurrency := settings.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	slots := (channelCount + concurrency - 1) / concurrency
	budget := time.Duration(slots)*(ProbeTimeout+probeSlotOverhead) + time.Minute
	if settings.Interval > budget {
		return settings.Interval
	}
	return budget
}

// probeModel 选择探测模型：test_model 优先，否则 supported_models 首项
// （modules-06 F-06-12；手工测试同一选择顺序）。
func probeModel(ch *store.Channel) string {
	if ch == nil {
		return ""
	}
	if ch.TestModel != nil {
		if model := strings.TrimSpace(*ch.TestModel); model != "" {
			return model
		}
	}
	var models []string
	if err := json.Unmarshal(ch.SupportedModels, &models); err != nil {
		return ""
	}
	for _, model := range models {
		if trimmed := strings.TrimSpace(model); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// workerCounters 是并发探测的原子计数（单 goroutine 消费，无需锁）。
type workerCounters struct {
	tested             atomic.Int64
	ok                 atomic.Int64
	failed             atomic.Int64
	disabled           atomic.Int64
	enabled            atomic.Int64
	skippedUnsupported atomic.Int64
	skippedNoModel     atomic.Int64
	skippedInvalid     atomic.Int64
}

func (c *workerCounters) snapshot() WorkerStats {
	return WorkerStats{
		Tested:             int(c.tested.Load()),
		OK:                 int(c.ok.Load()),
		Failed:             int(c.failed.Load()),
		Disabled:           int(c.disabled.Load()),
		Enabled:            int(c.enabled.Load()),
		SkippedUnsupported: int(c.skippedUnsupported.Load()),
		SkippedNoModel:     int(c.skippedNoModel.Load()),
		SkippedInvalid:     int(c.skippedInvalid.Load()),
	}
}
