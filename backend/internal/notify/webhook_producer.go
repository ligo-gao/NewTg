package notify

// B1b：Webhook 业务事件生产者（payment_success / user_signup）。
// B4：追加 balance_low（N6 低余额扫描的投递入口）。
//
// 设计权威：modules-08 F-08-20（出站 Webhook 配置与投递）与 §3.9.6b
// （至少一次 + 幂等键 = 有效一次）。N8 worker（webhookworker.go）只负责
// 领取与发送；本文件负责在业务副作用发生后把**投递行**写进队列。
//
// 冻结语义：
//   - category 是模板注册表键，worker 用 webhookEventByCategory 映射到
//     F-08-20 的订阅事件名（payment_success / user_signup）；
//   - 生产者先做订阅过滤（notify.webhook.events），未订阅**不写行**——
//     与 worker 的 suppressed 不同，队列里不留下永远不会发送的噪声；
//   - dedupe_key 是业务幂等锚：重复回调/重放命中 UNIQUE(dedupe_key) 后
//     不产生第二条投递；
//   - 写入失败是 best-effort：调用方只记 Warn，绝不回滚已经完成的
//     入账/注册事务（钱与账号是权威，通知是可补的副作用）。
//
// payload 归属：modules-08 F-08-20 明示事件 payload 结构属模块 01/03/04；
// 本批由 B1b 冻结形状并登记台账（G-DOM-59/60）待 owner 回写。payload 中
// 绝不包含口令、令牌、邮箱验证码或渠道密钥。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// WebhookEventSubscription 是生产者复用的订阅过滤面。
// 生产实现是 *WebhookSender（Subscribed 读取 notify.webhook.events 快照）；
// 配置损坏时 Subscribed 返回 false（fail-closed），生产者因此不写行。
type WebhookEventSubscription interface {
	Subscribed(event string) bool
}

// PaymentSuccessEvent 是充值订单结算成功的事件输入。
// AmountCents 是订单合同金额（最小货币单位），Credits 是实际到账积分。
type PaymentSuccessEvent struct {
	OrderID     int64
	AmountCents int64
	Credits     int64
	Provider    string
	PaidAt      time.Time
}

// UserSignupEvent 是注册成功的事件输入。CreatedAt 取 users.created_at
// （DB 时钟），而不是请求到达时间。
type UserSignupEvent struct {
	UserID    int64
	Username  string
	CreatedAt time.Time
}

// BalanceLowEvent 是 N6 低余额扫描命中一个用户时的输入（B4）。
//
// Balance/Threshold 都是积分整数；DetectedAt 是扫描轮次的「现在」
// （UTC 写入 payload）。SiteDay 是站点时区下的日期（YYYY-MM-DD），
// 只参与 dedupe_key，不写入 payload——它把「每人每天最多一条」钉在
// 业务时区上，跨时区副本得到同一天键。
type BalanceLowEvent struct {
	UserID     int64
	Balance    int64
	Threshold  int64
	DetectedAt time.Time
	SiteDay    string
}

// paymentSuccessPayload 是 payment_success 的冻结请求体。
// 字段名与顺序即接收方契约；新增字段需同步台账与设计回写。
type paymentSuccessPayload struct {
	Event       string `json:"event"`
	OrderID     int64  `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Credits     int64  `json:"credits"`
	Provider    string `json:"provider"`
	PaidAt      string `json:"paid_at"`
}

// userSignupPayload 是 user_signup 的冻结请求体。
// 明确不含 password / email_code / 会话令牌等敏感字段。
type userSignupPayload struct {
	Event     string `json:"event"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
}

// balanceLowPayload 是 balance_low 的冻结请求体。
// 只含余额事实与阈值，**不含**邮箱、手机号、用户名等可识别信息：该事件
// 会发往管理员配置的外部端点，最小化 PII 面。
type balanceLowPayload struct {
	Event      string `json:"event"`
	UserID     int64  `json:"user_id"`
	Balance    int64  `json:"balance"`
	Threshold  int64  `json:"threshold"`
	DetectedAt string `json:"detected_at"`
}

// WebhookEventProducer 把业务事件写成 webhook 投递行，可并发使用。
type WebhookEventProducer struct {
	store        *Store
	subscription WebhookEventSubscription
	log          *slog.Logger
}

// NewWebhookEventProducer 构造生产者。Store 与订阅面必填：缺任一都在启动期
// 暴露装配错误，而不是让生产者在运行期静默丢弃全部事件。
func NewWebhookEventProducer(store *Store, subscription WebhookEventSubscription, logger *slog.Logger) (*WebhookEventProducer, error) {
	if store == nil {
		return nil, errors.New("notify: webhook event producer requires a store")
	}
	if subscription == nil {
		return nil, errors.New("notify: webhook event producer requires a subscription reader")
	}
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &WebhookEventProducer{store: store, subscription: subscription, log: log}, nil
}

// EnqueuePaymentSuccess 写入一条 payment_success 投递。
//
// 订阅未开启时返回 nil 且不写行（不是错误）；只有序列化/DB 失败才返回
// error，调用方据此记 Warn。重复调用由 dedupe_key 幂等。
func (p *WebhookEventProducer) EnqueuePaymentSuccess(ctx context.Context, event PaymentSuccessEvent) error {
	eventName, ok := WebhookEventForCategory("billing.payment_success")
	if !ok {
		return errors.New("notify: billing.payment_success has no webhook event")
	}
	paidAt := event.PaidAt
	if paidAt.IsZero() {
		// 订单在成功分支一定有 paid_at；零值只可能来自调用方构造错误。
		// 不因此丢掉事件：用当前时刻保证 payload 仍是合法时间串。
		paidAt = time.Now()
	}
	_, err := p.enqueue(ctx, deliveryEvent{
		category:  "billing.payment_success",
		event:     eventName,
		dedupeKey: fmt.Sprintf("order:%d:payment_success", event.OrderID),
		payload: paymentSuccessPayload{
			Event:       "payment_success",
			OrderID:     event.OrderID,
			AmountCents: event.AmountCents,
			Credits:     event.Credits,
			Provider:    event.Provider,
			PaidAt:      paidAt.UTC().Format(time.RFC3339),
		},
	})
	return err
}

// EnqueueUserSignup 写入一条 user_signup 投递（category auth.welcome）。
func (p *WebhookEventProducer) EnqueueUserSignup(ctx context.Context, event UserSignupEvent) error {
	eventName, ok := WebhookEventForCategory("auth.welcome")
	if !ok {
		return errors.New("notify: auth.welcome has no webhook event")
	}
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	_, err := p.enqueue(ctx, deliveryEvent{
		category:  "auth.welcome",
		event:     eventName,
		dedupeKey: fmt.Sprintf("user:%d:signup", event.UserID),
		payload: userSignupPayload{
			Event:     "user_signup",
			UserID:    event.UserID,
			Username:  event.Username,
			CreatedAt: createdAt.UTC().Format(time.RFC3339),
		},
	})
	return err
}

// EnqueueBalanceLow 写入一条 balance_low 投递（N6 低余额扫描，B4）。
//
// 返回值 inserted 表示本次是否真的落了一行：同一用户同一天重复扫描由
// UNIQUE(dedupe_key) 去重，返回 (false, nil)。未订阅事件同样返回
// (false, nil)——扫描器已先做订阅过滤，这里的重复过滤是纵深防御。
// 只有序列化/DB 失败返回 error，调用方只记 Warn，绝不影响业务。
func (p *WebhookEventProducer) EnqueueBalanceLow(ctx context.Context, event BalanceLowEvent) (bool, error) {
	eventName, ok := WebhookEventForCategory("billing.balance_low")
	if !ok {
		return false, errors.New("notify: billing.balance_low has no webhook event")
	}
	detectedAt := event.DetectedAt
	if detectedAt.IsZero() {
		detectedAt = time.Now()
	}
	day := event.SiteDay
	if day == "" {
		// 调用方未给站点日期时退化为 UTC 日期；扫描器总会显式传入站点日期。
		day = detectedAt.UTC().Format("2006-01-02")
	}
	return p.enqueue(ctx, deliveryEvent{
		category:  "billing.balance_low",
		event:     eventName,
		dedupeKey: fmt.Sprintf("balance_low:%d:%s", event.UserID, day),
		payload: balanceLowPayload{
			Event:      "balance_low",
			UserID:     event.UserID,
			Balance:    event.Balance,
			Threshold:  event.Threshold,
			DetectedAt: detectedAt.UTC().Format(time.RFC3339),
		},
	})
}

type deliveryEvent struct {
	category  string
	event     string
	dedupeKey string
	payload   any
}

// enqueue 是各事件共用的写入路径：订阅过滤 → 序列化 → INSERT。
// inserted=false 表示未订阅或 dedupe_key 命中已有行（两种都算幂等成功）。
func (p *WebhookEventProducer) enqueue(ctx context.Context, event deliveryEvent) (bool, error) {
	if !p.subscription.Subscribed(event.event) {
		// 订阅过滤与 worker 的发送前过滤共用同一份配置语义；未订阅不写行，
		// 避免队列里堆积永远不会发送、也永远不会被 suppressed 的记录。
		p.log.Debug("webhook event skipped: not subscribed", "event", event.event)
		return false, nil
	}
	body, err := json.Marshal(event.payload)
	if err != nil {
		return false, fmt.Errorf("notify: marshal webhook payload %s: %w", event.event, err)
	}
	_, inserted, err := p.store.InsertDelivery(ctx, Delivery{
		Category:  event.category,
		Channel:   ChannelWebhook,
		Recipient: webhookRecipientPlaceholder,
		DedupeKey: event.dedupeKey,
		Status:    "queued",
		Attempt:   0,
		Payload:   body,
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// webhookRecipientPlaceholder 满足 notification_deliveries.recipient 的
// 非空约束。Webhook 的真实目标在发送时从配置中心读取（URL 可能被管理员
// 轮换），不在入队时冻结，也不把 URL 写进投递行。
const webhookRecipientPlaceholder = "webhook"
