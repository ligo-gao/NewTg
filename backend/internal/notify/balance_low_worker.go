package notify

// N6 低余额通知扫描 worker（billing-06 §3.3 / §9.6，modules-08 F-08-11 /
// F-08-20，tasks B4）。
//
// 冻结语义：
//   - 每 5 分钟扫描一次；启动即扫一轮（重启后不必等一个周期）；
//   - 仅当 `notify.webhook.enabled=true`、`notify.webhook.balance_low_threshold>0`
//     且 `notify.webhook.events` 订阅了 `balance_low` 时扫描；三者任一不满足
//     都直接跳过，不产生查询；
//   - 命中口径：`users.status='enabled' AND users.balance < threshold`；
//   - 每人每天最多一条投递：`dedupe_key='balance_low:<userID>:<YYYY-MM-DD>'`，
//     日期取**站点时区**（`site.timezone`），不是 UTC——同一时刻在不同时区
//     是不同业务日，跨副本必须得到同一个键；查询阶段即按该键排除当天已
//     通知用户（减少无效插入），UNIQUE(dedupe_key) 继续兜底多副本竞态；
//   - 投递行由 WebhookEventProducer 写入，category=`billing.balance_low`，
//     payload=`{event,user_id,balance,threshold,detected_at}`；N8 worker 负责
//     真正发送/重试；
//   - 多副本并发扫描是安全的：UNIQUE(dedupe_key) 让同一天同一用户只落一行，
//     因此不需要额外的 Redis 租约（N6 的幂等锚就是 dedupe_key）。
//
// 【去重与噪声】按天去重意味着用户持续低于阈值时**每天最多一条**；余额回升
// 后再次跌破，当天仍只有一条，次日可再发。这里刻意不维护
// `balance_alert_state` 一类状态表：状态表会引入「副本看到旧余额」的竞态，
// 而按天的 dedupe_key 是数据库唯一约束保证的强不变量。
//
// 【失败语义】扫描/入队失败只记 Warn（告警收敛在日志），绝不影响业务请求；
// 下一轮会重试同一批用户，已成功入队的用户由 dedupe_key 自动跳过。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	// DefaultBalanceLowScanInterval 是 N6 冻结的扫描周期（5 分钟）。
	DefaultBalanceLowScanInterval = 5 * time.Minute

	// KeySiteTimezone 是站点时区设置键，决定 dedupe_key 的「天」边界。
	KeySiteTimezone = "site.timezone"
)

// BalanceLowWorkerDeps 是扫描器的运行依赖与可调参数（测试注入时钟/时区）。
type BalanceLowWorkerDeps struct {
	// Store 读取命中用户并写入投递行（必填）。
	Store *Store
	// Producer 负责订阅过滤与投递行写入（必填）。
	Producer *WebhookEventProducer
	// Config 读取 notify.webhook.enabled / balance_low_threshold / site.timezone
	// （必填）。
	Config ConfigReader
	// Subscription 读取 notify.webhook.events 快照（必填）。未订阅 balance_low
	// 时整轮跳过，连用户查询都不做。
	Subscription WebhookEventSubscription
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Now 为空时使用 time.Now；生产与测试应看到同一个「现在」。
	Now func() time.Time
	// Location 覆盖站点时区（测试注入）。为空时每轮读取 site.timezone。
	Location *time.Location
	// Interval 覆盖扫描周期（测试注入短周期）。
	Interval time.Duration
}

// BalanceLowScanResult 是一轮扫描的实际效果。
type BalanceLowScanResult struct {
	// Skipped 表示本轮因开关关闭/阈值为 0/未订阅而没有扫描。
	Skipped bool
	// Scanned 是命中低余额条件**且当天尚未通知**的用户数（查询已排除
	// 当天已有 dedupe_key 的用户）。
	Scanned int
	// Enqueued 是真正写入的新投递行数（未被 dedupe 命中）。
	Enqueued int
	// Deduped 是查询之后、插入之前被其它副本抢先写入同一天投递行的用户数。
	// 正常单副本路径下当天已通知的用户已被 ListLowBalanceUsers 排除，因此
	// 这里只在多副本竞态或调用方绕过查询直接入队时非零。
	Deduped int
	// Failed 是入队失败的次数（DB/序列化错误；下一轮重试）。
	Failed int
}

// BalanceLowWorker 串行执行扫描轮次。
type BalanceLowWorker struct {
	store        *Store
	producer     *WebhookEventProducer
	cfg          ConfigReader
	subscription WebhookEventSubscription
	log          *slog.Logger
	now          func() time.Time
	location     *time.Location
	interval     time.Duration
}

// NewBalanceLowWorker 构造扫描器；依赖缺失是装配错误，必须在启动期暴露。
func NewBalanceLowWorker(deps BalanceLowWorkerDeps) (*BalanceLowWorker, error) {
	if deps.Store == nil {
		return nil, errors.New("notify: balance low worker requires a store")
	}
	if deps.Producer == nil {
		return nil, errors.New("notify: balance low worker requires a producer")
	}
	if deps.Config == nil {
		return nil, errors.New("notify: balance low worker requires a config reader")
	}
	if deps.Subscription == nil {
		return nil, errors.New("notify: balance low worker requires a subscription reader")
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
		interval = DefaultBalanceLowScanInterval
	}
	return &BalanceLowWorker{
		store: deps.Store, producer: deps.Producer, cfg: deps.Config,
		subscription: deps.Subscription, log: log, now: now,
		location: deps.Location, interval: interval,
	}, nil
}

// RunBalanceLowWorkerLoop 阻塞运行 N6 循环直到 ctx 取消。
func RunBalanceLowWorkerLoop(ctx context.Context, deps BalanceLowWorkerDeps) error {
	worker, err := NewBalanceLowWorker(deps)
	if err != nil {
		return err
	}
	// 已取消的上下文不再启动任何 DB 负载（停机竞态）。
	if ctx.Err() != nil {
		return nil
	}
	worker.log.Info("balance low worker started", "interval", worker.interval)
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

// RunOnce 执行一轮扫描并返回计数。DB 故障只记录，不向上抛错：下一轮重试；
// 把扫描失败伪装成「没有低余额用户」才是事故。
func (w *BalanceLowWorker) RunOnce(ctx context.Context) BalanceLowScanResult {
	var result BalanceLowScanResult
	if ctx.Err() != nil {
		result.Skipped = true
		return result
	}

	threshold, enabled, err := w.settings()
	if err != nil {
		// 配置损坏时 fail-closed：不扫描、不投递，日志保留分类错误。
		w.log.Warn("balance low worker: invalid webhook settings; skipping", "error", err)
		result.Skipped = true
		return result
	}
	if !enabled || threshold <= 0 {
		result.Skipped = true
		return result
	}
	event, ok := WebhookEventForCategory("billing.balance_low")
	if !ok || !w.subscription.Subscribed(event) {
		// 未订阅时整轮跳过：不查询用户，也不写永远不会发送的投递行。
		result.Skipped = true
		return result
	}

	now := w.now()
	day := now.In(w.siteLocation()).Format("2006-01-02")

	// 查询即排除当天已通知的用户：避免每轮为同一批持续低余额用户做注定
	// 被 UNIQUE(dedupe_key) 丢弃的插入。多副本竞态仍由唯一约束兜底。
	users, err := w.store.ListLowBalanceUsers(ctx, threshold, day)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("balance low worker: scan users failed", "error", err)
		}
		return result
	}
	result.Scanned = len(users)
	if len(users) == 0 {
		return result
	}

	for _, user := range users {
		if ctx.Err() != nil {
			break
		}
		inserted, err := w.producer.EnqueueBalanceLow(ctx, BalanceLowEvent{
			UserID:     user.ID,
			Balance:    user.Balance,
			Threshold:  threshold,
			DetectedAt: now,
			SiteDay:    day,
		})
		if err != nil {
			// 单个用户失败不阻断其余用户；下一轮会重试，dedupe_key 保证
			// 已成功的用户不会重复投递。
			result.Failed++
			w.log.Warn("balance low worker: enqueue failed",
				"user_id", user.ID, "error", err)
			continue
		}
		if inserted {
			result.Enqueued++
		} else {
			result.Deduped++
		}
	}
	w.log.Info("balance low scan completed",
		"scanned", result.Scanned,
		"enqueued", result.Enqueued,
		"deduped", result.Deduped,
		"failed", result.Failed,
		"threshold", threshold,
		"day", day,
	)
	return result
}

// settings 读取开关与阈值。JSON 解码失败返回错误（fail-closed），而不是
// 回退默认值——一个被写坏的行不应悄悄打开扫描。
func (w *BalanceLowWorker) settings() (threshold int64, enabled bool, err error) {
	if raw, ok := w.cfg.Get(KeyWebhookEnabled); ok {
		if err := json.Unmarshal(raw, &enabled); err != nil {
			return 0, false, fmt.Errorf("notify: webhook enabled is not a boolean: %w", err)
		}
	}
	if raw, ok := w.cfg.Get(KeyWebhookBalanceLowThreshold); ok {
		if err := json.Unmarshal(raw, &threshold); err != nil {
			return 0, false, fmt.Errorf("notify: balance low threshold is not an integer: %w", err)
		}
		if threshold < 0 {
			return 0, false, fmt.Errorf("notify: balance low threshold is negative: %d", threshold)
		}
	}
	return threshold, enabled, nil
}

// location 返回 dedupe_key 的「天」边界所用时区。
//
// 优先级：显式注入（测试）→ site.timezone → UTC。配置缺失/非法时回退 UTC
// 并记 Warn：扫描本身仍然可用，且 UTC 是跨副本一致、可解释的边界。
func (w *BalanceLowWorker) siteLocation() *time.Location {
	if w.location != nil {
		return w.location
	}
	raw, ok := w.cfg.Get(KeySiteTimezone)
	if !ok {
		return time.UTC
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		w.log.Warn("balance low worker: invalid site timezone; falling back to UTC")
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		w.log.Warn("balance low worker: unknown site timezone; falling back to UTC", "timezone", name)
		return time.UTC
	}
	return loc
}
