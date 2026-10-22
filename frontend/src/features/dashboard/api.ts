import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'

/**
 * Usage-statistics API (modules-05 F-05-03 / F-05-04).
 *
 * The dashboard consumes the same aggregation family as the logs page, so the
 * query objects are derived from the generated operations rather than declared
 * locally. `username` is only ever populated by the admin filter: the server
 * rejects admin-only parameters on a non-admin session with 403, so a stray
 * value here would turn a dashboard load into an error page.
 */
export type UsageRange = '24h' | '7d' | '14d' | '30d'
export type UsageGranularity = 'hour' | 'day'

export type UsageStatsSummary = components['schemas']['UsageStatsSummary']
export type UsageStatsByModelItem =
  components['schemas']['UsageStatsByModelItem']
export type UsageStatsTimeseriesItem =
  components['schemas']['UsageStatsTimeseriesItem']
export type UsageStatsRates = components['schemas']['UsageStatsRates']
export type CheckinResult = components['schemas']['CheckinResult']
export type CheckinCalendar = components['schemas']['CheckinCalendar']

export interface UsageStatsQuery {
  range: UsageRange
  /** Admin + logs:read only. Omitted entirely for every other viewer. */
  username?: string
}

export interface UsageTimeseriesQuery extends UsageStatsQuery {
  granularity: UsageGranularity
  /** Optional drill-down selected from the distribution legend. */
  model?: string
}

type SummaryQuery = NonNullable<
  operations['getUsageStatsSummary']['parameters']['query']
>
type ByModelQuery = NonNullable<
  operations['getUsageStatsByModel']['parameters']['query']
>
type TimeseriesQuery = NonNullable<
  operations['getUsageStatsTimeseries']['parameters']['query']
>
type RatesQuery = NonNullable<
  operations['getUsageStatsRates']['parameters']['query']
>

function baseQuery(params: UsageStatsQuery): {
  range: UsageRange
  username?: string
} {
  const query: { range: UsageRange; username?: string } = { range: params.range }
  const username = params.username?.trim()
  if (username !== undefined && username !== '') query.username = username
  return query
}

/**
 * Granularity is automatic (R-M6-12): the 24h view is hourly, every wider
 * window is daily. Keeping this as a pure function means the query key and the
 * request always agree — a manual switch cannot desynchronise them.
 */
export function granularityFor(range: UsageRange): UsageGranularity {
  return range === '24h' ? 'hour' : 'day'
}

export async function fetchUsageSummary(
  params: UsageStatsQuery,
): Promise<UsageStatsSummary> {
  const query: SummaryQuery = baseQuery(params)
  return requireData(
    await apiClient.GET('/api/usage-stats/summary', { params: { query } }),
  )
}

export async function fetchUsageByModel(
  params: UsageStatsQuery,
): Promise<UsageStatsByModelItem[]> {
  const query: ByModelQuery = baseQuery(params)
  return requireData(
    await apiClient.GET('/api/usage-stats/by-model', { params: { query } }),
  )
}

export async function fetchUsageTimeseries(
  params: UsageTimeseriesQuery,
): Promise<UsageStatsTimeseriesItem[]> {
  const query: TimeseriesQuery = {
    ...baseQuery(params),
    granularity: params.granularity,
  }
  const model = params.model?.trim()
  if (model !== undefined && model !== '') query.model = model
  return requireData(
    await apiClient.GET('/api/usage-stats/timeseries', { params: { query } }),
  )
}

export async function fetchUsageRates(
  params: UsageStatsQuery,
): Promise<UsageStatsRates> {
  const query: RatesQuery = baseQuery(params)
  return requireData(
    await apiClient.GET('/api/usage-stats/rates', { params: { query } }),
  )
}

/**
 * Daily check-in (modules-01 §3.19 F-01-30 / modules-03 F-03-12).
 *
 * The server owns the natural-day boundary (site timezone) and the reward
 * draw; the client only submits the optional Turnstile token. No user id or
 * amount is ever sent — the contract has no such fields.
 */
export async function fetchCheckinCalendar(): Promise<CheckinCalendar> {
  return requireData(await apiClient.GET('/api/checkin/calendar'))
}

export async function submitCheckin(
  turnstileToken?: string,
): Promise<CheckinResult> {
  const body: { turnstile_token?: string } = {}
  if (turnstileToken !== undefined && turnstileToken !== '') {
    body.turnstile_token = turnstileToken
  }
  return requireData(await apiClient.POST('/api/checkin', { body }))
}
