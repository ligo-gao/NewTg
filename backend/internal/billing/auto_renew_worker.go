package billing

// S4 自动续费 worker 与 Stripe off-session 扣款。
//
// 【P-17：同一线程、同一锁命名空间】本 worker 不自己起 goroutine：它由
// subscriptionworker 的 B12 到期/重置循环在同一个 goroutine 里按同一 tick
// 调用（platform 装配把 RunPass 传给 subscriptionworker.Deps.AutoRenew）。
// 锁不是「全局扫描锁」，而是**每订阅**的 `subscriptions:renew:<id>`：与
// 手动续费共用订阅粒度的互斥语义，多副本同时扫描时同一实例只有一个扣费者。
//
// 【R-41：不得另写第二套续费 SQL】扣款前用 createRenewalOrder（与手动续费
// 共享的幂等下单段）落 renew 订单与冻结快照；成功后调用
// settleSubscriptionRenewal，最终仍由 store.RenewSubscription 推进 end_at
// 并写 renewed 事件。自动续费只新增「渠道扣款」这一层，续费 SQL 只有一份。
//
// 【P-14：只支持 Stripe】支付令牌是 Stripe PaymentMethod id，经 SecretSealer
// 封存。扣款请求带 off_session=true + confirm=true；任何需要用户交互的状态
// （requires_action）都按失败处理并通知用户换卡，绝不在后台重试 SCA。
//
// 【为什么先写 attempted_at 再扣款】进程在「渠道已扣款、结算未提交」之间
// 崩溃时，下一轮会因退避等待重试；重试使用同一订单幂等键（Stripe
// Idempotency-Key 与 orders.idempotency_key 同值），Stripe 返回**同一个**
// PaymentIntent，不会二次扣款。这是 at-most-once 的资金路径。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/store"
)

const (
	// ConfigAutoRenewLeadHours 是续费窗口设置键（R-40）：now >= end_at - lead
	// 时进入可续费窗口。取值 1–168，缺键默认 24。
	ConfigAutoRenewLeadHours = "billing.auto_renew_lead_hours"
	// DefaultAutoRenewLeadHours 是冻结默认窗口（24 小时，R-40）。
	DefaultAutoRenewLeadHours = 24
	// MinAutoRenewLeadHours / MaxAutoRenewLeadHours 是配置合法区间（R-40）。
	MinAutoRenewLeadHours = 1
	MaxAutoRenewLeadHours = 168

	// DefaultAutoRenewRetryInterval 是两次扣费尝试之间的最小间隔。
	//
	// P-16 只规定「连续 3 次失败后关闭」，没有规定重试节奏。默认 6 小时让
	// 3 次尝试铺满 24 小时的窗口（而不是每分钟扫描的 3 分钟），用户有时间
	// 换卡；测试通过 Deps.RetryInterval 注入短间隔。
	DefaultAutoRenewRetryInterval = 6 * time.Hour

	// autoRenewLockTTL 覆盖一次渠道调用（HTTP client 默认 10s 超时）与结算
	// 事务，且远小于重试间隔；崩溃后最多 5 分钟即可被其它副本接管。
	autoRenewLockTTL = 5 * time.Minute
	// AutoRenewLockPrefix 是 P-17 冻结的锁命名空间。
	AutoRenewLockPrefix = "subscriptions:renew:"
)

// PaymentTokenSealer 是 SecretSealer 的最小方法集（生产实现为 credential
// SecretSealer，AES-256-GCM；billing 不直接依赖 httpapi/configcenter）。
type PaymentTokenSealer interface {
	Seal(plaintext string) (string, error)
	Open(sealed string) (string, error)
}

// AutoRenewSubscriptionStore 是 worker 需要的订阅存储接缝
// （*store.SQLSubscriptionRepo 是生产实现）。
type AutoRenewSubscriptionStore interface {
	ListDueForAutoRenew(ctx context.Context, now time.Time, lead, retry time.Duration,
		limit int) ([]store.Subscription, error)
	GetSubscription(ctx context.Context, id int64) (store.Subscription, error)
	MarkAutoRenewAttempt(ctx context.Context, id int64, now time.Time) (bool, error)
	RecordAutoRenewFailure(ctx context.Context, id int64, now time.Time,
		threshold int, detail map[string]any) (store.AutoRenewFailureState, error)
	RecordAutoRenewSuccess(ctx context.Context, id int64, now time.Time) error
	DisableAutoRenew(ctx context.Context, id int64, detail map[string]any) error
}

// AutoRenewPaymentMethods 是令牌读取接缝（*store.SQLPaymentMethodRepo）。
type AutoRenewPaymentMethods interface {
	GetForUser(ctx context.Context, userID int64, id string) (store.PaymentMethod, error)
	MarkInvalid(ctx context.Context, userID int64, id string) error
}

// AutoRenewChargeInput 是一次自动续费扣款请求。
type AutoRenewChargeInput struct {
	UserID          int64
	SubscriptionID  int64
	PaymentMethodID string
	// IdempotencyKey 同时用于 orders.idempotency_key 与 Stripe
	// Idempotency-Key 头：同一键的重试只会得到一个订单/一个 PaymentIntent。
	IdempotencyKey string
}

// AutoRenewChargeResult 是扣款/即时履约后的订单信息（日志用）。
type AutoRenewChargeResult struct {
	OrderID  int64
	Amount   int64
	Currency string
	// Immediate 表示 0 元套餐已在同一事务内完成续费，没有调用渠道。
	Immediate bool
	// Replayed 表示命中既有订单（上次已成功但结算后续步骤失败）。
	Replayed bool
}

// AutoRenewCharger 是渠道扣款接缝；生产实现为 *TopupService.ChargeAutoRenewal。
type AutoRenewCharger interface {
	ChargeAutoRenewal(ctx context.Context, in AutoRenewChargeInput) (AutoRenewChargeResult, error)
}

// AutoRenewChargeFailure 是扣款失败的受控分类。
//
// Code 只使用固定词汇（card_declined / authentication_required /
// provider_unavailable / plan_unavailable / …），绝不携带渠道原始 message：
// 渠道错误里可能回显卡信息，把它写进事件/通知会扩大敏感面。
type AutoRenewChargeFailure struct {
	Code string
	// Retryable 只影响日志措辞；无论是否可重试都计入连续失败（P-16）。
	Retryable bool
}

func (e *AutoRenewChargeFailure) Error() string {
	return "billing: auto-renew charge failed: " + e.Code
}

// AutoRenewFailureReason 把任意错误折叠成受控失败码（事件/通知 detail 用）。
func AutoRenewFailureReason(err error) string {
	var failure *AutoRenewChargeFailure
	if errors.As(err, &failure) && failure.Code != "" {
		return failure.Code
	}
	switch {
	case errors.Is(err, ErrSubscriptionPlanUnavailable):
		return "plan_unavailable"
	case errors.Is(err, ErrSubscriptionPlanInvalid):
		return "plan_invalid"
	case errors.Is(err, ErrRenewalSnapshot):
		return "snapshot_invalid"
	case errors.Is(err, ErrPaymentConfig):
		return "payment_config"
	case errors.Is(err, ErrProviderDisabled), errors.Is(err, ErrTopupDisabled):
		return "provider_disabled"
	case errors.Is(err, ErrRenewalSubscriptionNotFound), errors.Is(err, store.ErrSubscriptionNotFound):
		return "subscription_missing"
	case errors.Is(err, ErrSubscriptionNotRenewable), errors.Is(err, store.ErrSubscriptionNotActive):
		return "subscription_not_renewable"
	}
	return "internal"
}

// RenewalFailureNotice 是一次「自动续费失败」站内通知的输入。
type RenewalFailureNotice struct {
	UserID          int64
	SubscriptionID  int64
	PaymentMethodID string
	// Attempt 是本次失败后的连续失败次数（第 3 次时 Disabled=true）。
	Attempt  int32
	Disabled bool
	// Reason 是受控失败码（AutoRenewFailureReason）。
	Reason string
	// DedupeKey 由 worker 生成，保证同一次失败只通知一次。
	DedupeKey string
}

// RenewalFailureNotifier 是站内通知接缝。实现失败只记日志，不影响资金状态。
type RenewalFailureNotifier interface {
	NotifyRenewalFailure(ctx context.Context, notice RenewalFailureNotice) error
}

// AutoRenewDeps 是 worker 的装配输入。
type AutoRenewDeps struct {
	Subs    AutoRenewSubscriptionStore
	Methods AutoRenewPaymentMethods
	Charger AutoRenewCharger
	Sealer  PaymentTokenSealer
	// Config 读取 billing.auto_renew_lead_hours；nil 时恒用默认 24h。
	Config ConfigReader
	// Redis 提供每订阅的跨副本互斥（P-17）。
	Redis    *redis.Client
	Notifier RenewalFailureNotifier
	Logger   *slog.Logger
	Now      func() time.Time
	// LeadHours 覆盖配置（测试注入；0 表示读配置/默认值）。
	LeadHours int
	// RetryInterval 覆盖失败重试退避（测试注入；0 表示默认 6h）。
	RetryInterval time.Duration
}

// AutoRenewResult 是一轮扫描的实际效果。
type AutoRenewResult struct {
	Scanned int
	// Charged 是成功续费（含 0 元即时履约）的实例数。
	Charged int
	// Failed 是扣款失败（已计入连续失败）的实例数。
	Failed int
	// Disabled 是因失败阈值或令牌失效被关闭自动续费的实例数。
	Disabled int
	// Skipped 是因锁竞争/状态变化而放弃的候选数。
	Skipped int
}

// AutoRenewWorker 串行执行自动续费轮次，可并发安全（每订阅有 Redis 锁）。
type AutoRenewWorker struct {
	subs          AutoRenewSubscriptionStore
	methods       AutoRenewPaymentMethods
	charger       AutoRenewCharger
	sealer        PaymentTokenSealer
	cfg           ConfigReader
	redis         *redis.Client
	notifier      RenewalFailureNotifier
	log           *slog.Logger
	now           func() time.Time
	leadOverride  int
	retryInterval time.Duration
}

// NewAutoRenewWorker 构造 worker；依赖缺失是装配错误。
func NewAutoRenewWorker(deps AutoRenewDeps) (*AutoRenewWorker, error) {
	if deps.Subs == nil {
		return nil, errors.New("billing: auto-renew requires a subscription store")
	}
	if deps.Methods == nil {
		return nil, errors.New("billing: auto-renew requires a payment method store")
	}
	if deps.Charger == nil {
		return nil, errors.New("billing: auto-renew requires a charger")
	}
	if deps.Sealer == nil {
		return nil, errors.New("billing: auto-renew requires a payment token sealer")
	}
	if deps.Redis == nil {
		return nil, errors.New("billing: auto-renew requires Redis for per-subscription locks")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	retry := deps.RetryInterval
	if retry <= 0 {
		retry = DefaultAutoRenewRetryInterval
	}
	return &AutoRenewWorker{
		subs: deps.Subs, methods: deps.Methods, charger: deps.Charger,
		sealer: deps.Sealer, cfg: deps.Config, redis: deps.Redis,
		notifier: deps.Notifier, log: log, now: now,
		leadOverride: deps.LeadHours, retryInterval: retry,
	}, nil
}

// RunPass 是 subscriptionworker 调用的入口（同一 goroutine，P-17）。结果
// 只记日志：续费失败的明细已经落在事件流/通知里，worker 循环不因单实例
// 失败而中断。
func (w *AutoRenewWorker) RunPass(ctx context.Context) error {
	res, err := w.RunOnce(ctx)
	if err != nil && ctx.Err() == nil {
		return err
	}
	if res.Charged > 0 || res.Failed > 0 || res.Disabled > 0 {
		w.log.Info("subscriptions auto-renewed",
			"scanned", res.Scanned, "charged", res.Charged,
			"failed", res.Failed, "disabled", res.Disabled, "skipped", res.Skipped)
	}
	return err
}

// RunOnce 执行一轮：扫描窗口内实例，逐个抢订阅锁、复核状态并扣款。
func (w *AutoRenewWorker) RunOnce(ctx context.Context) (AutoRenewResult, error) {
	var res AutoRenewResult
	if ctx.Err() != nil {
		return res, nil
	}
	lead := w.leadDuration()
	now := w.now().UTC()
	due, err := w.subs.ListDueForAutoRenew(ctx, now, lead, w.retryInterval,
		store.AutoRenewScanLimit)
	if err != nil {
		return res, err
	}
	res.Scanned = len(due)
	for _, candidate := range due {
		if ctx.Err() != nil {
			break
		}
		w.renewOne(ctx, candidate, now, lead, &res)
	}
	return res, nil
}

// renewOne 处理一个候选实例。任何失败都只记录并计入统计，不返回错误：
// 一个用户的卡失效不应阻断其他用户的续费。
func (w *AutoRenewWorker) renewOne(ctx context.Context, candidate store.Subscription,
	now time.Time, lead time.Duration, res *AutoRenewResult) {

	token, acquired, err := w.acquireLock(ctx, AutoRenewLockPrefix+strconv.FormatInt(candidate.ID, 10))
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("auto-renew lock acquire failed", "subscription_id", candidate.ID, "error", err)
		}
		res.Skipped++
		return
	}
	if !acquired {
		// 另一副本/手动续费路径正在处理同一实例：本轮跳过是预期路径。
		res.Skipped++
		return
	}
	defer w.releaseLock(AutoRenewLockPrefix+strconv.FormatInt(candidate.ID, 10), token)

	// 抢锁后复核：扫描到抢锁之间用户可能已关闭自动续费/取消周期末续费，
	// 或手动续费已把 end_at 推进出窗口。以最新行为准，绝不按旧快照扣费。
	sub, err := w.subs.GetSubscription(ctx, candidate.ID)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("auto-renew reload subscription failed", "subscription_id", candidate.ID, "error", err)
		}
		res.Skipped++
		return
	}
	if !autoRenewDue(sub, now, lead) {
		res.Skipped++
		return
	}
	methodID := ""
	if sub.AutoRenewPaymentMethodID != nil {
		methodID = *sub.AutoRenewPaymentMethodID
	}
	method, err := w.methods.GetForUser(ctx, sub.UserID, methodID)
	if err != nil {
		// R-42：令牌已删除（或不属于该用户）→ fail-closed，不再尝试扣费。
		reason := "payment_method_missing"
		if !errors.Is(err, store.ErrPaymentMethodNotFound) {
			w.log.Warn("auto-renew load payment method failed",
				"subscription_id", sub.ID, "error", err)
			reason = "payment_method_unavailable"
		}
		w.failClosed(ctx, sub, methodID, reason, res)
		return
	}
	if method.Status != "active" {
		w.failClosed(ctx, sub, methodID, "payment_method_invalid", res)
		return
	}
	plaintext, err := w.sealer.Open(method.TokenCipher)
	if err != nil || strings.TrimSpace(plaintext) == "" {
		// 解密失败不能回退到「把密文当令牌」：标记失效并 fail-closed。
		w.log.Warn("auto-renew payment token cannot be opened",
			"subscription_id", sub.ID, "payment_method_id", methodID)
		if markErr := w.methods.MarkInvalid(ctx, sub.UserID, methodID); markErr != nil &&
			!errors.Is(markErr, store.ErrPaymentMethodNotFound) {
			w.log.Warn("auto-renew mark payment method invalid failed",
				"payment_method_id", methodID, "error", markErr)
		}
		w.failClosed(ctx, sub, methodID, "payment_method_unreadable", res)
		return
	}

	// 先记录尝试时刻再扣款：崩溃重试会走退避，且同一幂等键不会二次扣款。
	ok, err := w.subs.MarkAutoRenewAttempt(ctx, sub.ID, now)
	if err != nil {
		w.log.Warn("auto-renew mark attempt failed", "subscription_id", sub.ID, "error", err)
		res.Skipped++
		return
	}
	if !ok {
		// 状态在复核后又变了（例如用户刚关闭开关）。
		res.Skipped++
		return
	}

	result, chargeErr := w.charger.ChargeAutoRenewal(ctx, AutoRenewChargeInput{
		UserID:          sub.UserID,
		SubscriptionID:  sub.ID,
		PaymentMethodID: strings.TrimSpace(plaintext),
		// 幂等键按「订阅 + 当前周期末 + 第几次尝试」冻结：同一周期内的重试
		// 复用同一订单；真正失败后下一轮是新的尝试号，允许重新建单。
		IdempotencyKey: autoRenewIdempotencyKey(sub.ID, sub.EndAt, sub.AutoRenewFailures),
	})
	if chargeErr != nil {
		reason := AutoRenewFailureReason(chargeErr)
		state, ferr := w.subs.RecordAutoRenewFailure(ctx, sub.ID, now,
			store.AutoRenewFailureThreshold, map[string]any{
				"reason":            reason,
				"payment_method_id": methodID,
			})
		if ferr != nil {
			w.log.Error("auto-renew record failure failed",
				"subscription_id", sub.ID, "error", ferr)
			res.Skipped++
			return
		}
		res.Failed++
		if state.Disabled {
			res.Disabled++
		}
		w.notify(ctx, sub, methodID, state, reason)
		w.log.Warn("auto-renew charge failed",
			"subscription_id", sub.ID, "reason", reason,
			"failures", state.Failures, "auto_renew_disabled", state.Disabled)
		return
	}

	if err := w.subs.RecordAutoRenewSuccess(ctx, sub.ID, now); err != nil {
		// 续费已经成功提交；计数清零失败只影响下一轮的通知噪声，不回滚资金。
		w.log.Warn("auto-renew reset failure counter failed",
			"subscription_id", sub.ID, "error", err)
	}
	res.Charged++
	w.log.Info("subscription auto-renewed",
		"subscription_id", sub.ID, "order_id", result.OrderID,
		"amount", result.Amount, "currency", result.Currency,
		"immediate", result.Immediate, "replayed", result.Replayed)
}

// autoRenewDue 是抢锁后的复核条件（与 SQL 扫描条件保持一致）。
func autoRenewDue(sub store.Subscription, now time.Time, lead time.Duration) bool {
	return sub.Status == "active" && sub.AutoRenew &&
		!sub.CancelAtPeriodEnd && sub.AutoRenewPaymentMethodID != nil &&
		!sub.EndAt.After(now.Add(lead))
}

// autoRenewIdempotencyKey 生成「订阅 + 周期末 + 尝试号」幂等键。
//
// attempt 取当前连续失败数：首次尝试为 1，第 N 次重试为 N。成功路径不会
// 再用到该周期（end_at 已推进）；失败重试（同一 attempt）复用同一订单，
// 真正失败后的下一轮是新 attempt，才能重新建单。
func autoRenewIdempotencyKey(subscriptionID int64, endAt time.Time, failures int32) string {
	return fmt.Sprintf("auto-renew:%d:%d:%d", subscriptionID, endAt.UTC().Unix(), failures+1)
}

// failClosed 关闭自动续费、写 renewal_failed 事件并通知（R-42）。
func (w *AutoRenewWorker) failClosed(ctx context.Context, sub store.Subscription,
	methodID, reason string, res *AutoRenewResult) {

	detail := map[string]any{"reason": reason}
	if methodID != "" {
		detail["payment_method_id"] = methodID
	}
	if err := w.subs.DisableAutoRenew(ctx, sub.ID, detail); err != nil {
		w.log.Error("auto-renew fail-closed disable failed",
			"subscription_id", sub.ID, "reason", reason, "error", err)
		res.Skipped++
		return
	}
	res.Disabled++
	w.notify(ctx, sub, methodID, store.AutoRenewFailureState{
		Failures: sub.AutoRenewFailures, Disabled: true,
	}, reason)
	w.log.Warn("auto-renew disabled (fail-closed)",
		"subscription_id", sub.ID, "reason", reason)
}

// notify 发一条站内通知（best-effort：失败只记日志）。
func (w *AutoRenewWorker) notify(ctx context.Context, sub store.Subscription,
	methodID string, state store.AutoRenewFailureState, reason string) {

	if w.notifier == nil {
		return
	}
	notice := RenewalFailureNotice{
		UserID:          sub.UserID,
		SubscriptionID:  sub.ID,
		PaymentMethodID: methodID,
		Attempt:         state.Failures,
		Disabled:        state.Disabled,
		Reason:          reason,
		// 同一次失败只通知一次：重试是新的尝试号，用户会看到每次失败；
		// 多副本被锁保护，dedupe 只是重叠扫描的兜底。
		DedupeKey: fmt.Sprintf("renewal_failed:%d:%d:%d",
			sub.ID, sub.EndAt.UTC().Unix(), state.Failures+1),
	}
	if err := w.notifier.NotifyRenewalFailure(ctx, notice); err != nil && ctx.Err() == nil {
		w.log.Warn("auto-renew notification failed",
			"subscription_id", sub.ID, "error", err)
	}
}

// leadDuration 读取 billing.auto_renew_lead_hours（1–168，默认 24）。
//
// 非法值回退默认并告警而不是让整轮失败：配置错误不应让所有用户的自动
// 续费停摆（那比「提前/推迟一天扣费」更严重的业务后果）。
func (w *AutoRenewWorker) leadDuration() time.Duration {
	hours := w.leadOverride
	if hours <= 0 {
		hours = DefaultAutoRenewLeadHours
		if w.cfg != nil {
			if raw, ok := w.cfg.Get(ConfigAutoRenewLeadHours); ok {
				parsed, err := parseConfigInt(raw)
				if err == nil && parsed >= MinAutoRenewLeadHours && parsed <= MaxAutoRenewLeadHours {
					hours = parsed
				} else {
					w.log.Warn("auto-renew lead hours is invalid; using default",
						"value", strings.TrimSpace(string(raw)), "default", DefaultAutoRenewLeadHours)
				}
			}
		}
	}
	return time.Duration(hours) * time.Hour
}

// parseConfigInt 接受 JSON number 或数字字符串（配置中心两种形态都合法）。
func parseConfigInt(raw json.RawMessage) (int, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0, errors.New("empty config value")
	}
	if strings.HasPrefix(text, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, err
		}
		text = strings.TrimSpace(str)
	}
	return strconv.Atoi(text)
}

// acquireLock / releaseLock 与 subscriptionworker 同一范式（SET NX PX +
// compare-and-delete），但键空间是 P-17 冻结的每订阅锁。
func (w *AutoRenewWorker) acquireLock(ctx context.Context, key string) (string, bool, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("billing: generate auto-renew lock token: %w", err)
	}
	token := hex.EncodeToString(buf)
	acquired, err := w.redis.SetNX(ctx, key, token, autoRenewLockTTL).Result()
	if err != nil {
		return "", false, fmt.Errorf("billing: acquire auto-renew lock %s: %w", key, err)
	}
	return token, acquired, nil
}

func (w *AutoRenewWorker) releaseLock(key, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := autoRenewReleaseScript.Run(ctx, w.redis, []string{key}, token).Err(); err != nil {
		w.log.Warn("auto-renew release lock failed (TTL will reclaim it)",
			"key", key, "error", err)
	}
}

var autoRenewReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

// ---------------------------------------------------------------------------
// Stripe off-session 扣款（TopupService 实现）
// ---------------------------------------------------------------------------

// stripeChargeResponse 只解析扣款判定与受控错误码；不保留渠道 message。
type stripeChargeResponse struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	Amount           int64  `json:"amount"`
	Currency         string `json:"currency"`
	LastPaymentError *struct {
		Code        string `json:"code"`
		DeclineCode string `json:"decline_code"`
	} `json:"last_payment_error"`
}

// ChargeAutoRenewal 用已存令牌做一次 off-session 扣款，并在成功时走与手动
// 续费**相同**的结算事务（settleSubscriptionRenewal → store.RenewSubscription）。
//
// 失败分类：
//   - 渠道明确拒绝（4xx / requires_payment_method / requires_action）→ 订单
//     置 failed，返回不可重试的 AutoRenewChargeFailure；
//   - 网络/5xx → 订单保持 pending，返回 provider_unavailable；下一轮用同一
//     幂等键重试（Stripe 返回同一 PaymentIntent）。
func (s *TopupService) ChargeAutoRenewal(ctx context.Context,
	in AutoRenewChargeInput) (AutoRenewChargeResult, error) {

	var zero AutoRenewChargeResult
	order, immediate, replayed, err := s.createRenewalOrder(ctx, in.UserID,
		in.SubscriptionID, ProviderStripe, in.IdempotencyKey)
	if err != nil {
		return zero, err
	}
	result := AutoRenewChargeResult{
		OrderID: order.ID, Amount: order.Amount, Currency: order.Currency,
		Immediate: immediate, Replayed: replayed,
	}
	if immediate {
		// 0 元套餐已在 createRenewalOrder 的事务内续费，无渠道动作。
		return result, nil
	}
	if order.Status == string(OrderSucceeded) {
		// 上次扣款已成功、结算后的后续步骤失败；订单即事实，直接视为成功。
		return result, nil
	}
	if order.Status != string(OrderPending) {
		return zero, &AutoRenewChargeFailure{Code: "order_not_pending"}
	}

	body, err := s.chargeStripeOffSession(ctx, order, strings.TrimSpace(in.PaymentMethodID), in.IdempotencyKey)
	if err != nil {
		var failure *AutoRenewChargeFailure
		if errors.As(err, &failure) {
			if !failure.Retryable {
				s.markOrderFailed(ctx, order.ID, "provider_rejected")
			}
			return zero, err
		}
		// 未知错误按可重试处理：不把订单置 failed，避免一次网络抖动
		// 让用户白白损失一次重试机会。
		return zero, &AutoRenewChargeFailure{Code: AutoRenewFailureReason(err), Retryable: true}
	}
	if body.Status != "succeeded" {
		s.markOrderFailed(ctx, order.ID, "provider_rejected")
		code := "payment_required"
		if body.LastPaymentError != nil && body.LastPaymentError.Code != "" {
			code = body.LastPaymentError.Code
		}
		if body.Status == "requires_action" {
			// off-session 无法完成 SCA；必须由用户重新授权。
			code = "authentication_required"
		}
		return zero, &AutoRenewChargeFailure{Code: code}
	}
	// 金额/币种以渠道返回为准交给结算校验：不一致会被 failMismatch 置 failed
	// 并返回错误，绝不会按错误金额续费。
	if err := s.settleSubscriptionRenewal(ctx, order, body.Amount,
		strings.ToUpper(strings.TrimSpace(body.Currency)), body.raw); err != nil {
		return zero, &AutoRenewChargeFailure{Code: "settlement_failed", Retryable: true}
	}
	return result, nil
}

// stripeChargeBody 把响应体与解析结果一起返回（raw 用于 orders.raw_callback）。
type stripeChargeBody struct {
	ID               string
	Status           string
	Amount           int64
	Currency         string
	LastPaymentError *struct {
		Code        string `json:"code"`
		DeclineCode string `json:"decline_code"`
	}
	raw []byte
}

// chargeStripeOffSession 用 `confirm=true&off_session=true` 创建并确认
// PaymentIntent（P-14 / P-7：标准库实现，不引入 SDK）。
//
// 不发送任何卡字段：令牌是 Stripe 侧的 PaymentMethod id，网关只做引用。
func (s *TopupService) chargeStripeOffSession(ctx context.Context, order *store.Order,
	paymentMethodID, idempotencyKey string) (*stripeChargeBody, error) {

	if paymentMethodID == "" {
		return nil, &AutoRenewChargeFailure{Code: "payment_method_invalid"}
	}
	secret, err := s.configSecret(ConfigStripeSecretKey)
	if err != nil {
		return nil, &AutoRenewChargeFailure{Code: "payment_config"}
	}
	if secret == "" {
		return nil, &AutoRenewChargeFailure{Code: "payment_config"}
	}
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(order.Amount, 10))
	form.Set("currency", strings.ToLower(order.Currency))
	form.Set("payment_method", paymentMethodID)
	form.Set("payment_method_types[0]", "card")
	form.Set("confirm", "true")
	form.Set("off_session", "true")
	form.Set("metadata[order_id]", strconv.FormatInt(order.ID, 10))
	form.Set("metadata[user_id]", strconv.FormatInt(order.UserID, 10))
	form.Set("metadata[auto_renew]", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.stripeBaseURL+"/v1/payment_intents", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &AutoRenewChargeFailure{Code: "provider_unavailable", Retryable: true}
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, &AutoRenewChargeFailure{Code: "provider_unavailable", Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &AutoRenewChargeFailure{Code: "provider_unavailable", Retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, &AutoRenewChargeFailure{Code: "provider_unavailable", Retryable: true}
		}
		return nil, &AutoRenewChargeFailure{Code: "payment_method_rejected"}
	}
	var parsed stripeChargeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.ID == "" {
		return nil, &AutoRenewChargeFailure{Code: "provider_unavailable", Retryable: true}
	}
	return &stripeChargeBody{
		ID: parsed.ID, Status: parsed.Status, Amount: parsed.Amount,
		Currency: parsed.Currency, LastPaymentError: parsed.LastPaymentError,
		raw: raw,
	}, nil
}
