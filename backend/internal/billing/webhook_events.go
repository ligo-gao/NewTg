package billing

// B1b：充值结算成功后的 Webhook 事件生产（payment_success）。
//
// 触发点是 settleOrder 的 **topup 分支**（订阅购买/续费走各自的结算函数，
// 不产生充值事件），且只在事务提交后调用。语义：
//   - 幂等：dedupe_key = order:<id>:payment_success，重复回调/重放最多一行；
//   - best-effort：入队失败只 Warn，绝不把已成功的入账回滚或让回调返回错误
//     （渠道会因此重投，而入账幂等不会重复加钱，但通知本就可补）；
//   - 订阅过滤在生产者内完成，未订阅不写行。
//
// 生产实现是 *notify.WebhookEventProducer；nil 表示未接线（纯计费单测）。

import (
	"context"

	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

// PaymentSuccessWebhook 是结算成功事件的生产者接缝。
type PaymentSuccessWebhook interface {
	EnqueuePaymentSuccess(ctx context.Context, event notify.PaymentSuccessEvent) error
}

// enqueuePaymentSuccess 在订单已成功且状态已刷新后写入事件。
//
// 并发回调的可能结果是「本请求 credited=false，但另一请求已完成入账」：
// 此时订单刷新为 succeeded，同样尝试入队，靠 dedupe_key 收敛为一行。
func (s *TopupService) enqueuePaymentSuccess(ctx context.Context, order *store.Order) {
	if s.webhook == nil || order == nil || order.Status != string(OrderSucceeded) {
		return
	}
	paidAt := s.now().UTC()
	if order.PaidAt != nil {
		paidAt = order.PaidAt.UTC()
	}
	if err := s.webhook.EnqueuePaymentSuccess(ctx, notify.PaymentSuccessEvent{
		OrderID:     order.ID,
		AmountCents: order.Amount,
		Credits:     order.CreditedCredits,
		Provider:    order.Provider,
		PaidAt:      paidAt,
	}); err != nil {
		// 事件是可补的副作用；钱已经到账。日志只带订单号与错误分类，
		// 不带渠道原始报文。
		s.log.Warn("payment success webhook enqueue failed",
			"order_id", order.ID, "provider", order.Provider, "error", err)
	}
}
