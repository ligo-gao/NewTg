import type { components } from '@/api/schema'

/**
 * Order states the console renders. Five come from the frozen P-1 state machine;
 * `refund_required` was added by S2/R-29② for the "money received, fulfilment
 * failed" case (e.g. a plan change paid after the subscription was cancelled).
 * S3 adds `refunded` (refund completed) and `refund_failed` (provider refund
 * failed, retryable), so the UI must show them instead of falling back to an
 * em dash. The UI still never invents `paid`.
 */
export type OrderStatus =
  | 'pending'
  | 'succeeded'
  | 'refund_required'
  | 'refunded'
  | 'refund_failed'
  | 'failed'
  | 'expired'
  | 'cancelled'

export type TopupProvider = 'stripe' | 'alipay'

/**
 * Providers this build can render end-to-end. The server may advertise `epay`
 * (`enabled_providers`), but its redirect-form checkout is not wired into the
 * console yet (registered as G-DOM-72 ④); filtering keeps the picker from
 * offering a flow that cannot complete. Widen this when the epay form lands.
 */
export function isUiSupportedProvider(provider: string): provider is TopupProvider {
  return provider === 'stripe' || provider === 'alipay'
}

export type TopupConfig = components['schemas']['TopupConfig']
export type OrderView = components['schemas']['OrderView']
export type TopupOrderView = components['schemas']['TopupOrderView']
export type CreateOrderResponse = components['schemas']['CreateOrderResponse']
export type OrderList = components['schemas']['OrderList']
export type RedeemCodeResponse = components['schemas']['RedeemCodeResponse']
export type WalletSummary = components['schemas']['WalletSummary']
