import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'

import {
  TOPUP_CONFIG_KEY,
  WALLET_SUMMARY_KEY,
} from '../subscription/constants'

/**
 * Shared read models for the virtual wallet.
 *
 * The overview (wallet feature) and the top-up tab (topup feature) are both
 * part of the account domain; the query keys themselves are defined once in
 * `subscription/constants` (with the subscription flows that also read them),
 * so a range or tab switch never causes a second `/api/wallet/summary` call.
 */
export { TOPUP_CONFIG_KEY, WALLET_SUMMARY_KEY } from '../subscription/constants'

export function topupConfigQueryOptions() {
  return {
    queryKey: TOPUP_CONFIG_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/topup/config')),
  }
}

export function walletSummaryQueryOptions() {
  return {
    queryKey: WALLET_SUMMARY_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/wallet/summary')),
  }
}
