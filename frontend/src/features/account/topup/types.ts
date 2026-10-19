import type { components } from '@/api/schema'

/** Order states frozen by P-1 (the UI never invents `paid`/`refunded`). */
export type OrderStatus =
  | 'pending'
  | 'succeeded'
  | 'failed'
  | 'expired'
  | 'cancelled'

export type TopupProvider = 'stripe' | 'alipay'

export type TopupConfig = components['schemas']['TopupConfig']
export type OrderView = components['schemas']['OrderView']
export type TopupOrderView = components['schemas']['TopupOrderView']
export type CreateOrderResponse = components['schemas']['CreateOrderResponse']
export type OrderList = components['schemas']['OrderList']
export type RedeemCodeResponse = components['schemas']['RedeemCodeResponse']
export type WalletSummary = components['schemas']['WalletSummary']
