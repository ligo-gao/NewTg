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

/** Reused verbatim so the purchase response flows into the top-up checkout. */
export type { CreateOrderResponse } from '../topup/types'
