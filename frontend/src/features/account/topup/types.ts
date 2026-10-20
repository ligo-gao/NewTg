import type { components } from '@/api/schema'

/** Order states frozen by P-1 (the UI never invents `paid`/`refunded`). */
export type OrderStatus =
  | 'pending'
  | 'succeeded'
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
