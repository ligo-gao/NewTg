import type { components } from '@/api/schema'

/**
 * User-side subscription projections (billing-08 §15.4).
 *
 * These aliases deliberately point at the generated contract instead of
 * re-declaring shapes: `schema.d.ts` is the single compile-time link between
 * the frozen OpenAPI document and this feature.
 */
export type AvailableSubscriptionPlan =
  components['schemas']['AvailableSubscriptionPlan']
export type AvailableSubscriptionPlanList =
  components['schemas']['AvailableSubscriptionPlanList']
export type UserSubscription = components['schemas']['UserSubscription']
export type UserSubscriptionList = components['schemas']['UserSubscriptionList']
export type BillingPreference = components['schemas']['BillingPreference']
export type BillingPreferenceValue = BillingPreference['preference']
export type SubscriptionProvider =
  components['schemas']['SubscriptionPurchaseRequest']['provider']

/**
 * 续费/升降级只接受渠道支付（P2a / S2 契约冻结 provider ∈ stripe|alipay|epay）。
 *
 * 余额（wallet）只在**购买**时可用（P2b）：续费/升降级的金额由服务端按
 * 剩余价值折算，钱包扣款路径没有在这些端点实现，所以这里用独立别名而不是
 * 复用 `SubscriptionProvider`——否则调用方会把 wallet 发给必然 400 的端点。
 */
export type SubscriptionChannelProvider = 'stripe' | 'alipay' | 'epay'

/** 状态迁移事件流（S1/R-19）；`events` 是单次 after_seq 窗口。 */
export type SubscriptionEvent = components['schemas']['SubscriptionEvent']
export type SubscriptionEventList =
  components['schemas']['SubscriptionEventList']

/** 升降级响应（S2）：订单 + 折算结果，0 元路径没有支付载荷。 */
export type SubscriptionChangeResponse =
  components['schemas']['SubscriptionChangeResponse']

/** 退款申请回执（S3）：只记录申请标记，不动资金。 */
export type SubscriptionRefundRequestView =
  components['schemas']['SubscriptionRefundRequestView']

/**
 * S4 支付令牌/自动续费。`PaymentMethod` 是服务端的公开投影：只有
 * brand/last4/exp 与句柄 id，绝不包含令牌（P-14 零回显）。
 */
export type PaymentMethod = components['schemas']['PaymentMethod']
export type PaymentMethodList = components['schemas']['PaymentMethodList']
export type SubscriptionAutoRenewView =
  components['schemas']['SubscriptionAutoRenewView']

/** Reused verbatim so the purchase response flows into the top-up checkout. */
export type { CreateOrderResponse } from '../topup/types'
