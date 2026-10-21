import type { components } from '@/api/schema'

/**
 * Contract aliases for the wallet transaction read surface (W7/M3).
 *
 * The generated `schema.d.ts` is the only compile-time link to the frozen
 * OpenAPI document; never redeclare these shapes locally.
 */
export type WalletTransaction = components['schemas']['WalletTransaction']
export type WalletTransactionPage = components['schemas']['WalletTransactionPage']
export type WalletTransactionDetail =
  components['schemas']['WalletTransactionDetail']
export type WalletTransactionType = WalletTransaction['type']
export type WalletTransactionStatus = WalletTransaction['status']
export type WalletTransactionFilterDir = 'in' | 'out'
export type WalletTransactionSegments =
  components['schemas']['WalletTransactionSegments']
export type WalletUsageContext = components['schemas']['WalletUsageContext']
export type WalletOrderContext = components['schemas']['WalletOrderContext']
export type WalletRedemptionContext =
  components['schemas']['WalletRedemptionContext']
export type WalletAdjustmentContext =
  components['schemas']['WalletAdjustmentContext']
