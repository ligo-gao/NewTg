package perfmetrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// defaultFlushInterval 对应 platform-05 §3.5.5 的默认值 5m。
	defaultFlushInterval = 5 * time.Minute
	// defaultLateWindow 是迟到数据的重算窗口：超过后旧分钟桶不再接收新样本。
	// 与设计 §3.5.2 的「迟到数据允许 1 小时内重算」一致。
	defaultLateWindow = time.Hour
	// maxBuckets 是进程内分钟桶的硬上限，防止模型基数异常或数据库长期不可用
	// 时把内存吃光。超限后的样本被丢弃并计入 dropped（监控降级，不影响中继）。
	maxBuckets = 100_000
	// flushTimeout 是单次 flush 的上限；中继请求不等待 flush。
	flushTimeout = 30 * time.Second
	// shutdownFlushTimeout 是停机最终 flush 的上限。必须小于 HTTP 优雅停机
	// 窗口，避免关库被 flush 拖死。
	shutdownFlushTimeout = 5 * time.Second
	// defaultRetentionInterval 是分钟→小时物化与过期清理的周期。
	defaultRetentionInterval = time.Hour
	// defaultHourRetention 对应 PERF_METRICS_RETENTION_DAYS 默认 400 天。
	defaultHourRetention = 400 * 24 * time.Hour
	// retentionTimeout 是单次保留清理的上限。
	retentionTimeout = 2 * time.Minute
	// maxModelLen 与 usage_logs.model 的 varchar(128) 对齐。
	maxModelLen = 128
)

// ErrDisabled 表示采集器未装配/已停止。注意：生产装配在
// PERF_METRICS_ENABLED=false 时根本不构造 Collector，因此这条错误主要供
// 测试与防御性调用使用。
var ErrDisabled = errors.New("perfmetrics: collector is disabled")

// Config 是 Collector 的装配输入。
type Config struct {
	// Store 是持久化接缝，必填。
	Store Store
	// FlushInterval 是批量刷写周期（PERF_METRICS_FLUSH_INTERVAL，1–60 分钟
	// 已由 config 校验）。
	FlushInterval time.Duration
	// Node 是本副本标识；多副本时与 Epoch 一起构成快照键的一部分。
	Node string
	// LateWindow 是迟到重算窗口；零值取 1 小时。
	LateWindow time.Duration
	// RetentionInterval 是保留清理周期；零值取 1 小时。
	RetentionInterval time.Duration
	// MinuteRetention 是分钟桶保留期；零值取固定的 7 天。
	MinuteRetention time.Duration
	// HourRetention 是小时桶保留期（PERF_METRICS_RETENTION_DAYS）；零值取
	// 400 天。只在 Store 同时实现 RetentionStore 时生效。
	HourRetention time.Duration
	// Logger 用于失败/丢弃告警；nil 回退 slog.Default。
	Logger *slog.Logger
	// Now 是时钟接缝（测试注入固定时间）；零值使用 time.Now。
	Now func() time.Time
}

// aggregate 是一个分钟桶的累计绝对量（不是增量）。
type aggregate struct {
	success int64
	failed  int64
	refused int64

	latencyCount int64
	latencySumMS float64
	latency      [HistogramBuckets]int64

	ttftCount int64
	ttft      [HistogramBuckets]int64

	completion int64
	generation float64
}

func (a *aggregate) addObservation(obs Observation) {
	switch obs.Status {
	case StatusSuccess:
		a.success++
	case StatusError:
		a.failed++
	case StatusRefused:
		a.refused++
	default:
		// 未知状态不应进入采样；调用方（billing）也只映射 0/1/2。
		return
	}

	if obs.LatencyMS != nil && *obs.LatencyMS >= 0 {
		latency := *obs.LatencyMS
		a.latencyCount++
		a.latencySumMS += float64(latency)
		if idx := BucketIndex(latency); idx >= 0 {
			a.latency[idx]++
		}

		// generation 派生（platform-05 §3.5.1）：流式且 TTFT 有效时 =
		// latency - TTFT，否则 = latency。负值钳到 0。
		generationMS := latency
		if obs.Stream && obs.FirstTokenMS != nil && *obs.FirstTokenMS >= 0 && *obs.FirstTokenMS <= latency {
			generationMS = latency - *obs.FirstTokenMS
		}
		a.generation += float64(generationMS) / 1000
	}

	if obs.Stream && obs.FirstTokenMS != nil && *obs.FirstTokenMS >= 0 {
		ttft := *obs.FirstTokenMS
		a.ttftCount++
		if idx := BucketIndex(ttft); idx >= 0 {
			a.ttft[idx]++
		}
	}

	if obs.CompletionTokens > 0 {
		a.completion += obs.CompletionTokens
	}
}

// bucketKey 是进程内分钟桶的键。渠道不进入聚合键（platform-05 §3.5.3
// 基数护栏）；模型截断到 128 字节。
type bucketKey struct {
	start time.Time
	model string
}

type entry struct {
	agg aggregate
	// seq 每次收到样本就递增；flush 时随快照写入，存储侧只在更大序号时覆盖。
	seq   uint64
	dirty bool
}

// Collector 是进程内采样 + 周期批量刷写的采集器。
//
// 并发契约：Observe 只持有一把互斥锁做常数级内存更新；Flush 在持锁期间只
// 复制快照，数据库 I/O 在锁外执行。Observe 绝不阻塞在数据库上。
type Collector struct {
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	epoch string

	mu       sync.Mutex
	buckets  map[bucketKey]*entry
	started  time.Time
	stopped  bool
	dropped  uint64
	lastData time.Time // 最近一次「确实写入了桶」的成功 flush
	lastOK   time.Time // 最近一次成功 flush（可无数据）
	// lastRetention 是最近一次保留清理尝试；只由周期任务推进。
	lastRetention time.Time
	failures      int // 连续失败周期数：3 次即判定停摆

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewCollector 构造采集器。Store 必填；FlushInterval 非法时回落到默认 5m，
// 保证装配遗漏不会把周期变成 0（0 会退化成忙循环）。
func NewCollector(cfg Config) (*Collector, error) {
	if cfg.Store == nil {
		return nil, errors.New("perfmetrics: Config.Store is required")
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.LateWindow <= 0 {
		cfg.LateWindow = defaultLateWindow
	}
	if cfg.RetentionInterval <= 0 {
		cfg.RetentionInterval = defaultRetentionInterval
	}
	if cfg.MinuteRetention <= 0 {
		cfg.MinuteRetention = MinuteRetention
	}
	if cfg.HourRetention <= 0 {
		cfg.HourRetention = defaultHourRetention
	}
	if cfg.Node == "" {
		cfg.Node = "unknown"
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	started := now().UTC()
	return &Collector{
		cfg:           cfg,
		log:           logger,
		now:           now,
		epoch:         newEpoch(),
		buckets:       make(map[bucketKey]*entry),
		started:       started,
		lastOK:        started,
		lastRetention: started,
	}, nil
}

// Observe 记录一次请求完成样本。nil/已停止的采集器是 no-op，调用方无需判空
// （但生产热路径仍应只挂 nil 接口以做到禁用时零调用）。
func (c *Collector) Observe(obs Observation) {
	if c == nil {
		return
	}
	// 未知状态（含 refunded=3）不进入采样：成功率只认完成时三态。
	if obs.Status > StatusError {
		return
	}
	observedAt := obs.ObservedAt
	if observedAt.IsZero() {
		observedAt = c.now()
	}
	minute := observedAt.UTC().Truncate(time.Minute)
	model := normalizeModel(obs.Model)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}

	key := bucketKey{start: minute, model: model}
	item := c.buckets[key]
	if item == nil {
		if len(c.buckets) >= maxBuckets {
			// 内存护栏：丢弃新键而不是无界增长。中继不受影响，flush 后
			// 可通过 dropped 指标/日志发现管道异常。
			c.dropped++
			return
		}
		item = &entry{}
		c.buckets[key] = item
	}
	item.agg.addObservation(obs)
	item.seq++
	item.dirty = true
}

// Flush 把已闭合分钟桶的绝对快照批量写入 Store。
//
// force=true 时连同当前（未闭合）分钟一起写，用于停机排空。失败时条目保持
// dirty，下一周期原样重试；由于快照是绝对量且序号只增，重试不会双计。
func (c *Collector) Flush(ctx context.Context, force bool) error {
	if c == nil {
		return ErrDisabled
	}
	now := c.now().UTC()
	cutoff := now.Truncate(time.Minute)

	c.mu.Lock()
	if c.stopped && !force {
		c.mu.Unlock()
		return ErrDisabled
	}
	snapshots := make([]Snapshot, 0, len(c.buckets))
	for key, item := range c.buckets {
		if !item.dirty {
			continue
		}
		if !force && !key.start.Before(cutoff) {
			continue
		}
		snapshots = append(snapshots, c.snapshotLocked(key, item))
	}
	c.mu.Unlock()

	if len(snapshots) == 0 {
		c.markFlush(now, true, false)
		return nil
	}
	// 稳定排序让测试与日志可复现；对存储语义没有影响。
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].BucketStart.Equal(snapshots[j].BucketStart) {
			return snapshots[i].Model < snapshots[j].Model
		}
		return snapshots[i].BucketStart.Before(snapshots[j].BucketStart)
	})

	if err := c.cfg.Store.UpsertBuckets(ctx, snapshots); err != nil {
		c.markFlush(now, false, false)
		c.log.Warn("perfmetrics: flush failed; will retry next interval", "error", err, "buckets", len(snapshots))
		return fmt.Errorf("perfmetrics: flush: %w", err)
	}

	c.mu.Lock()
	for _, snap := range snapshots {
		key := bucketKey{start: snap.BucketStart, model: snap.Model}
		item := c.buckets[key]
		// 只有「复制快照之后没有新样本」的条目才能标 clean；否则保留 dirty，
		// 下轮用更大的 Seq 重写累计绝对量。
		if item != nil && item.seq == snap.Seq {
			item.dirty = false
		}
	}
	c.mu.Unlock()
	c.markFlush(now, true, true)
	c.evict(now)
	return nil
}

func (c *Collector) snapshotLocked(key bucketKey, item *entry) Snapshot {
	return Snapshot{
		BucketStart:       key.start,
		Model:             key.model,
		Node:              c.cfg.Node,
		Epoch:             c.epoch,
		Seq:               item.seq,
		Success:           item.agg.success,
		Error:             item.agg.failed,
		Refused:           item.agg.refused,
		LatencyCount:      item.agg.latencyCount,
		LatencySumMS:      item.agg.latencySumMS,
		LatencyBuckets:    item.agg.latency,
		TTFTCount:         item.agg.ttftCount,
		TTFTBuckets:       item.agg.ttft,
		CompletionTokens:  item.agg.completion,
		GenerationSeconds: item.agg.generation,
	}
}

// markFlush 更新健康状态。success 表示本轮 flush 成功；wrote 表示本轮确实
// 写入了桶（停摆判定的 reference 只在 wrote 时推进）。空 flush 成功只推进
// lastOK，不伪造「桶更新」。
func (c *Collector) markFlush(now time.Time, success, wrote bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !success {
		c.failures++
		return
	}
	c.failures = 0
	c.lastOK = now
	if wrote {
		c.lastData = now
	}
}

// evict 清理超过迟到窗口的进程内条目。脏条目在窗口外被丢弃时计入 dropped
// （监控数据可丢，计费链路不受影响）。
func (c *Collector) evict(now time.Time) {
	cutoff := now.Add(-c.cfg.LateWindow)
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, item := range c.buckets {
		// 以分钟桶的结束时刻衡量迟到窗口。
		if key.start.Add(time.Minute).After(cutoff) {
			continue
		}
		if item.dirty {
			c.dropped++
		}
		delete(c.buckets, key)
	}
}

// Start 启动周期刷写 goroutine。重复调用会被忽略（以第一次为准）。
func (c *Collector) Start(parent context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.cancel != nil || c.stopped {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.cfg.FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				flushCtx, cancel := context.WithTimeout(ctx, flushTimeout)
				_ = c.Flush(flushCtx, false)
				cancel()
				// 保留清理与周期刷写解耦：即使 flush 失败，也按自己的节奏
				// 尝试物化/清理（指标库暂时不可用时只记告警）。
				c.maybeRunRetention(ctx, c.now().UTC())
			}
		}
	}()
}

// maybeRunRetention 按 RetentionInterval 触发一次指标库保留清理。
//
// 生产 Store（store.ModelPerfRepo）同时实现 RetentionStore：先物化分钟→小时，
// 再删除超过 7 天的分钟行与超过 PERF_METRICS_RETENTION_DAYS 的小时行。若未来
// 换成自带 retention 的指标库产品，实现方不必实现 RetentionStore，本方法
// 会自动跳过。失败只告警并在下一周期重试，绝不影响中继/计费。
func (c *Collector) maybeRunRetention(parent context.Context, now time.Time) {
	if c == nil {
		return
	}
	store, ok := c.cfg.Store.(RetentionStore)
	if !ok {
		return
	}
	c.mu.Lock()
	if now.Sub(c.lastRetention) < c.cfg.RetentionInterval {
		c.mu.Unlock()
		return
	}
	c.lastRetention = now
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, retentionTimeout)
	defer cancel()
	result, err := store.RunRetention(ctx, RetentionOptions{
		Now:             now,
		MinuteRetention: c.cfg.MinuteRetention,
		HourRetention:   c.cfg.HourRetention,
	})
	if err != nil {
		c.log.Warn("perfmetrics: retention failed; will retry next interval", "error", err)
		return
	}
	if result.Locked {
		// 另一个副本正在清理：正常现象，不刷 Info 日志。
		return
	}
	c.log.Info("perfmetrics: retention completed",
		"hours_materialized", result.HoursMaterialized,
		"minutes_deleted", result.MinutesDeleted,
		"hours_deleted", result.HoursDeleted)
}

// Stop 取消周期任务并执行一次强制 flush（包含当前分钟），用于停机排空。
// 幂等；必须在关闭日志库之前调用。
func (c *Collector) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.mu.Lock()
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.wg.Wait()

		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
		defer cancel()
		if err := c.Flush(ctx, true); err != nil {
			c.log.Warn("perfmetrics: shutdown flush failed; monitor data may be lost", "error", err)
		}
	})
}

// ModelPerfBuckets 实现 Source。updatedAt 取「窗口内最近写入」与「本进程
// 最近一次有数据 flush」的较大值：前者覆盖重启后已有数据/其它副本的写入，
// 后者覆盖刚写入但查询窗口尚未包含的边界情况。
//
// 连续 3 个周期 flush 失败时返回 ReasonPipelineStalled 的 UnavailableError，
// 让 /api/model-perf 立即进入停摆闸门（而不是再等满 3×周期的时间阈值）。
// 无数据导致的时间陈旧由 httpapi 的 `now-updatedAt > 3×interval` 判定；
// updatedAt 本身保持旧值。
func (c *Collector) ModelPerfBuckets(ctx context.Context, start, end time.Time, granularity, model string) ([]Bucket, time.Time, error) {
	if c == nil {
		return nil, time.Time{}, ErrDisabled
	}
	buckets, stored, err := c.cfg.Store.ModelPerfBuckets(ctx, start, end, granularity, model)
	if err != nil {
		return nil, time.Time{}, err
	}
	if c.FailureStreak() >= 3 {
		// 3× 失败停摆：即使窗口里仍有旧桶，读面也必须显示不可用，
		// 不得让陈旧数字看起来仍然新鲜。多副本场景下先看共享存储是否仍
		// 有新鲜写入（别的副本可能正常），避免单副本故障误伤全局看板。
		if stored.IsZero() || c.now().UTC().Sub(stored) > 3*c.cfg.FlushInterval {
			return nil, time.Time{}, &UnavailableError{Reason: ReasonPipelineStalled}
		}
	}
	if last := c.LastDataAt(); last.After(stored) {
		stored = last
	}
	return buckets, stored, nil
}

// Stalled 实现 3× 停摆判据：连续 3 次 flush 失败，或最近一次「有数据的
// flush」距今超过 3 个刷写周期。与 httpapi 的 pipeline.stale 同一口径。
func (c *Collector) Stalled() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures >= 3 {
		return true
	}
	reference := c.lastData
	if reference.IsZero() {
		reference = c.started
	}
	return c.now().UTC().Sub(reference) > 3*c.cfg.FlushInterval
}

// LastDataAt 返回最近一次写入桶的时间（零值表示尚未写入过数据）。
func (c *Collector) LastDataAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastData
}

// LastSuccessAt 返回最近一次成功 flush 的时间（可能没有数据）。
func (c *Collector) LastSuccessAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastOK
}

// FailureStreak 返回连续失败周期数（测试/诊断用）。
func (c *Collector) FailureStreak() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}

// Dropped 返回因内存上限/迟到窗口被丢弃的桶数。
func (c *Collector) Dropped() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

func normalizeModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		// usage_logs.model 非空；缺模型时用稳定占位值而不是空标签。
		return "unknown"
	}
	if len(model) > maxModelLen {
		// 模型名是用户可控输入，与 usage_logs 的 varchar(128) 同口径截断。
		model = model[:maxModelLen]
	}
	return model
}

// epoch 是进程启动标识；放在 Config 之外是因为它由构造期生成、不参与装配。
// 见 Collector.epoch 字段（通过 newEpoch 注入）。
func newEpoch() string { return uuid.NewString() }
