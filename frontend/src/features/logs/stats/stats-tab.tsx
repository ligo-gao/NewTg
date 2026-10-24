import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatCredits, formatTokenAmount } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'

import { formatSeconds } from '../format'
import {
  usageRangeParams,
  usageStatsGranularity,
  type UsageLogSearch,
} from '../search'
import { EmptyState, InlineError } from '../table-ui'
import {
  DistributionCard,
  RecentFailures,
  type DistributionItem,
} from './breakdowns'
import {
  formatBucketTick,
  tokenSplit,
  topActiveBuckets,
  trendInsights,
  type TrendPoint,
} from './insights'
import { fillTimeseries } from './series'
import styles from './stats-tab.module.css'
import { TrendChart } from './trend-chart'

type UsageStatsByTokenItem = components['schemas']['UsageStatsByTokenItem']
type UsageStatsByUserItem = components['schemas']['UsageStatsByUserItem']
type UsageStatsByGroupItem = components['schemas']['UsageStatsByGroupItem']

type StatsQuery = NonNullable<
  operations['getUsageStatsByModel']['parameters']['query']
>
type TimeseriesQuery = NonNullable<
  operations['getUsageStatsTimeseries']['parameters']['query']
>
type RatesQuery = NonNullable<
  operations['getUsageStatsRates']['parameters']['query']
>
type ListQuery = NonNullable<operations['listUsageLogs']['parameters']['query']>

/**
 * Shared filter set for every `/api/usage-stats/*` call. It is derived from
 * the same route search as the list, which is what keeps the cards, the charts
 * and the table on one window (F-05-03 §5.7).
 *
 * `request_id` and pagination are deliberately absent: the aggregation
 * endpoints do not accept a request id, and page offsets are meaningless for
 * aggregates.
 */
function buildStatsQuery(
  search: UsageLogSearch,
  canFilterByUsername: boolean,
): StatsQuery {
  const query: StatsQuery = { ...usageRangeParams(search) }
  if (search.model !== undefined) query.model = search.model
  if (search.group !== undefined) query.group = search.group
  if (search.token_id !== undefined) query.token_id = search.token_id
  if (search.status !== undefined) query.status = search.status
  // Admin-only, exactly as on the list; the server would answer 403 otherwise.
  if (canFilterByUsername && search.username !== undefined) {
    query.username = search.username
  }
  return query
}

function statsErrorText(
  error: unknown,
  t: (key: string) => string,
): string {
  if (error instanceof ApiError) {
    if (error.kind === 'offline') return t('logs.usage.error.offline')
    if (error.kind === 'validation') return t('logs.usage.error.invalid_range')
  }
  return t('logs.usage.error.load')
}

export interface UsageStatsTabProps {
  search: UsageLogSearch
  canFilterByUsername: boolean
  /** Admin sees the all-site distributions and the triage link. */
  isAdmin: boolean
  /**
   * The stats panel stays mounted (WAI-ARIA keeps both tabpanels in the DOM),
   * so queries are gated on the active tab: switching back to the list must
   * not keep polling aggregates in the background.
   */
  active: boolean
  /**
   * Session capabilities are still loading. Waiting mirrors the list: an
   * admin's `?username=` filter must be applied before the first aggregate
   * request, otherwise the first paint briefly shows every user's totals.
   */
  ready: boolean
  onOpenLog: (logId: number) => void
  onViewAllFailures: () => void
}

/**
 * F-05-03 stats board (UI §3.2). One keyed filter set drives every aggregate;
 * each section degrades on its own — a failed cards query never blocks the
 * trend, and every error keeps an inline retry.
 *
 * The board is role-aware: an admin sees the all-site group/user rankings and
 * a link into failure triage; everyone else sees their own token split and the
 * newest failures, which route into the detail drawer.
 */
export function UsageStatsTab({
  search,
  canFilterByUsername,
  isAdmin,
  active,
  ready,
  onOpenLog,
  onViewAllFailures,
}: UsageStatsTabProps) {
  const { t, locale } = useI18n()
  const granularity = usageStatsGranularity(search)
  const query = buildStatsQuery(search, canFilterByUsername)
  const enabled = active && ready

  const summaryQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'summary', query],
    enabled,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/summary', {
          params: { query },
        }),
      ),
  })

  const byModelQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'by-model', query],
    enabled,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/by-model', {
          params: { query },
        }),
      ),
  })

  const byGroupQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'by-group', query],
    enabled: enabled && isAdmin,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/by-group', {
          params: { query },
        }),
      ),
  })

  const byUserQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'by-user', query],
    enabled: enabled && isAdmin,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/by-user', {
          params: { query },
        }),
      ),
  })

  const byTokenQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'by-token', query],
    // User-only card; an admin never renders the token distribution.
    enabled: enabled && !isAdmin,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/by-token', {
          params: { query },
        }),
      ),
  })

  // User-only failure list. `status` is forced to `error` so the card never
  // inherits the list's current status filter; the range and the row filters
  // still describe the same window as the cards above.
  //
  // Enabled only when the summary reports a failure: with zero failures the
  // card is already known-empty, and a second `/api/usage-logs` call would be
  // indistinguishable from a list refetch. A failed summary still enables the
  // card so the section keeps its own retry.
  const failureQuery: ListQuery = {
    ...usageRangeParams(search),
    status: 'error',
    p: 1,
    page_size: 10,
    ...(search.model !== undefined ? { model: search.model } : {}),
    ...(search.group !== undefined ? { group: search.group } : {}),
    ...(search.token_id !== undefined ? { token_id: search.token_id } : {}),
  }
  const failureCountInSummary =
    summaryQuery.data === undefined
      ? null
      : (summaryQuery.data.error_count ?? 0) +
        (summaryQuery.data.refused_count ?? 0)
  const failuresEnabled =
    enabled &&
    !isAdmin &&
    (summaryQuery.isError ||
      (failureCountInSummary !== null && failureCountInSummary > 0))
  const failuresQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'failures', failureQuery],
    enabled: failuresEnabled,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-logs', { params: { query: failureQuery } }),
      ),
  })

  const timeseriesQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'timeseries', query, granularity],
    enabled,
    queryFn: async () => {
      const timeseriesQuery: TimeseriesQuery = { ...query, granularity }
      return requireData(
        await apiClient.GET('/api/usage-stats/timeseries', {
          params: { query: timeseriesQuery },
        }),
      )
    },
  })

  // `getUsageStatsRates` in the frozen OpenAPI document omits the model/group/
  // token/status fields even though the backend applies the same `statsWhere`
  // filter set. Keeping them makes the rail describe the same window as the
  // cards instead of silently widening to all models.
  const ratesQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'rates', query],
    enabled:
      enabled &&
      summaryQuery.data !== undefined &&
      summaryQuery.data.request_count > 0,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/rates', {
          params: { query: query as unknown as RatesQuery },
        }),
      ),
  })

  const summary = summaryQuery.data
  const byModel = byModelQuery.data
  const trendRows: TrendPoint[] = fillTimeseries(
    timeseriesQuery.data ?? [],
    search,
    granularity,
  )
  const insights = trendInsights(trendRows)
  const peak = insights.peak
  const activeBuckets = topActiveBuckets(trendRows, 5)

  const emptyHint =
    canFilterByUsername && search.username !== undefined
      ? t('logs.usage.stats.no_user_data')
      : t('logs.usage.stats.empty_hint')

  const count = (value: number): string =>
    new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
  const decimal = (value: number): string =>
    new Intl.NumberFormat(locale, {
      minimumFractionDigits: 0,
      maximumFractionDigits: 2,
    }).format(value)
  const oneDecimal = (value: number): string =>
    new Intl.NumberFormat(locale, {
      minimumFractionDigits: 1,
      maximumFractionDigits: 1,
    }).format(value)
  const compact = (value: number): string =>
    new Intl.NumberFormat(locale, {
      notation: 'compact',
      maximumFractionDigits: 1,
    }).format(value)

  const spendTooltip =
    summary === undefined
      ? undefined
      : t('logs.usage.stats.spend_tooltip', {
          pre: formatCredits(summary.pre_deducted_total, locale),
          refunded: formatCredits(summary.refunded_total, locale),
        })

  const failureCount =
    summary === undefined
      ? 0
      : (summary.error_count ?? 0) + (summary.refused_count ?? 0)
  const retryLabel = t('logs.usage.error.retry')
  const granularityKey =
    granularity === 'hour'
      ? 'logs.usage.stats.granularity_hour'
      : 'logs.usage.stats.granularity_day'

  const split =
    summary === undefined
      ? null
      : tokenSplit({
          prompt: summary.prompt_tokens_total,
          completion: summary.completion_tokens_total,
          cache: summary.cache_read_tokens_total ?? 0,
          requests: summary.request_count,
        })
  const inputOutputTotal =
    summary === undefined
      ? 0
      : summary.prompt_tokens_total + summary.completion_tokens_total

  const modelItems: DistributionItem[] = (byModel ?? []).map((item) => ({
    key: item.model,
    label:
      item.model === 'other' ? t('logs.usage.stats.other') : item.model,
    cost: item.cost,
    requests: item.requests,
    tokens: item.tokens,
    percent: item.percent,
  }))

  const toDistributionItem = (
    raw: {
      cost: number
      requests: number
      tokens: number
      percent: number
    },
    key: string,
    label: string,
  ): DistributionItem => ({
    key,
    label,
    cost: raw.cost,
    requests: raw.requests,
    tokens: raw.tokens,
    percent: raw.percent,
  })

  const groupItems: DistributionItem[] = (byGroupQuery.data?.items ?? []).map(
    (item: UsageStatsByGroupItem) =>
      toDistributionItem(
        item,
        item.group,
        item.group === 'other'
          ? t('logs.usage.stats.other')
          : item.group,
      ),
  )

  const userItems: DistributionItem[] = (byUserQuery.data?.items ?? []).map(
    (item: UsageStatsByUserItem) =>
      toDistributionItem(
        item,
        `${String(item.user_id)}:${item.username}`,
        item.username === 'other'
          ? t('logs.usage.stats.other')
          : item.username === ''
            ? `#${String(item.user_id)}`
            : item.username,
      ),
  )

  const tokenItems: DistributionItem[] = (
    byTokenQuery.data?.items ?? []
  ).map((item: UsageStatsByTokenItem) =>
    toDistributionItem(
      item,
      `${String(item.token_id)}:${item.token_name}`,
      item.token_name === ''
        ? t('logs.usage.stats.token_key.unassigned')
        : item.token_name === 'other'
          ? t('logs.usage.stats.other')
          : item.token_name,
    ),
  )

  const bandColumns = isAdmin
    ? 'xl:grid-cols-[1.35fr_repeat(5,minmax(0,1fr))]'
    : 'xl:grid-cols-[1.35fr_repeat(4,minmax(0,1fr))]'
  const bandClass = [
    styles.band,
    // The cells own the hairlines (stats-tab.module.css); the band itself is
    // only the rounded outline and the card fill behind an unfilled slot.
    'grid overflow-hidden rounded-xl border border-border-hairline bg-surface-card',
    'sm:grid-cols-3',
    bandColumns,
  ].join(' ')

  return (
    <div className="flex flex-col gap-3.5" data-testid="usage-stats-tab">
      {summaryQuery.isError ? (
        <InlineError
          message={statsErrorText(summaryQuery.error, t)}
          onRetry={() => void summaryQuery.refetch()}
          retryLabel={retryLabel}
        />
      ) : null}

      {/* KPI band: one hairline grid, no nested card borders. */}
      {summaryQuery.isPending ? (
        <div
          data-testid="stats-cards-loading"
          aria-busy="true"
          className={bandClass}
        >
          {Array.from({ length: isAdmin ? 6 : 5 }, (_, index) => (
            <div
              key={index}
              className="h-24 animate-pulse bg-surface-card motion-reduce:animate-none"
            />
          ))}
        </div>
      ) : (
        <section
          aria-label={t('logs.usage.stats.cards_label')}
          className={bandClass}
        >
          <MetricCard
            label={t('logs.usage.stats.total_spent')}
            value={summary === undefined ? '—' : formatCredits(summary.cost_total, locale)}
            unit={t('logs.usage.credits')}
            title={spendTooltip}
            foot={
              summary === undefined
                ? undefined
                : isAdmin
                  ? t('logs.usage.stats.kpi.foot.pre_refund', {
                      pre: formatCredits(summary.pre_deducted_total, locale),
                      refunded: formatCredits(summary.refunded_total, locale),
                    })
                  : t('logs.usage.stats.kpi.foot.avg_request', {
                      value: decimal(
                        summary.request_count === 0
                          ? 0
                          : summary.cost_total / summary.request_count,
                      ),
                      refunded: formatCredits(summary.refunded_total, locale),
                    })
            }
          />
          <MetricCard
            label={t('logs.usage.stats.requests')}
            value={summary === undefined ? '—' : count(summary.request_count)}
            foot={
              summary === undefined
                ? undefined
                : t('logs.usage.stats.kpi.foot.status_counts', {
                    success: count(summary.success_count ?? 0),
                    error: count(summary.error_count ?? 0),
                    refunded: count(summary.refunded_count ?? 0),
                    refused: count(summary.refused_count ?? 0),
                  })
            }
          />
          <MetricCard
            label={t('logs.usage.stats.kpi.success')}
            value={
              summary === undefined
                ? '—'
                : `${((summary.success_rate ?? 0) * 100).toFixed(1)}%`
            }
            foot={
              failureCount === 0
                ? undefined
                : isAdmin
                  ? (
                      <a
                        href="/admin/system/failure-triage"
                        className="text-brand hover:text-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      >
                        {t('logs.usage.stats.kpi.foot.failures_admin', {
                          count: count(failureCount),
                        })}
                      </a>
                    )
                  : (
                      <button
                        type="button"
                        onClick={onViewAllFailures}
                        className="text-brand hover:text-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      >
                        {t('logs.usage.stats.kpi.foot.failures_user', {
                          count: count(failureCount),
                        })}
                      </button>
                    )
            }
          />
          <MetricCard
            label={t('logs.usage.stats.kpi.tokens')}
            value={
              summary === undefined
                ? '—'
                : formatTokenAmount(
                    summary.prompt_tokens_total + summary.completion_tokens_total,
                    locale,
                  )
            }
            foot={
              summary === undefined
                ? undefined
                : t('logs.usage.stats.kpi.foot.token_split', {
                    input: formatTokenAmount(summary.prompt_tokens_total, locale),
                    output: formatTokenAmount(
                      summary.completion_tokens_total,
                      locale,
                    ),
                  })
            }
          />
          <MetricCard
            label={t('logs.usage.stats.kpi.latency')}
            value={
              summary === undefined || (summary.latency_samples ?? 0) === 0
                ? '—'
                : formatSeconds(summary.p50_latency_ms)
            }
            foot={
              summary === undefined
                ? undefined
                : t('logs.usage.stats.kpi.foot.p95', {
                    p95:
                      (summary.latency_samples ?? 0) === 0
                        ? '—'
                        : formatSeconds(summary.p95_latency_ms),
                    samples: count(summary.latency_samples ?? 0),
                  })
            }
          />
          {isAdmin ? (
            <MetricCard
              label={t('logs.usage.stats.kpi.active')}
              value={
                summary === undefined ? '—' : count(summary.active_users ?? 0)
              }
              foot={t('logs.usage.stats.kpi.foot.active_note')}
            />
          ) : null}
        </section>
      )}

      {/* Trend + rail. */}
      <div className="grid items-start gap-3.5 xl:grid-cols-[minmax(0,2.1fr)_minmax(0,1fr)]">
        <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
          <CardHeader
            title={t('logs.usage.stats.trend')}
            subtitle={t('logs.usage.stats.chart.sub')}
            aside={
              <div className="flex flex-wrap items-center justify-end gap-3">
                <span className="inline-flex items-center gap-1.5 text-badge text-ink-2">
                  <i
                    aria-hidden="true"
                    className="size-2 rounded-sm bg-chart-cat-1"
                  />
                  {t('logs.usage.stats.chart.cost_series')}
                </span>
                <span className="inline-flex items-center gap-1.5 text-badge text-ink-2">
                  <i
                    aria-hidden="true"
                    className="size-2 rounded-sm bg-chart-cat-2"
                  />
                  {t('logs.usage.stats.chart.request_series')}
                </span>
                <span className="rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 text-badge text-ink-2">
                  {t(granularityKey)}
                </span>
              </div>
            }
          />
          <div className="px-4 pb-4">
            {timeseriesQuery.isPending ? (
              <div
                data-testid="stats-trend-loading"
                aria-busy="true"
                className="h-60 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
              />
            ) : timeseriesQuery.isError ? (
              <InlineError
                message={statsErrorText(timeseriesQuery.error, t)}
                onRetry={() => void timeseriesQuery.refetch()}
                retryLabel={retryLabel}
              />
            ) : trendRows.length === 0 ? (
              <EmptyState
                title={t('logs.usage.stats.empty')}
                hint={emptyHint}
              />
            ) : (
              <>
                <TrendChart rows={trendRows} granularity={granularity} />
                <div className="mt-3 flex flex-wrap items-center gap-x-5 gap-y-1 border-t border-border-hairline pt-2.5 text-badge text-ink-2">
                  {peak !== null ? (
                    <span>
                      {t('logs.usage.stats.chart.peak', {
                        time: formatBucketTick(peak.bucket, granularity, locale),
                        cost: formatCredits(peak.cost, locale),
                        percent: oneDecimal(insights.peakShare),
                      })}
                    </span>
                  ) : null}
                  <span>
                    {t('logs.usage.stats.chart.active', {
                      active: count(insights.activeBuckets),
                      total: count(insights.bucketCount),
                      unit: t(
                        granularity === 'hour'
                          ? 'logs.usage.stats.chart.unit_hour'
                          : 'logs.usage.stats.chart.unit_day',
                      ),
                    })}
                  </span>
                  <span>
                    {t(
                      granularity === 'hour'
                        ? 'logs.usage.stats.chart.average_hour'
                        : 'logs.usage.stats.chart.average_day',
                      {
                        value: decimal(insights.averageCost),
                      },
                    )}
                  </span>
                  <span className="text-ink-3">
                    {t('logs.usage.stats.chart.zero_note', {
                      count: count(insights.zeroBuckets),
                    })}
                  </span>
                </div>
                {/* Relief rule: every chart carries a table equivalent. The
                    component's own sr-only table is the screen-reader path;
                    this visible disclosure is the sighted-reader path (UI §3.8). */}
                <details className="mt-3">
                  <summary className="cursor-pointer text-meta font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none">
                    {t('logs.usage.stats.view_table')}
                  </summary>
                  <div className="overflow-x-auto">
                    <table
                      data-testid="stats-trend-table"
                      className="mt-2 w-full text-body"
                    >
                      <caption className="sr-only">
                        {t('logs.usage.stats.trend_table_caption')}
                      </caption>
                      <thead>
                        <tr className="border-b border-border-hairline text-left text-meta font-medium text-ink-2">
                          <th scope="col" className="px-2 py-2">
                            {t('logs.usage.stats.columns.time')}
                          </th>
                          <th scope="col" className="px-2 py-2 text-right">
                            {t('logs.usage.stats.columns.cost')}
                          </th>
                          <th scope="col" className="px-2 py-2 text-right">
                            {t('logs.usage.stats.columns.requests')}
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {trendRows.map((row) => (
                          <tr
                            key={row.bucket}
                            className="border-b border-border-hairline last:border-0"
                          >
                            <td className="px-2 py-1.5 font-mono text-meta">
                              {formatBucketTick(row.bucket, granularity, locale)}
                            </td>
                            <td className="px-2 py-1.5 text-right font-mono text-meta tabular-nums">
                              {formatCredits(row.cost, locale)}
                            </td>
                            <td className="px-2 py-1.5 text-right tabular-nums">
                              {count(row.requests)}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </details>
              </>
            )}
          </div>
        </section>

        <div className="grid min-w-0 gap-3.5 lg:grid-cols-2 xl:grid-cols-1">
          <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
            <CardHeader
              title={t('logs.usage.stats.rail.title')}
              subtitle={t('logs.usage.stats.rail.sub')}
            />
            <div className="flex flex-col px-4 text-meta">
              <RailRow
                label={t('logs.usage.stats.rail.peak')}
                value={
                  peak === null
                    ? '—'
                    : `${formatBucketTick(peak.bucket, granularity, locale)} · ${formatCredits(peak.cost, locale)}`
                }
              />
              <RailRow
                label={t('logs.usage.stats.rail.peak_share')}
                value={`${oneDecimal(insights.peakShare)}%`}
              />
              <RailRow
                label={t('logs.usage.stats.rail.active')}
                value={t('logs.usage.stats.rail.hours_value', {
                  active: count(insights.activeBuckets),
                  total: count(insights.bucketCount),
                  unit: t(
                    granularity === 'hour'
                      ? 'logs.usage.stats.chart.unit_hour'
                      : 'logs.usage.stats.chart.unit_day',
                  ),
                })}
              />
              <RailRow
                label={t(
                  granularity === 'hour'
                    ? 'logs.usage.stats.rail.avg_hour'
                    : 'logs.usage.stats.rail.avg_day',
                )}
                value={decimal(insights.averageCost)}
              />
              <RailRow
                label={t('logs.usage.stats.rail.avg_request')}
                value={`${formatTokenAmount(split?.avgPerRequest ?? 0, locale)} ${t('logs.usage.stats.columns.tokens')}`}
              />
              {summary !== undefined &&
              summary.request_count > 0 &&
              (ratesQuery.data !== undefined || ratesQuery.isError) ? (
                <div
                  data-testid="stats-rates"
                  role="group"
                  aria-label={t('logs.usage.stats.rates.title')}
                  className="flex flex-col"
                >
                  {ratesQuery.isError ? (
                    <InlineError
                      message={statsErrorText(ratesQuery.error, t)}
                      onRetry={() => void ratesQuery.refetch()}
                      retryLabel={retryLabel}
                    />
                  ) : ratesQuery.data !== undefined ? (
                    <>
                      <RailRow
                        label={t('logs.usage.stats.rail.rpm')}
                        value={t('logs.usage.stats.rail.rpm_value', {
                          avg: oneDecimal(ratesQuery.data.rpm_avg),
                          peak: oneDecimal(ratesQuery.data.rpm_peak),
                        })}
                      />
                      <RailRow
                        label={t('logs.usage.stats.rail.tpm')}
                        value={t('logs.usage.stats.rail.tpm_value', {
                          avg: compact(ratesQuery.data.tpm_avg),
                        })}
                      />
                    </>
                  ) : null}
                </div>
              ) : null}
            </div>
            <div className="border-t border-border-hairline px-4 py-2.5">
              <div className="flex items-center justify-between gap-3 text-badge text-ink-3">
                <span>{t('logs.usage.stats.rail.active_list')}</span>
                <span>{t('logs.usage.stats.rail.active_columns')}</span>
              </div>
              <div className="mt-1 flex flex-col">
                {activeBuckets.map((row) => (
                  <div
                    key={row.bucket}
                    className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-3 border-b border-border-hairline py-1.5 text-badge last:border-0"
                  >
                    <span className="font-mono text-ink-2">
                      {formatBucketTick(row.bucket, granularity, locale)}
                    </span>
                    <span className="font-mono tabular-nums text-ink-1">
                      {formatCredits(row.cost, locale)}
                    </span>
                    <span className="min-w-8 text-right tabular-nums text-ink-3">
                      {count(row.requests)}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          </section>

          <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
            <CardHeader
              title={t('logs.usage.stats.token.title')}
              subtitle={t('logs.usage.stats.token.sub', {
                total: formatTokenAmount(inputOutputTotal, locale),
              })}
            />
            {split === null || split.total === 0 ? (
              <EmptyState title={t('logs.usage.stats.empty')} />
            ) : (
              <div className="flex flex-col gap-3 px-4 pb-4">
                <div
                  aria-hidden="true"
                  className="flex h-2 overflow-hidden rounded-full bg-surface-subtle"
                >
                  <span
                    className="bg-chart-cat-1"
                    style={{ width: `${String(split.promptPct)}%` }}
                  />
                  <span
                    className="bg-chart-cat-2"
                    style={{ width: `${String(split.completionPct)}%` }}
                  />
                  {split.cachePct > 0 ? (
                    <span
                      className="bg-chart-cat-3"
                      style={{ width: `${String(split.cachePct)}%` }}
                    />
                  ) : null}
                </div>
                <div className="flex flex-col">
                  <TokenLegendRow
                    name={t('logs.usage.stats.input_tokens')}
                    swatch="bg-chart-cat-1"
                    value={formatTokenAmount(
                      summary?.prompt_tokens_total ?? 0,
                      locale,
                    )}
                    percent={split.promptPct}
                  />
                  <TokenLegendRow
                    name={t('logs.usage.stats.output_tokens')}
                    swatch="bg-chart-cat-2"
                    value={formatTokenAmount(
                      summary?.completion_tokens_total ?? 0,
                      locale,
                    )}
                    percent={split.completionPct}
                  />
                  {split.cachePct > 0 ? (
                    <TokenLegendRow
                      name={t('logs.usage.stats.token.cache')}
                      swatch="bg-chart-cat-3"
                      value={formatTokenAmount(
                        summary?.cache_read_tokens_total ?? 0,
                        locale,
                      )}
                      percent={split.cachePct}
                    />
                  ) : null}
                </div>
                <p className="text-badge text-ink-3">
                  {split.inputToOutput === null
                    ? t('logs.usage.stats.token.note_no_ratio', {
                        avg: formatTokenAmount(split.avgPerRequest, locale),
                      })
                    : t('logs.usage.stats.token.note', {
                        avg: formatTokenAmount(split.avgPerRequest, locale),
                        ratio: split.inputToOutput,
                      })}
                </p>
              </div>
            )}
          </section>
        </div>
      </div>

      {/* Role-aware distribution grid. */}
      <div className="grid gap-3.5 lg:grid-cols-2 xl:grid-cols-[minmax(0,1.7fr)_minmax(0,1fr)_minmax(0,1fr)]">
        <div className="min-w-0 lg:col-span-2 xl:col-span-1">
          <DistributionPanel
            title={t('logs.usage.stats.by_model')}
            subtitle={t('logs.usage.stats.model.sub', { n: 8 })}
            tag={t('logs.usage.stats.model.tag', { count: modelItems.length })}
            items={modelItems}
            mode="full"
            emptyLabel={t('logs.usage.stats.empty')}
            pending={byModelQuery.isPending}
            error={
              byModelQuery.isError
                ? statsErrorText(byModelQuery.error, t)
                : null
            }
            retryLabel={retryLabel}
            onRetry={() => void byModelQuery.refetch()}
          />
        </div>

        {isAdmin ? (
          <>
            <DistributionPanel
              title={t('logs.usage.stats.group.title')}
              subtitle={t('logs.usage.stats.group.sub', {
                count: groupItems.length,
              })}
              items={groupItems}
              mode="compact"
              emptyLabel={t('logs.usage.stats.empty')}
              pending={byGroupQuery.isPending}
              error={
                byGroupQuery.isError
                  ? statsErrorText(byGroupQuery.error, t)
                  : null
              }
              retryLabel={retryLabel}
              onRetry={() => void byGroupQuery.refetch()}
            />
            <DistributionPanel
              title={t('logs.usage.stats.user.title')}
              subtitle={t('logs.usage.stats.user.sub', {
                count: userItems.length,
              })}
              items={userItems}
              mode="compact"
              emptyLabel={t('logs.usage.stats.empty')}
              pending={byUserQuery.isPending}
              error={
                byUserQuery.isError
                  ? statsErrorText(byUserQuery.error, t)
                  : null
              }
              retryLabel={retryLabel}
              onRetry={() => void byUserQuery.refetch()}
            />
          </>
        ) : (
          <>
            <DistributionPanel
              title={t('logs.usage.stats.token_key.title')}
              subtitle={t('logs.usage.stats.token_key.sub')}
              tag={t('logs.usage.stats.token_key.tag', {
                count: tokenItems.length,
              })}
              note={t('logs.usage.stats.token_key.note')}
              items={tokenItems}
              mode="compact"
              emptyLabel={t('logs.usage.stats.empty')}
              pending={byTokenQuery.isPending}
              error={
                byTokenQuery.isError
                  ? statsErrorText(byTokenQuery.error, t)
                  : null
              }
              retryLabel={retryLabel}
              onRetry={() => void byTokenQuery.refetch()}
            />
            <FailurePanel
              pending={
                summaryQuery.isPending ||
                (failuresEnabled && failuresQuery.isPending)
              }
              error={
                failuresEnabled && failuresQuery.isError
                  ? statsErrorText(failuresQuery.error, t)
                  : null
              }
              items={failuresQuery.data?.items ?? []}
              total={failuresQuery.data?.total ?? 0}
              onOpen={onOpenLog}
              onViewAll={onViewAllFailures}
              retryLabel={retryLabel}
              onRetry={() => void failuresQuery.refetch()}
            />
          </>
        )}
      </div>

      <p className="text-meta text-ink-3">
        {t('logs.usage.stats.source_note')}
      </p>
    </div>
  )
}

function MetricCard({
  label,
  value,
  unit,
  foot,
  title,
}: {
  label: string
  value: string
  unit?: string
  foot?: ReactNode
  title?: string
}) {
  return (
    <div
      title={title}
      className="flex min-w-0 flex-col gap-1 bg-surface-card px-4 py-3.5"
    >
      <p className="text-meta text-ink-2">{label}</p>
      <p className="flex items-baseline gap-1 text-stat text-ink-1 tabular-nums">
        <span>{value}</span>
        {unit !== undefined ? (
          <span className="text-meta font-medium text-ink-3">{unit}</span>
        ) : null}
      </p>
      {foot !== undefined ? (
        <div className="text-badge text-ink-3">{foot}</div>
      ) : null}
    </div>
  )
}

function CardHeader({
  title,
  subtitle,
  aside,
}: {
  title: string
  subtitle: string
  aside?: ReactNode
}) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3 p-4 pb-2">
      <div className="min-w-0">
        <h2 className="text-body font-semibold text-ink-1">{title}</h2>
        <p className="text-meta text-ink-3">{subtitle}</p>
      </div>
      {aside}
    </div>
  )
}

function RailRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border-hairline py-1.5 last:border-0">
      <span className="text-ink-3">{label}</span>
      <span className="font-mono tabular-nums text-ink-1">{value}</span>
    </div>
  )
}

function TokenLegendRow({
  name,
  swatch,
  value,
  percent,
}: {
  name: string
  swatch: string
  value: string
  percent: number
}) {
  const { locale } = useI18n()
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border-hairline py-1.5 text-meta last:border-0">
      <span className="inline-flex min-w-0 items-center gap-1.5 text-ink-2">
        <i aria-hidden="true" className={`size-2 rounded-sm ${swatch}`} />
        {name}
      </span>
      <span className="flex items-baseline gap-2">
        <b className="font-mono font-medium tabular-nums text-ink-1">
          {value}
        </b>
        <em className="font-mono text-badge not-italic tabular-nums text-ink-3">
          {new Intl.NumberFormat(locale, {
            minimumFractionDigits: 1,
            maximumFractionDigits: 1,
          }).format(percent)}
          %
        </em>
      </span>
    </div>
  )
}

function DistributionPanel({
  title,
  subtitle,
  tag,
  note,
  items,
  mode,
  emptyLabel,
  pending,
  error,
  retryLabel,
  onRetry,
}: {
  title: string
  subtitle: string
  tag?: string
  note?: string
  items: DistributionItem[]
  mode: 'full' | 'compact'
  emptyLabel: string
  pending: boolean
  error: string | null
  retryLabel: string
  onRetry: () => void
}): ReactNode {
  if (pending) {
    return (
      <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
        <CardHeader
          title={title}
          subtitle={subtitle}
          aside={<TagPill tag={tag} />}
        />
        <div
          data-testid="stats-section-loading"
          aria-busy="true"
          className="mx-4 mb-4 h-28 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
        />
      </section>
    )
  }
  if (error !== null) {
    return (
      <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
        <CardHeader
          title={title}
          subtitle={subtitle}
          aside={<TagPill tag={tag} />}
        />
        <InlineError message={error} onRetry={onRetry} retryLabel={retryLabel} />
      </section>
    )
  }
  return (
    <DistributionCard
      title={title}
      subtitle={subtitle}
      {...(tag !== undefined ? { tag } : {})}
      {...(note !== undefined ? { note } : {})}
      items={items}
      mode={mode}
      emptyLabel={emptyLabel}
    />
  )
}

function FailurePanel({
  pending,
  error,
  items,
  total,
  onOpen,
  onViewAll,
  retryLabel,
  onRetry,
}: {
  pending: boolean
  error: string | null
  items: NonNullable<
    components['schemas']['UsageLogPage']['items']
  >
  total: number
  onOpen: (logId: number) => void
  onViewAll: () => void
  retryLabel: string
  onRetry: () => void
}): ReactNode {
  const { t } = useI18n()
  if (pending) {
    return (
      <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
        <CardHeader
          title={t('logs.usage.stats.fail.title')}
          subtitle={t('logs.usage.stats.fail.sub')}
        />
        <div
          data-testid="stats-section-loading"
          aria-busy="true"
          className="mx-4 mb-4 h-28 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
        />
      </section>
    )
  }
  if (error !== null) {
    return (
      <section className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
        <CardHeader
          title={t('logs.usage.stats.fail.title')}
          subtitle={t('logs.usage.stats.fail.sub')}
        />
        <InlineError message={error} onRetry={onRetry} retryLabel={retryLabel} />
      </section>
    )
  }
  return (
    <RecentFailures
      items={items}
      total={total}
      onOpen={onOpen}
      onViewAll={onViewAll}
    />
  )
}

function TagPill({ tag }: { tag?: string }) {
  if (tag === undefined) return null
  return (
    <span className="shrink-0 rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 text-badge text-ink-2">
      {tag}
    </span>
  )
}
