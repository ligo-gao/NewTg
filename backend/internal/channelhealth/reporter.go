package channelhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

// Reporter 的常量全部是**进程内**资源参数，不是站点设置：
//   - 冷却窗口只用于抑制同一渠道的重复上报，条件 UPDATE 才是幂等真源，
//     因此不需要 Redis（R-M4-13「不新增 Redis 依赖」的 M4-a 侧）；
//   - 队列满时丢弃并计数，绝不阻塞中继响应（brief 第 2 条）。
const (
	defaultReportCooldown = 10 * time.Second
	defaultReportQueue    = 256
	// autoDisableStoreTimeout 是后台状态迁移的单次上限。请求上下文在响应
	// 完成后即取消，后台写必须使用独立且有时限的 context。
	autoDisableStoreTimeout = 5 * time.Second
	// autoDisableSideEffectTimeout 是审计/通知的单次上限。两者都是
	// best-effort：失败只 Warn，绝不回滚已提交的状态迁移（R-M4-09）。
	autoDisableSideEffectTimeout = 5 * time.Second
	// autoDisableDrainTimeout 是停机排空队列的总预算（I-2）。ctx 取消后
	// Reporter 仍会处理已判定的事件，但最多再花这么久，避免优雅停机被
	// 积压事件无限拖长。
	autoDisableDrainTimeout = 5 * time.Second
)

// AutoDisableStore 是 Reporter 需要的存储面（生产 *store.ChannelRepo）。
type AutoDisableStore interface {
	AutoDisable(ctx context.Context, id int64, reason string) (*store.Channel, bool, error)
}

// AuditWriter 是审计面（生产 *audit.Logger）。nil 表示跳过审计。
type AuditWriter interface {
	LogEvent(ctx context.Context, e audit.Event) error
}

// AutoDisableNotifier 是站内通知面（生产 *notify.Service）。
// nil 表示跳过通知；email 不在 M4 范围（R-M4-18）。
type AutoDisableNotifier interface {
	DeliverChannelAutoDisabledInApp(ctx context.Context, in notify.ChannelAutoDisabledInput) (int, error)
}

// ReporterConfig 是 Reporter 的显式依赖。
type ReporterConfig struct {
	// Store 必填：条件更新是唯一实际的状态迁移。
	Store AutoDisableStore
	// Config 是设置快照读取面；nil 时按注册表默认（开关 false）处理。
	Config SettingsSource
	// Audit 可选；nil 时跳过审计（仅测试/降级）。
	Audit AuditWriter
	// Notifier 可选；nil 时跳过站内通知（测试/降级）。
	Notifier AutoDisableNotifier
	Logger   *slog.Logger
	// Now 是冷却窗口的时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// Cooldown 覆盖默认 10s 冷却；<=0 使用默认值。
	Cooldown time.Duration
	// QueueSize 覆盖默认 256 的入队缓冲；<=0 使用默认值。
	QueueSize int
	// ChannelsURL 是站内通知里的渠道管理链接（可为相对路径）。
	ChannelsURL string
}

// attemptFailure 是入队的一条失败事实。
//
// 判定在 ReportAttemptFailure 的调用线程里同步完成（纯 CPU、无 I/O），
// 入队的只有「已判定应禁用」的结论。这样响应体不需要跨 goroutine 传递，
// 也不会因为队列里堆着全量 body 放大内存。
type attemptFailure struct {
	channelID int64
	decision  Decision
}

// Reporter 把「执行层失败 → 条件迁移 → 审计/通知」串成异步副作用。
//
// 生命周期：platform 在装配期构造并 Start(ctx)，ctx 结束即退出唯一后台
// goroutine（Wait 供测试/停机等待）。ReportAttemptFailure 永不阻塞：
// 纯判定 + 冷却检查 + 非阻塞入队，队列满则丢弃并计数。
type Reporter struct {
	store       AutoDisableStore
	config      SettingsSource
	audit       AuditWriter
	notifier    AutoDisableNotifier
	log         *slog.Logger
	now         func() time.Time
	cooldown    time.Duration
	channelsURL string
	events      chan attemptFailure

	started atomic.Bool
	wg      sync.WaitGroup

	// mu 保护 lastReport。同一渠道在冷却窗口内的重复失败只入队一次：
	// 这是负载抑制，不是正确性保证——真正的「只迁移一次」由
	// store.AutoDisable 的 WHERE status=1 AND auto_ban=true 条件更新保证。
	mu         sync.Mutex
	lastReport map[int64]time.Time

	dropped atomic.Uint64
	applied atomic.Uint64
	failed  atomic.Uint64
}

// NewReporter 构造 Reporter。Store 必填；其余依赖允许为 nil（降级）。
func NewReporter(cfg ReporterConfig) (*Reporter, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("channelhealth: reporter requires an AutoDisableStore")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = defaultReportCooldown
	}
	queue := cfg.QueueSize
	if queue <= 0 {
		queue = defaultReportQueue
	}
	return &Reporter{
		store:       cfg.Store,
		config:      cfg.Config,
		audit:       cfg.Audit,
		notifier:    cfg.Notifier,
		log:         log,
		now:         now,
		cooldown:    cooldown,
		channelsURL: cfg.ChannelsURL,
		events:      make(chan attemptFailure, queue),
		lastReport:  make(map[int64]time.Time),
	}, nil
}

// Start 启动唯一的后台处理 goroutine。重复调用是 no-op。
//
// 只起一个 goroutine 而不是「每条失败起一个」：自动禁用是低频且必须有序的
// 副作用（审计/通知在状态迁移之后），单消费者天然串行；ctx 取消即退出，
// 不存在 goroutine 泄漏（停机时队列里未处理的失败由下一次请求/体检重试）。
func (r *Reporter) Start(ctx context.Context) {
	if r == nil || !r.started.CompareAndSwap(false, true) {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-ctx.Done():
				// I-2：停机时先排空已判定的事件，而不是直接丢弃。排空预算
				// 有界（5s），且 platform 会在关闭 DB/日志库之前 Wait()，
				// 因此不会出现「排空写入撞上已关闭连接池」。
				r.drain()
				return
			case event := <-r.events:
				r.handle(ctx, event)
			}
		}
	}()
}

// drain 在停机窗口内处理队列中剩余的事件（最多 autoDisableDrainTimeout）。
//
// 只处理已经入队的事件：不再接受新上报（loop 已退出），也不阻塞停机超过
// 预算。每个事件的存储写/审计/通知各自仍有 5s 超时，因此单个事件的最坏
// 时长是常数级、不随队列长度线性放大停机时间。
func (r *Reporter) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), autoDisableDrainTimeout)
	defer cancel()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case event := <-r.events:
			r.handle(ctx, event)
		case <-ctx.Done():
			return
		default:
			return
		}
	}
}

// Wait 等待后台 goroutine 退出（测试与停机顺序断言用）。
func (r *Reporter) Wait() {
	if r != nil {
		r.wg.Wait()
	}
}

// ReportAttemptFailure 实现 httpapi.AttemptFailureReporter（M4-a 接缝）。
//
// 必须保持非阻塞：调用方是中继热路径，且该函数在错误响应写回之前同步执行。
// 所有 I/O（条件 UPDATE、审计、通知）都在后台 handle 中完成。
func (r *Reporter) ReportAttemptFailure(_ context.Context, channelID int, autoBan bool,
	class contract.ErrorClass, statusCode int, body []byte) {
	if r == nil || channelID <= 0 {
		return
	}
	decision, ok := DecideFailure(class, statusCode, body, autoBan, LoadDecisionSettings(r.config))
	if !ok {
		return
	}
	now := r.now()
	if !r.allowReport(int64(channelID), now) {
		return
	}
	select {
	case r.events <- attemptFailure{channelID: int64(channelID), decision: decision}:
	default:
		// 队列满：丢弃并计数。丢弃是安全的——渠道下一轮请求会再次命中同一
		// 触发条件并重新上报；绝不能为了入队而阻塞响应。
		dropped := r.dropped.Add(1)
		if dropped == 1 || dropped%100 == 0 {
			r.log.Warn("channelhealth: auto-disable report dropped (queue full)",
				"channel_id", channelID, "reason", decision.ReasonCode, "dropped_total", dropped)
		}
	}
}

// RunOnce 同步处理当前队列里的全部事件，返回处理条数。
//
// 生产不使用（由 Start 的后台循环消费）；测试用它把异步副作用变成确定性
// 断言，避免 sleep/轮询。与 Start 同时使用会竞争队列，调用方不得混用。
func (r *Reporter) RunOnce(ctx context.Context) int {
	if r == nil {
		return 0
	}
	processed := 0
	for {
		select {
		case event := <-r.events:
			r.handle(ctx, event)
			processed++
		default:
			return processed
		}
	}
}

// Dropped 返回队列满导致的丢弃总数（可观测性/测试）。
func (r *Reporter) Dropped() uint64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// Applied 返回实际完成状态迁移的次数（changed=true）。
func (r *Reporter) Applied() uint64 {
	if r == nil {
		return 0
	}
	return r.applied.Load()
}

// Failed 返回条件更新失败（DB 错误）的次数。
func (r *Reporter) Failed() uint64 {
	if r == nil {
		return 0
	}
	return r.failed.Load()
}

// allowReport 在冷却窗口内抑制同一渠道的重复入队。
//
// 冷却在**入队前**检查并写入时间戳：并发的 100 次失败里只有第一个能通过，
// 其余直接返回。即使冷却失效，条件更新也只会让第一次 changed=true；
// 冷却的意义是把「100 次 DB 往返」压成 1 次。
func (r *Reporter) allowReport(channelID int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastReport[channelID]; ok && now.Sub(last) < r.cooldown {
		return false
	}
	// 只记录「上报尝试」而不是「迁移成功」：即使本次因为并发/人工改态没有
	// 迁移，短时间内重复上报也不会改变结果，抑制它们不损失正确性。
	// 渠道数量级很小（数十～数百），map 不做过期清理。
	r.lastReport[channelID] = now
	return true
}

// handle 执行一次完整的自动禁用副作用链：条件迁移 → 审计 → 站内通知。
//
// 状态迁移是唯一有回滚语义的步骤；审计/通知失败只记 Warn，不回滚状态
// （R-M4-09：通知是 best-effort，渠道已经不可用这一事实必须保留）。
func (r *Reporter) handle(ctx context.Context, event attemptFailure) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoDisableStoreTimeout)
	channel, changed, err := r.store.AutoDisable(writeCtx, event.channelID, event.decision.ReasonCode)
	cancel()
	if err != nil {
		r.failed.Add(1)
		// 只记录 channel_id/reason 与存储错误：不含响应体、base_url 或凭据。
		r.log.Warn("channelhealth: auto-disable update failed",
			"channel_id", event.channelID, "reason", event.decision.ReasonCode, "error", err)
		return
	}
	if !changed {
		// 条件不满足：渠道已被人工改态、已自动禁用、auto_ban 被关闭，或行已删。
		// 这是并发上报的预期路径，不写审计、不发通知。
		r.log.Debug("channelhealth: auto-disable skipped (state changed)",
			"channel_id", event.channelID, "reason", event.decision.ReasonCode)
		return
	}
	r.applied.Add(1)
	// 结构化日志只含稳定 code，不含上游错误原文（GC-6）。
	r.log.Info("channelhealth: channel auto-disabled",
		"channel_id", event.channelID, "reason", event.decision.ReasonCode, "trigger", event.decision.Trigger)
	r.recordAudit(channel, event)
	r.notifyAdmins(channel, event)
}

// recordAudit 写 channel.auto_disable 审计。
//
// R-M4-10：Actor=0 + Username="system"；detail 只允许非秘密事实
// （channel_id/channel_name/reason/trigger），before/after 只允许 status 与
// reason code。写失败只 Warn——审计库不可用不得把已迁移的渠道回滚。
func (r *Reporter) recordAudit(channel *store.Channel, event attemptFailure) {
	if r.audit == nil {
		return
	}
	detailFields := map[string]any{
		"channel_id": event.channelID,
		"reason":     event.decision.ReasonCode,
		"trigger":    event.decision.Trigger,
	}
	if channel != nil && channel.Name != "" {
		detailFields["channel_name"] = channel.Name
	}
	detail, err := json.Marshal(detailFields)
	if err != nil {
		r.log.Warn("channelhealth: marshal audit detail failed", "channel_id", event.channelID, "error", err)
		return
	}
	before, _ := json.Marshal(map[string]any{"status": 1})
	after, _ := json.Marshal(map[string]any{
		"status":               3,
		"auto_disabled_reason": event.decision.ReasonCode,
	})
	ctx, cancel := context.WithTimeout(context.Background(), autoDisableSideEffectTimeout)
	defer cancel()
	err = r.audit.LogEvent(ctx, audit.Event{
		Action:   audit.ActionChannelAutoDisable,
		Actor:    0,
		Username: "system",
		Detail:   detail,
		Before:   before,
		After:    after,
	})
	if err != nil {
		r.log.Warn("channelhealth: auto-disable audit write failed",
			"channel_id", event.channelID, "error", err)
	}
}

// notifyAdmins 给全体管理员写站内通知（best-effort，R-M4-09）。
//
// 去重键基于本次迁移的 updated_at：同一次迁移在重试/并发下只会产生一组
// 投递行；人工重新启用后再次自动禁用会得到新的 updated_at，因此是新事件。
func (r *Reporter) notifyAdmins(channel *store.Channel, event attemptFailure) {
	if r.notifier == nil || channel == nil {
		return
	}
	dedupe := fmt.Sprintf("channel_auto_disabled:%d:%s",
		channel.ID, channel.UpdatedAt.UTC().Format(time.RFC3339Nano))
	ctx, cancel := context.WithTimeout(context.Background(), autoDisableSideEffectTimeout)
	defer cancel()
	delivered, err := r.notifier.DeliverChannelAutoDisabledInApp(ctx, notify.ChannelAutoDisabledInput{
		ChannelID:   channel.ID,
		ChannelName: channel.Name,
		ReasonCode:  event.decision.ReasonCode,
		DisabledAt:  channel.UpdatedAt,
		ChannelsURL: r.channelsURL,
		DedupeKey:   dedupe,
	})
	if err != nil {
		r.log.Warn("channelhealth: auto-disable notification failed",
			"channel_id", channel.ID, "delivered", delivered, "error", err)
	}
}
