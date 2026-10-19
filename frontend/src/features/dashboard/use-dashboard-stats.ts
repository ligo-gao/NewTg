import { useQuery } from '@tanstack/react-query'

import {
  fetchUsageByModel,
  fetchUsageRates,
  fetchUsageSummary,
  fetchUsageTimeseries,
  granularityFor,
  type UsageRange,
} from './api'

export interface DashboardStatsParams {
  range: UsageRange
  /** Admin filter, already applied (submit), not the draft input value. */
  username?: string
  /** Legend drill-down for the trend query only. */
  model?: string
}

/**
 * All four dashboard requests are issued from one hook so a range (or admin
 * filter) change rekeys every query in the same render. TanStack Query fires
 * them concurrently; there is no waterfall between cards, ring and trend.
 */
export function useDashboardStats({
  range,
  username,
  model,
}: DashboardStatsParams) {
  const normalizedUsername = username?.trim() === '' ? undefined : username?.trim()
  const granularity = granularityFor(range)

  const summary = useQuery({
    queryKey: ['dashboard', 'summary', range, normalizedUsername ?? null],
    queryFn: () => fetchUsageSummary({ range, username: normalizedUsername }),
  })

  const byModel = useQuery({
    queryKey: ['dashboard', 'by-model', range, normalizedUsername ?? null],
    queryFn: () => fetchUsageByModel({ range, username: normalizedUsername }),
  })

  const timeseries = useQuery({
    queryKey: [
      'dashboard',
      'timeseries',
      range,
      normalizedUsername ?? null,
      model ?? null,
      granularity,
    ],
    queryFn: () =>
      fetchUsageTimeseries({
        range,
        username: normalizedUsername,
        model,
        granularity,
      }),
  })

  const rates = useQuery({
    queryKey: ['dashboard', 'rates', range, normalizedUsername ?? null],
    queryFn: () => fetchUsageRates({ range, username: normalizedUsername }),
  })

  return { summary, byModel, timeseries, rates, granularity }
}
