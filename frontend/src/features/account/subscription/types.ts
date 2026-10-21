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
 * S4 支付令牌/自动续费。`PaymentMethod` 是服务端的公开投影：只有
 * brand/last4/exp 与句柄 id，绝不包含令牌（P-14 零回显）。
 */
export type PaymentMethod = components['schemas']['PaymentMethod']
export type PaymentMethodList = components['schemas']['PaymentMethodList']
export type SubscriptionAutoRenewView =
  components['schemas']['SubscriptionAutoRenewView']

/** Reused verbatim so the purchase response flows into the top-up checkout. */
export type { CreateOrderResponse } from '../topup/types'
