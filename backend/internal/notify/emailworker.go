package notify

// W10-EM3/EM4/EM5：email 投递 worker。
//
// 设计权威：modules-08 §3.9.6「（b）发送、重试与幂等」「（d）限流」，
// billing-06 N8 的重试范式（webhookworker.go），R-W10-06 冻结口径。
//
// 冻结语义：
//   - 领取 channel='email' AND status='queued' AND 到期，FOR UPDATE SKIP
//     LOCKED；与 webhook 完全相同的占用/提交协议（store.claimDelivery）；
//   - 渲染失败分两类：**本地数据问题**（模板缺失/停用、必填变量缺失、
//     payload 损坏）→ 终态 failed，不重试（重试不会让数据变好）；**传输
//     故障**（SMTP 拒绝/超时）→ attempt+1，1/5/25 分钟后重试，第 4 次失败
//     → failed；
//   - 发送前复核偏好（非事务性且有偏好槽位的类别）：opt-out →
//     suppressed，不消耗重试次数；事务性/管理员侧类别恒发；
//   - 全局 SMTP 令牌桶：**10 条/秒**（常量，不加设置键）。设计称「worker
//     级令牌桶」；本系统按设计单副本消费（行锁 + 单 worker 角色），因此
//     进程内桶即全局桶；
//   - SMTP 未配置：worker 不领取任何行（行保持 queued、attempt 不变），
//     而不是把行标 failed——配置修复后原样发出，符合「不得静默丢」；
//   - 停机：ctx 取消后循环立即返回；进行中的 Send 使用同一 ctx，取消会使
//     发送失败并回滚占用（行保持 queued），不留下「发送中」悬挂状态。
//
// 【日志脱敏】只记录投递 id、类别、尝试次数与固定分类文案；绝不记录收件
// 地址、主题、正文、SMTP 凭据或底层错误原文（SMTP 错误可能含服务器回显）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultEmailWorkerInterval 是扫描周期（1 分钟）。重试间隔 1/5/25
	// 分钟都是它的整数倍，最坏情况只会晚一个周期发出。
	DefaultEmailWorkerInterval = time.Minute
	// DefaultEmailBatchSize 是单轮最多处理的投递数，约束单轮总时长。
	DefaultEmailBatchSize = 50

	// EmailRateLimitPerSecond 是 R-W10-06 冻结的全局 SMTP 吞吐上限。
	// 常量而非设置键：限流是 SMTP 提供方的保护措施，调参需要重新评估
	// 提供方配额，不应由管理员在运行期随手改动。
	EmailRateLimitPerSecond = 10
	// emailRateBurst 是桶容量：允许瞬时 10 封，之后严格按 10/s 排队。
	emailRateBurst = 10

	// emailMaxAttempts 是冻结的尝试总数：初投 + 3 次重试（1/5/25）。
	emailMaxAttempts = 4
)

// EmailWorkerSender 是 worker 需要的发送能力。生产实现是 *SMTPSender；
// 测试注入 fake。Configured 必须在每轮扫描前查询：SMTP 未配置时**不领取**，
// 避免每轮为同一批行消耗一次 attempt 并最终误标 failed。
type EmailWorkerSender interface {
	Configured(ctx context.Context) bool
	Send(ctx context.Context, to, subject, body string) (messageID string, err error)
}

// EmailWorkerDeps 是 worker 的运行依赖与可调参数（测试注入短周期/时钟）。
type EmailWorkerDeps struct {
	// Store 是投递记录的存储（必填）。
	Store *Store
	// Sender 是 SMTP 出口（必填）。
	Sender EmailWorkerSender
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Now 为空时使用 time.Now；生产与测试应看到同一个「现在」。
	Now func() time.Time
	// Interval 覆盖扫描周期（测试注入短周期）。
	Interval time.Duration
	// BatchSize 覆盖单轮最大投递数。
	BatchSize int
}

// EmailWorkerResult 是一轮扫描的实际效果。
type EmailWorkerResult struct {
	Claimed    int
	Sent       int
	Failed     int
	Suppressed int
}

// EmailWorker 可并发使用（每次 RunOnce 之间由调用方串行；桶自身加锁）。
type EmailWorker struct {
	store     *Store
	sender    EmailWorkerSender
	log       *slog.Logger
	now       func() time.Time
	interval  time.Duration
	batchSize int
	bucket    *emailTokenBucket
}

// NewEmailWorker 构造 worker；依赖缺失是装配错误，必须在启动期暴露。
func NewEmailWorker(deps EmailWorkerDeps) (*EmailWorker, error) {
	if deps.Store == nil {
		return nil, errors.New("notify: email worker requires a store")
	}
	if deps.Sender == nil {
		return nil, errors.New("notify: email worker requires a sender")
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
		interval = DefaultEmailWorkerInterval
	}
	batchSize := deps.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultEmailBatchSize
	}
	return &EmailWorker{
		store: deps.Store, sender: deps.Sender, log: log,
		now: now, interval: interval, batchSize: batchSize,
		bucket: newEmailTokenBucket(EmailRateLimitPerSecond, emailRateBurst),
	}, nil
}

// RunEmailWorkerLoop 阻塞运行 email 循环直到 ctx 取消。
//
// 启动即扫一轮：重启后不必等完整周期才继续处理积压；崩溃时被回滚的占用
// 也会在首轮被重新领取。
func RunEmailWorkerLoop(ctx context.Context, deps EmailWorkerDeps) error {
	worker, err := NewEmailWorker(deps)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		// 已取消的上下文不再启动任何 DB/网络负载（停机竞态）。
		return nil
	}
	worker.log.Info("email worker started",
		"interval", worker.interval,
		"batch_size", worker.batchSize,
		"rate_limit_per_second", EmailRateLimitPerSecond,
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

// RunOnce 执行一轮扫描并返回计数。单轮内的 DB/网络故障只记录，不向上
// 抛错：下一轮会重试，把「扫描失败」伪装成「没有待发邮件」才是事故。
func (w *EmailWorker) RunOnce(ctx context.Context) EmailWorkerResult {
	var result EmailWorkerResult
	if ctx.Err() != nil {
		return result
	}
	// SMTP 未配置时不领取：行保持 queued、attempt 不变。设计允许「worker
	// 不启动」作为未配置行为；这里选择比「标 failed」更强的可恢复语义——
	// 管理员保存 SMTP 后，积压邮件会原样发出，而不是需要人工重放。
	if !w.sender.Configured(ctx) {
		w.log.Debug("email worker skipped: SMTP is not configured")
		return result
	}

	for i := 0; i < w.batchSize; i++ {
		if ctx.Err() != nil {
			break
		}
		claim, ok, err := w.store.ClaimEmailDelivery(ctx, w.now().UTC())
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("email worker: claim failed", "error", err)
			}
			return result
		}
		if !ok {
			break
		}
		result.Claimed++
		switch w.deliver(ctx, claim) {
		case emailOutcomeSent:
			result.Sent++
		case emailOutcomeSuppressed:
			result.Suppressed++
		case emailOutcomeFailed:
			result.Failed++
		case emailOutcomeReleased:
			// SMTP 在投递中途变为未配置/停机：行保持 queued，不计数，
			// 本轮停止（继续只会重复同样的失败）。
			return result
		case emailOutcomeAborted:
			// 状态写不进去：占用已回滚，行仍是 queued。若继续本轮，claim
			// 会立刻再次领取同一行并重复发送，因此必须中止到下一周期。
			return result
		}
	}
	if result.Claimed > 0 {
		w.log.Info("email deliveries processed",
			"claimed", result.Claimed,
			"sent", result.Sent,
			"failed", result.Failed,
			"suppressed", result.Suppressed,
		)
	}
	return result
}

type emailOutcome int

const (
	emailOutcomeFailed emailOutcome = iota
	emailOutcomeSent
	emailOutcomeSuppressed
	emailOutcomeReleased
	// emailOutcomeAborted：投递状态未能落库（Mark* 失败）。占用已回滚，
	// 行保持 queued；本轮必须停止，否则同一行会在同一轮被再次领取/发送。
	emailOutcomeAborted
)

// deliver 投递一条已占用的 email。无论结果如何都必须关闭占用。
func (w *EmailWorker) deliver(ctx context.Context, claim *EmailDeliveryClaim) emailOutcome {
	delivery := claim.Delivery

	allowed, suppressReason, err := w.emailAllowedAtSend(ctx, delivery)
	if err != nil {
		// 偏好读取失败：不能凭空发送（可能已退订），也不能吞掉邮件。
		// 释放占用、保持 queued，下一轮重试读取；本轮停止。
		if ctx.Err() == nil {
			w.log.Warn("email worker: preference check failed; releasing",
				"id", delivery.ID, "error", err)
		}
		claim.Release()
		return emailOutcomeReleased
	}
	if !allowed {
		if err := claim.MarkSuppressed(ctx, suppressReason); err != nil {
			w.log.Error("email worker: mark delivery suppressed", "id", delivery.ID, "error", err)
			return emailOutcomeAborted
		}
		return emailOutcomeSuppressed
	}

	rendered, terminalReason, retryable := w.renderDelivery(ctx, delivery)
	if terminalReason != "" {
		if err := claim.MarkFailed(ctx, delivery.Attempt, nil, terminalReason); err != nil {
			w.log.Error("email worker: mark undeliverable delivery failed", "id", delivery.ID, "error", err)
			return emailOutcomeAborted
		}
		w.log.Warn("email delivery failed permanently",
			"id", delivery.ID, "category", delivery.Category, "reason", terminalReason)
		return emailOutcomeFailed
	}
	if !retryable {
		// 模板读取遇到 DB 故障：不是邮件本身的问题，不消耗 attempt。
		claim.Release()
		return emailOutcomeReleased
	}

	// 全局 10 条/秒：在真正 Send 之前取令牌。取消（停机）时释放占用，
	// 行保持 queued。
	if err := w.bucket.wait(ctx, w.now); err != nil {
		claim.Release()
		return emailOutcomeReleased
	}

	messageID, sendErr := w.sender.Send(ctx, delivery.Recipient, rendered.Subject, rendered.Body)
	switch {
	case sendErr == nil:
		if err := claim.MarkSentWithProvider(ctx, delivery.Attempt+1, messageID); err != nil {
			w.log.Error("email worker: mark delivery sent", "id", delivery.ID, "error", err)
			// 行已回滚到 queued；继续本轮会立刻重领并重复发送。
			return emailOutcomeAborted
		}
		return emailOutcomeSent
	case errors.Is(sendErr, ErrSMTPNotConfigured):
		// SMTP 在本轮中途被清空：与未配置态同语义，不消耗 attempt。
		claim.Release()
		return emailOutcomeReleased
	case ctx.Err() != nil:
		// 停机取消导致发送失败：不写 failed，行回到 queued 等重启。
		claim.Release()
		return emailOutcomeReleased
	default:
		attempt := delivery.Attempt + 1
		next := retryAtForAttempt(w.now().UTC(), attempt)
		// last_error 只写固定分类：SMTP 服务器回显可能包含邮箱、IP 或
		// 上游内部信息。
		if err := claim.MarkFailed(ctx, attempt, next, "smtp delivery failed"); err != nil {
			w.log.Error("email worker: mark delivery failed", "id", delivery.ID, "error", err)
			return emailOutcomeAborted
		}
		w.log.Warn("email delivery failed",
			"id", delivery.ID,
			"category", delivery.Category,
			"attempt", attempt,
			"will_retry", next != nil,
		)
		return emailOutcomeFailed
	}
}

// emailAllowedAtSend 是 R-W10-06 的发送前偏好复核。
//
// 只有「非事务性 + 有偏好槽位」的类别需要复核；事务性（auth/security/
// billing.payment_* 等）与管理员侧（ops.*）直接放行。投递行没有 user id
// 时无法复核，按放行处理（行是入队时的权威快照）。用户已删除 → 抑制，
// 避免向失效账号继续发信。
func (w *EmailWorker) emailAllowedAtSend(ctx context.Context, delivery Delivery) (bool, string, error) {
	if IsTransactionalEmailCategory(delivery.Category) || !HasEmailPreference(delivery.Category) {
		return true, "", nil
	}
	if delivery.RecipientUserID == nil {
		return true, "", nil
	}
	profile, err := w.store.LoadEmailProfile(ctx, *delivery.RecipientUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "recipient account is no longer available", nil
	}
	if err != nil {
		return false, "", err
	}
	if profile.PreferencesCorrupt {
		// 偏好损坏时 fail-closed：宁可漏发一封提醒，也不向已退订用户发信。
		return false, "notification preferences are corrupt", nil
	}
	if !PreferencesAllowEmail(profile.Preferences, delivery.Category) {
		return false, "recipient opted out of email", nil
	}
	return true, "", nil
}

// renderDelivery 渲染投递行。返回值语义：
//   - terminalReason != ""：终态 failed 的固定分类文案；
//   - retryable=false（且 terminalReason==""）：模板读取的 DB 故障，释放
//     占用稍后重试，不消耗 attempt。
func (w *EmailWorker) renderDelivery(ctx context.Context, delivery Delivery) (Rendered, string, bool) {
	key := delivery.Category
	if delivery.TemplateKey != nil && strings.TrimSpace(*delivery.TemplateKey) != "" {
		key = *delivery.TemplateKey
	}
	spec, ok := LookupTemplate(key, ChannelEmail)
	if !ok {
		return Rendered{}, "email template is not registered", true
	}
	locale := DefaultLocale
	if delivery.Locale != nil {
		locale = *delivery.Locale
	}
	content, resolved, row, retryable, terminal := w.resolveEmailTemplate(ctx, spec, locale)
	if terminal != "" {
		return Rendered{}, terminal, true
	}
	if !retryable {
		return Rendered{}, "", false
	}
	if !row.Enabled {
		// 管理员停用模板 → 不发送，终态 failed（设计 P4 的读取侧口径）。
		return Rendered{}, "email template is disabled", true
	}

	variables := map[string]any{}
	if len(delivery.Payload) > 0 {
		if err := json.Unmarshal(delivery.Payload, &variables); err != nil || variables == nil {
			return Rendered{}, "email payload is invalid", true
		}
	}
	if err := ValidateVariables(spec, variables); err != nil {
		var missing *ErrMissingVariable
		if errors.As(err, &missing) {
			// 设计冻结的 last_error 口径：missing_var:{name}。
			return Rendered{}, "missing_var:" + missing.Variable, true
		}
		return Rendered{}, "email template variables are invalid", true
	}
	rendered, err := RenderContent(spec, content, resolved, variables)
	if err != nil {
		var missing *ErrMissingVariable
		if errors.As(err, &missing) {
			return Rendered{}, "missing_var:" + missing.Variable, true
		}
		return Rendered{}, "email template is invalid", true
	}
	return rendered, "", true
}

// resolveEmailTemplate 实施语言回退链：精确行 → en 行 → 失败（P1）。
//
// 与 Service.resolve 的 allowBundle=false 分支同口径，但这里不使用内置
// bundle：发送路径必须只认库里的行，被删掉的行不能靠代码文案「看起来
// 还能发」。DB 故障与「行不存在」区分开：前者 retryable=false。
func (w *EmailWorker) resolveEmailTemplate(
	ctx context.Context, spec Template, locale Locale,
) (Content, Locale, TemplateRow, bool, string) {
	if content, row, err := w.lookupEmailTemplate(ctx, spec, locale); err == nil {
		return content, locale, row, true, ""
	} else if !errors.Is(err, sql.ErrNoRows) {
		if ctx.Err() == nil {
			w.log.Warn("email worker: template lookup failed", "error", err)
		}
		return Content{}, "", TemplateRow{}, false, ""
	}
	if locale != DefaultLocale {
		if content, row, err := w.lookupEmailTemplate(ctx, spec, DefaultLocale); err == nil {
			return content, DefaultLocale, row, true, ""
		} else if !errors.Is(err, sql.ErrNoRows) {
			if ctx.Err() == nil {
				w.log.Warn("email worker: template fallback lookup failed", "error", err)
			}
			return Content{}, "", TemplateRow{}, false, ""
		}
	}
	return Content{}, "", TemplateRow{}, true, "email template is missing for all locales"
}

func (w *EmailWorker) lookupEmailTemplate(ctx context.Context, spec Template, locale Locale) (Content, TemplateRow, error) {
	row, err := w.store.Get(ctx, spec.Key, ChannelEmail, locale)
	if err != nil {
		return Content{}, TemplateRow{}, err
	}
	return contentFromRow(row), row, nil
}

// emailTokenBucket 是进程级令牌桶（固定速率，burst 容量）。
//
// 用「下一次可用时刻」而不是逐次 sleep：多个投递在同一轮内排队时，等待
// 时间可累加且不会被重复计算；初始 next 前移 burst-1 个间隔，使前 10 个
// 请求立即通过（与令牌桶语义一致）。
type emailTokenBucket struct {
	mu       sync.Mutex
	interval time.Duration
	burst    int
	next     time.Time
	started  bool
}

func newEmailTokenBucket(perSecond int, burst int) *emailTokenBucket {
	if perSecond <= 0 {
		perSecond = EmailRateLimitPerSecond
	}
	if burst <= 0 {
		burst = emailRateBurst
	}
	interval := time.Duration(float64(time.Second) / float64(perSecond))
	// 避免极端参数下 interval=0 导致 next 永不前进。
	if interval <= 0 {
		interval = time.Nanosecond
	}
	return &emailTokenBucket{interval: interval, burst: burst}
}

// take 返回调用方需要等待的时长；0 表示可立即发送。
func (b *emailTokenBucket) take(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		// 第一次调用把「虚拟起点」平移到 now：next 回拨 burst-1 个间隔，
		// 使前 burst 次调用立即通过。
		b.next = now.Add(-time.Duration(b.burst-1) * b.interval)
		b.started = true
	}
	scheduled := b.next
	b.next = scheduled.Add(b.interval)
	if scheduled.Before(now) {
		scheduled = now
	}
	wait := scheduled.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return wait
}

// wait 取一个令牌，必要时阻塞到可用。ctx 取消时返回错误（停机路径）。
func (b *emailTokenBucket) wait(ctx context.Context, now func() time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := b.take(now())
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// 编译期钉死生产实现与接口一致。
var _ EmailWorkerSender = (*SMTPSender)(nil)
