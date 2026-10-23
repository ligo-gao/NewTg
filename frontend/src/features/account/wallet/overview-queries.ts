import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'

import { MY_SUBSCRIPTIONS_KEY } from '../subscription/my-subscriptions'

/** Windows the overview trend offers (the dashboard uses 24h/7d/14d/30d). */
export type WalletUsageRange = '7d' | '30d'

/** The overview deliberately shows a short ledger; the full list owns filters. */
const WALLET_OVERVIEW_LEDGER_SIZE = 6

export function recentTransactionsQueryOptions() {
  return {
    queryKey: ['wallet', 'transactions', 'recent'] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/wallet/transactions', {
          params: {
            query: { p: 1, page_size: WALLET_OVERVIEW_LEDGER_SIZE },
          },
        }),
      ),
  }
}

export function walletUsageSummaryQueryOptions(range: WalletUsageRange) {
  return {
    queryKey: ['wallet', 'usage-summary', range] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/summary', {
          params: { query: { range } },
        }),
      ),
  }
}

export function walletUsageTimeseriesQueryOptions(range: WalletUsageRange) {
  return {
    queryKey: ['wallet', 'usage-timeseries', range] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/timeseries', {
          params: { query: { range, granularity: 'day' } },
        }),
      ),
  }
}

/**
 * Same key and page size as `SubscriptionSection`, so opening the overview's
 * card warms the cache the subscription tab reads (and vice versa).
 */
export function overviewSubscriptionsQueryOptions() {
  return {
    queryKey: MY_SUBSCRIPTIONS_KEY,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/subscriptions', {
          params: { query: { page_size: 100 } },
        }),
      ),
  }
}
