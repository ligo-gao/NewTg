package notify

// N8 通知 Webhook 发送/重试 worker（billing-06 §3.2 / §9.7，modules-08
// F-08-20）。
//
// 冻结语义：
//   - 扫描 channel='webhook' AND status='queued' AND (next_retry_at IS NULL
//     OR next_retry_at <= now)；
//   - 每次尝试调用 WebhookSender.Send，payload 使用投递行的 jsonb 内容
//     （PostgreSQL 可能重排/格式化，worker 发送并签名的就是读到的这份字节），
//     event 由 category 映射到 F-08-20 的订阅事件名；
//   - 成功 → sent；失败 → attempt+1，1/5/25 分钟后重试，第 4 次失败 →
//     failed（初投 + 3 次重试）；
//   - 连续失败（Redis notify:webhook:fail_streak）> 50 → 置
//     notify:webhook:paused；暂停期间不领取、不发送，行保持 queued；
//   - 测试端点（SendTest）不经过本 worker，也不受暂停影响。
//
// 【单副本保证】行占用用 `SELECT ... FOR UPDATE SKIP LOCKED`（见
// WebhookDeliveryClaim）：多个副本可以同时扫描，但同一行只可能被一个持有
// 行锁的副本发送。没有额外使用 Redis 租约——行锁本身就是权威，租约只会
// 引入「锁过期但旧副本仍在发」的第二种竞态。进程崩溃时事务回滚，行自动
// 回到 queued，下一轮（或重启后的首轮）重新领取。
//
// 【停机】RunWebhookWorkerLoop 在 ctx 取消后立即返回；进行中的 Send 使用
// 同一个 ctx，取消会让请求失败并回滚占用（行保持 queued），不会留下
// 「发送中」的悬挂状态。platform.runWorker 用 WaitGroup 等待本循环退出后
// 才关闭连接池。
//
// 【脱敏】日志与 last_error 只允许固定分类文案与 HTTP 状态码：不记录
// secret、完整 URL（含 query）、payload 或响应体。WebhookSender 已保证
// 其错误文本不含这些内容，本层再做一次分类映射，防止未来错误包装漂移。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// WebhookFailStreakKey 是连续投递失败计数（成功一次即清零）。
	WebhookFailStreakKey = "notify:webhook:fail_streak"
	// WebhookPausedKey 存在即表示推送已自动暂停；只能由 resume 端点清除。
	WebhookPausedKey = "notify:webhook:paused"
	// WebhookPauseThreshold 是冻结阈值：连续失败**超过** 50 次暂停（第 51 次）。
	WebhookPauseThreshold = 50

	// DefaultWebhookWorkerInterval 是 worker 扫描周期（1 分钟）。重试间隔
	// 1/5/25 分钟都是它的整数倍，最坏情况只会晚一个周期发出。
	DefaultWebhookWorkerInterval = time.Minute
	// DefaultWebhookBatchSize 是单轮最多处理的投递数。它约束单轮总时长
	// （最坏 = 批次 × 10s 投递超时），避免一轮扫描长期占住 worker。
	DefaultWebhookBatchSize = 50

	// webhookMaxAttempts 是冻结的尝试总数：初投 + 3 次重试。
	webhookMaxAttempts = 4
)

// webhookRetryDelays 是 F-08-20 冻结的重试间隔，下标 = 已完成失败次数-1。
var webhookRetryDelays = [...]time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	25 * time.Minute,
}

// webhookEventByCategory 把投递行的 category 映射到 F-08-20 的订阅事件名。
//
// 这是 worker 与业务生产者之间的唯一词汇表：投递行只存 category（模板
// 注册表的键），而签名请求的订阅面只有 balance_low / payment_success /
// announcement / user_signup 四组。未映射的 category 不应入队；worker 把
// 它当作**终态失败**（不重试、不计入端点失败连续数），以便排障时可见，
// 而不是每小时重试一个永远不可能订阅的事件。
//
// billing.quota_exhausted 同属「余额不足提醒」的用户可见语义，归入
// balance_low：管理员勾选余额提醒后，额度耗尽也应收到。
var webhookEventByCategory = map[string]string{
	"billing.balance_low":     "balance_low",
	"billing.quota_exhausted": "balance_low",
	"billing.payment_success": "payment_success",
	"system.announcement":     "announcement",
	"auth.welcome":            "user_signup",
}

// WebhookEventForCategory 返回 category 对应的 F-08-20 事件名。
// ok=false 表示该 category 没有 Webhook 订阅面（调用方必须 fail-closed）。
func WebhookEventForCategory(category string) (string, bool) {
	event, ok := webhookEventByCategory[category]
	return event, ok
}

// WebhookWorkerSender 是 worker 需要的发送器能力。
//
// Active 与 Send 分离：开关关闭/配置缺失时 worker 不应领取投递（否则每次
// 领取都要失败并消耗一次重试），业务事件应原样留在队列等待配置修复。
type WebhookWorkerSender interface {
	Send(ctx context.Context, payload []byte, event, eventID string) (WebhookResult, error)
	Active(ctx context.Context) bool
}

// WebhookWorkerDeps 是 worker 的运行依赖与可调参数（测试注入短周期/时钟）。
type WebhookWorkerDeps struct {
	// Store 是投递记录的存储（必填）。
	Store *Store
	// Sender 是出站发送器（必填）。
	Sender WebhookWorkerSender
	// Redis 保存暂停状态与连续失败计数（必填）。
	Redis *redis.Client
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Now 为空时使用 time.Now；生产与测试都应看到同一个「现在」。
	Now func() time.Time
	// Interval 覆盖扫描周期（测试注入短周期）。
	Interval time.Duration
	// BatchSize 覆盖单轮最大投递数。
	BatchSize int
}

// WebhookWorkerResult 是一轮扫描的实际效果。
type WebhookWorkerResult struct {
	// Claimed 是本轮领取的投递数。
	Claimed int
	// Sent 是成功置为 sent 的投递数。
	Sent int
	// Failed 是失败后仍可重试（queued）或已终态 failed 的投递数。
	Failed int
	// Suppressed 是因事件不再订阅而终止的投递数。
	Suppressed int
	// Paused 表示本轮结束时暂停标记已生效。
	Paused bool
}

// WebhookWorker 可并发使用（每次 RunOnce 之间由调用方串行）。
type WebhookWorker struct {
	store     *Store
	sender    WebhookWorkerSender
	redis     *redis.Client
	log       *slog.Logger
	now       func() time.Time
	interval  time.Duration
	batchSize int
}

// NewWebhookWorker 构造 worker；依赖缺失是装配错误，必须在启动期暴露。
func NewWebhookWorker(deps WebhookWorkerDeps) (*WebhookWorker, error) {
	if deps.Store == nil {
		return nil, errors.New("notify: webhook worker requires a store")
	}
	if deps.Sender == nil {
		return nil, errors.New("notify: webhook worker requires a sender")
	}
	if deps.Redis == nil {
		return nil, errors.New("notify: webhook worker requires redis for pause state")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	interval := deps.Interval
	if interval <= 0 {
		interval = DefaultWebhookWorkerInterval
	}
	batchSize := deps.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultWebhookBatchSize
	}
	return &WebhookWorker{
		store: deps.Store, sender: deps.Sender, redis: deps.Redis, log: log,
		now: now, interval: interval, batchSize: batchSize,
	}, nil
}

// RunWebhookWorkerLoop 阻塞运行 N8 循环直到 ctx 取消。
//
// 启动即扫一轮：重启后不必等一个完整周期才继续处理积压的投递；崩溃时
// 被回滚的占用也会在首轮被重新领取。
func RunWebhookWorkerLoop(ctx context.Context, deps WebhookWorkerDeps) error {
	worker, err := NewWebhookWorker(deps)
	if err != nil {
		return err
	}
	// 已取消的上下文不再启动任何 DB/网络负载（停机竞态）。
	if ctx.Err() != nil {
		return nil
	}
	worker.log.Info("webhook worker started",
		"interval", worker.interval,
		"batch_size", worker.batchSize,
	)
	worker.RunOnce(ctx)

	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 优雅停机不是错误，与其它 worker 循环一致。
			return nil
		case <-ticker.C:
			worker.RunOnce(ctx)
		}
	}
}

// RunOnce 执行一轮扫描并返回计数。单轮内的 DB/Redis 故障只记录，不向上
// 抛错：下一轮会重试，把「扫描失败」伪装成「没有待发投递」才是事故。
func (w *WebhookWorker) RunOnce(ctx context.Context) WebhookWorkerResult {
	var result WebhookWorkerResult
	if ctx.Err() != nil {
		return result
	}
	// 关停开关优先于队列：暂停期间不领取任何行（保留 queued）。
	if w.isPaused(ctx) {
		result.Paused = true
		return result
	}
	// 未启用/未配置时不领取：不消耗尝试次数，配置修好后原样发出。
	if !w.sender.Active(ctx) {
		w.log.Debug("webhook worker skipped: sender is not active")
		return result
	}

	now := w.now().UTC()
	for i := 0; i < w.batchSize; i++ {
		if ctx.Err() != nil {
			break
		}
		// 每投递一条都复查暂停：第 51 次失败后必须立刻停手，而不是把
		// 本轮剩余行的失败也打进同一个端点。
		if w.isPaused(ctx) {
			result.Paused = true
			break
		}
		claim, ok, err := w.store.ClaimWebhookDelivery(ctx, now)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("webhook worker: claim failed", "error", err)
			}
			return result
		}
		if !ok {
			break
		}
		result.Claimed++
		outcome := w.deliver(ctx, claim)
		switch outcome {
		case webhookOutcomeSent:
			result.Sent++
		case webhookOutcomeSuppressed:
			result.Suppressed++
		case webhookOutcomeFailed:
			result.Failed++
		case webhookOutcomeReleased:
			// 配置在投递中途关闭/暂停：行保持 queued，不计数。
			return result
		}
	}
	if result.Claimed > 0 {
		w.log.Info("webhook deliveries processed",
			"claimed", result.Claimed,
			"sent", result.Sent,
			"failed", result.Failed,
			"suppressed", result.Suppressed,
		)
	}
	return result
}

type webhookOutcome int

const (
	webhookOutcomeFailed webhookOutcome = iota
	webhookOutcomeSent
	webhookOutcomeSuppressed
	webhookOutcomeReleased
)

// deliver 投递一条已占用的记录。无论结果如何都必须关闭占用（提交或回滚）。
func (w *WebhookWorker) deliver(ctx context.Context, claim *WebhookDeliveryClaim) webhookOutcome {
	delivery := claim.Delivery
	event, ok := WebhookEventForCategory(delivery.Category)
	if !ok {
		// 生产者写入了没有 Webhook 订阅面的 category：终态失败，不重试，
		// 也不计入端点连续失败（这不是端点故障）。
		reason := "unsupported webhook category"
		if err := claim.MarkFailed(ctx, delivery.Attempt, nil, reason); err != nil {
			w.log.Error("webhook worker: mark unsupported delivery failed", "id", delivery.ID, "error", err)
		}
		w.log.Warn("webhook worker: delivery category has no webhook event",
			"id", delivery.ID, "category", delivery.Category)
		return webhookOutcomeFailed
	}
	if len(delivery.Payload) == 0 {
		// webhook 行的 payload 由生产者定义；为空时无法构造请求体，重试
		// 也不会变好，直接终态失败。
		if err := claim.MarkFailed(ctx, delivery.Attempt, nil, "webhook payload is missing"); err != nil {
			w.log.Error("webhook worker: mark empty payload delivery failed", "id", delivery.ID, "error", err)
		}
		return webhookOutcomeFailed
	}

	result, sendErr := w.sender.Send(ctx, delivery.Payload, event, webhookEventID(delivery))
	switch {
	case sendErr == nil && result.OK:
		if err := claim.MarkSent(ctx, delivery.Attempt+1); err != nil {
			w.log.Error("webhook worker: mark delivery sent", "id", delivery.ID, "error", err)
		}
		w.resetFailStreak(ctx)
		return webhookOutcomeSent
	case errors.Is(sendErr, ErrWebhookDisabled):
		// 开关在本轮中途被关闭：不消耗尝试，行保持 queued。
		claim.Release()
		return webhookOutcomeReleased
	case errors.Is(sendErr, ErrEventNotSubscribed):
		// 管理员改了订阅列表：这不是端点故障，终止该条投递而不是永久重试。
		if err := claim.MarkSuppressed(ctx, "event is not subscribed"); err != nil {
			w.log.Error("webhook worker: mark delivery suppressed", "id", delivery.ID, "error", err)
		}
		return webhookOutcomeSuppressed
	default:
		w.recordFailure(ctx, claim, delivery, event, result, sendErr)
		return webhookOutcomeFailed
	}
}

// recordFailure 写入一次失败并维护连续失败/暂停状态。
func (w *WebhookWorker) recordFailure(
	ctx context.Context,
	claim *WebhookDeliveryClaim,
	delivery Delivery,
	event string,
	result WebhookResult,
	sendErr error,
) {
	attempt := delivery.Attempt + 1
	next := retryAtForAttempt(w.now().UTC(), attempt)
	reason := webhookFailureReason(result, sendErr)

	if err := claim.MarkFailed(ctx, attempt, next, reason); err != nil {
		w.log.Error("webhook worker: mark delivery failed", "id", delivery.ID, "error", err)
	}
	// 日志只带非敏感事实：id/event/attempt/status/reason。绝不带 payload、
	// URL（含 query）、secret 或响应体。
	w.log.Warn("webhook delivery failed",
		"id", delivery.ID,
		"event", event,
		"attempt", attempt,
		"status_code", result.StatusCode,
		"reason", reason,
	)
	w.countFailure(ctx)
}

// retryAtForAttempt 返回第 attempt 次失败后的重试时刻；nil 表示已达终态。
//
// attempt 是**已经发生的尝试次数**：初投失败后为 1 → 1 分钟；第 2 次 → 5
// 分钟；第 3 次 → 25 分钟；第 4 次（= 初投 + 3 重试）→ failed。
func retryAtForAttempt(now time.Time, attempt int) *time.Time {
	index := attempt - 1
	if index < 0 || index >= len(webhookRetryDelays) {
		return nil
	}
	next := now.Add(webhookRetryDelays[index])
	return &next
}

// webhookFailureReason 把发送失败折叠成固定、可入库的文案。
//
// 绝不拼接底层 error/URL/响应体：url.Error 会带完整 URL（含 query token），
// 响应体可能含端点内部信息。last_error 列只承担「哪一类失败」的分类。
func webhookFailureReason(result WebhookResult, err error) string {
	switch {
	case errors.Is(err, ErrWebhookBlockedTarget):
		return "webhook target is not allowed"
	case errors.Is(err, ErrWebhookTransport):
		return "webhook request failed"
	case errors.Is(err, ErrWebhookNotConfigured):
		return "webhook is not configured"
	case err != nil:
		// 配置解析/密钥解封等内部错误：分类而不回显细节。
		return "webhook delivery failed"
	case !result.OK:
		// HTTP 状态码不是秘密，保留它便于管理员定位；响应体仍不入库。
		return fmt.Sprintf("endpoint returned http status %d", result.StatusCode)
	default:
		return "webhook delivery failed"
	}
}

// webhookEventID 是投递给下游的幂等锚：dedupe_key 是投递表的唯一业务键
// （至少一次 + 幂等键 = 有效一次）。缺失时退化为行 id，仍保证同一条
// 投递在重试之间不变。
func webhookEventID(delivery Delivery) string {
	if delivery.DedupeKey != "" {
		return delivery.DedupeKey
	}
	return fmt.Sprintf("delivery:%d", delivery.ID)
}

// isPaused 读取自动暂停标记。Redis 故障按「未暂停」处理并告警：暂停是
// 对故障端点的保护，不应因为缓存不可用而让所有通知永久停摆；真正的
// 发送正确性由投递行的行锁与重试状态保证。
func (w *WebhookWorker) isPaused(ctx context.Context) bool {
	exists, err := w.redis.Exists(ctx, WebhookPausedKey).Result()
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("webhook worker: pause state unavailable; continuing", "error", err)
		}
		return false
	}
	return exists > 0
}

// countFailure 递增连续失败计数；越过阈值时置暂停标记（无 TTL，只能人工
// resume）。计数失败不阻断投递（fail-open）。
func (w *WebhookWorker) countFailure(ctx context.Context) {
	streak, err := w.redis.Incr(ctx, WebhookFailStreakKey).Result()
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("webhook worker: fail streak unavailable", "error", err)
		}
		return
	}
	if streak <= WebhookPauseThreshold {
		return
	}
	if err := w.redis.Set(ctx, WebhookPausedKey, "1", 0).Err(); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("webhook worker: set pause flag failed", "error", err)
		}
		return
	}
	w.log.Error("webhook deliveries paused after consecutive failures",
		"streak", streak,
		"threshold", WebhookPauseThreshold,
	)
}

// resetFailStreak 在成功一次后清零连续失败计数（「连续」的字面语义）。
func (w *WebhookWorker) resetFailStreak(ctx context.Context) {
	if err := w.redis.Del(ctx, WebhookFailStreakKey).Err(); err != nil && ctx.Err() == nil {
		w.log.Warn("webhook worker: reset fail streak failed", "error", err)
	}
}
