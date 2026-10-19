import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatCredits, formatTokenAmount } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatAxis, type ChartSeries } from '@/shared/lib/chart-data'
import { AnalyticsChart } from '@/shared/ui/analytics-chart'

import {
  usageRangeParams,
  usageStatsGranularity,
  type UsageLogSearch,
} from '../search'
import { EmptyState, InlineError } from '../table-ui'
import { fillTimeseries } from './series'

type UsageStatsByModelItem = components['schemas']['UsageStatsByModelItem']
type UsageStatsRates = components['schemas']['UsageStatsRates']

type StatsQuery = NonNullable<
  operations['getUsageStatsByModel']['parameters']['query']
>
type TimeseriesQuery = NonNullable<
  operations['getUsageStatsTimeseries']['parameters']['query']
>
type RatesQuery = NonNullable<
  operations['getUsageStatsRates']['parameters']['query']
>

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
}

/**
 * F-05-03 stats board (UI §3.2). Four requests share one keyed filter set and
 * run concurrently; each section degrades on its own — a failed cards query
 * never blocks the trend table and every error keeps an inline retry.
 */
export function UsageStatsTab({
  search,
  canFilterByUsername,
  active,
  ready,
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
  // filter set. Keeping them makes the rates strip describe the same window as
  // the cards instead of silently widening to all models.
  const ratesQuery = useQuery({
    queryKey: ['usage-logs', 'stats', 'rates', query],
    enabled: enabled && summaryQuery.data !== undefined && summaryQuery.data.request_count > 0,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/rates', {
          params: { query: query as unknown as RatesQuery },
        }),
      ),
  })

  const summary = summaryQuery.data
  const summaryFailed = summaryQuery.isError
  const byModel = byModelQuery.data
  const trendRows = fillTimeseries(
    timeseriesQuery.data ?? [],
    search,
    granularity,
  )
  const trendSeries: ChartSeries[] =
    trendRows.length === 0
      ? []
      : [
          {
            id: 'cost',
            label: t('logs.usage.stats.trend_series'),
            points: trendRows.map((row) => ({
              ts: row.bucket,
              value: row.cost,
            })),
          },
        ]

  const spendTooltip =
    summary === undefined
      ? undefined
      : t('logs.usage.stats.spend_tooltip', {
          pre: formatCredits(summary.pre_deducted_total, locale),
          refunded: formatCredits(summary.refunded_total, locale),
        })

  const emptyHint =
    canFilterByUsername && search.username !== undefined
      ? t('logs.usage.stats.no_user_data')
      : t('logs.usage.stats.empty_hint')

  return (
    <div className="flex flex-col gap-4" data-testid="usage-stats-tab">
      {summaryFailed ? (
        <InlineError
          message={statsErrorText(summaryQuery.error, t)}
          onRetry={() => void summaryQuery.refetch()}
          retryLabel={t('logs.usage.error.retry')}
        />
      ) : null}

      <section
        aria-label={t('logs.usage.stats.cards_label')}
        className="grid grid-cols-2 gap-4 lg:grid-cols-4"
      >
        {summaryQuery.isPending ? (
          <div
            data-testid="stats-cards-loading"
            aria-busy="true"
            className="col-span-full grid grid-cols-2 gap-4 lg:grid-cols-4"
          >
            {[0, 1, 2, 3].map((index) => (
              <div
                key={index}
                className="h-24 animate-pulse rounded-xl bg-surface-hover motion-reduce:animate-none"
              />
            ))}
          </div>
        ) : (
          <>
            <StatCard
              label={t('logs.usage.stats.total_spent')}
              value={
                summary === undefined
                  ? '—'
                  : `${formatCredits(summary.cost_total, locale)} ${t('logs.usage.credits')}`
              }
              title={spendTooltip}
            />
            <StatCard
              label={t('logs.usage.stats.requests')}
              value={
                summary === undefined
                  ? '—'
                  : new Intl.NumberFormat(locale).format(summary.request_count)
              }
              hint={
                summary === undefined
                  ? undefined
                  : t('logs.usage.stats.success_rate', {
                      value: (summary.success_rate * 100).toFixed(1),
                    })
              }
            />
            <StatCard
              label={t('logs.usage.stats.input_tokens')}
              value={
                summary === undefined
                  ? '—'
                  : formatTokenAmount(summary.prompt_tokens_total, locale)
              }
            />
            <StatCard
              label={t('logs.usage.stats.output_tokens')}
              value={
                summary === undefined
                  ? '—'
                  : formatTokenAmount(summary.completion_tokens_total, locale)
              }
            />
          </>
        )}
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
        <h3 className="text-sm font-semibold text-ink-1">
          {t('logs.usage.stats.by_model')}
        </h3>
        {byModelQuery.isPending ? (
          <div
            data-testid="stats-model-loading"
            aria-busy="true"
            className="mt-4 h-40 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
          />
        ) : byModelQuery.isError ? (
          <div className="mt-4">
            <InlineError
              message={statsErrorText(byModelQuery.error, t)}
              onRetry={() => void byModelQuery.refetch()}
              retryLabel={t('logs.usage.error.retry')}
            />
          </div>
        ) : byModel === undefined || byModel.length === 0 ? (
          <EmptyState title={t('logs.usage.stats.empty')} hint={emptyHint} />
        ) : (
          <ModelDistributionTable items={byModel} />
        )}
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 className="text-sm font-semibold text-ink-1">
            {t('logs.usage.stats.trend')}
          </h3>
          <span className="rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 text-xs text-ink-2">
            {t(
              granularity === 'hour'
                ? 'logs.usage.stats.granularity_hour'
                : 'logs.usage.stats.granularity_day',
            )}
          </span>
        </div>
        {timeseriesQuery.isPending ? (
          <div
            data-testid="stats-trend-loading"
            aria-busy="true"
            className="mt-4 h-60 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
          />
        ) : timeseriesQuery.isError ? (
          <div className="mt-4">
            <InlineError
              message={statsErrorText(timeseriesQuery.error, t)}
              onRetry={() => void timeseriesQuery.refetch()}
              retryLabel={t('logs.usage.error.retry')}
            />
          </div>
        ) : trendRows.length === 0 ? (
          <EmptyState title={t('logs.usage.stats.empty')} hint={emptyHint} />
        ) : (
          <>
            <div className="mt-2">
              <AnalyticsChart
                series={trendSeries}
                config={{ kind: 'area', unit: t('logs.usage.credits') }}
              />
            </div>
            {/* Relief rule: every chart carries a table equivalent. The
                component's own sr-only table is the screen-reader path; this
                visible disclosure is the sighted-reader path (UI §3.8). */}
            <details className="mt-3">
              <summary className="cursor-pointer text-[13px] font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none">
                {t('logs.usage.stats.view_table')}
              </summary>
              <table
                data-testid="stats-trend-table"
                className="mt-2 w-full text-sm"
              >
                <caption className="sr-only">
                  {t('logs.usage.stats.trend_table_caption')}
                </caption>
                <thead>
                  <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                    <th scope="col" className="px-2 py-2 font-medium">
                      {t('logs.usage.stats.columns.time')}
                    </th>
                    <th scope="col" className="px-2 py-2 text-right font-medium">
                      {t('logs.usage.stats.columns.cost')}
                    </th>
                    <th scope="col" className="px-2 py-2 text-right font-medium">
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
                      <td className="px-2 py-1.5 font-mono text-[13px]">
                        {formatAxis(row.bucket, locale)}
                      </td>
                      <td className="px-2 py-1.5 text-right font-mono text-[13px] tabular-nums">
                        {formatCredits(row.cost, locale)}
                      </td>
                      <td className="px-2 py-1.5 text-right tabular-nums">
                        {new Intl.NumberFormat(locale).format(row.requests)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </details>
          </>
        )}
      </section>

      {/* P1 rate strip. Hidden when the window has no requests: there is no
          meaningful average to show and a row of zeros reads as a real
          measurement (UI §3.5 empty state). */}
      {ratesQuery.data !== undefined &&
      summary !== undefined &&
      summary.request_count > 0 ? (
        <RatesStrip rates={ratesQuery.data} />
      ) : null}
    </div>
  )
}

function StatCard({
  label,
  value,
  hint,
  title,
}: {
  label: string
  value: string
  hint?: string
  title?: string
}) {
  return (
    <div
      title={title}
      className="flex h-24 flex-col justify-center gap-1 rounded-xl border border-border-hairline bg-surface-card px-4"
    >
      <p className="text-[13px] text-ink-2">{label}</p>
      <p className="text-2xl font-semibold text-ink-1">{value}</p>
      {hint !== undefined ? (
        <p className="text-[13px] text-ink-3">{hint}</p>
      ) : null}
    </div>
  )
}

function ModelDistributionTable({ items }: { items: UsageStatsByModelItem[] }) {
  const { t, locale } = useI18n()
  // Locale-aware decimal separator; `toFixed` would print `50.0%` in fr/ru
  // where the interface otherwise uses a comma.
  const percentFormat = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  })
  return (
    <table
      data-testid="stats-model-table"
      className="mt-2 w-full text-sm"
    >
      <caption className="sr-only">{t('logs.usage.stats.by_model')}</caption>
      <thead>
        <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
          <th scope="col" className="px-2 py-2 font-medium">
            {t('logs.usage.stats.columns.model')}
          </th>
          <th scope="col" className="px-2 py-2 text-right font-medium">
            {t('logs.usage.stats.columns.cost')}
          </th>
          <th scope="col" className="px-2 py-2 text-right font-medium">
            {t('logs.usage.stats.columns.tokens')}
          </th>
          <th scope="col" className="px-2 py-2 text-right font-medium">
            {t('logs.usage.stats.columns.percent')}
          </th>
        </tr>
      </thead>
      <tbody>
        {items.map((item) => {
          const percent = Math.min(Math.max(item.percent, 0), 100)
          return (
            <tr
              key={item.model}
              data-testid="stats-model-row"
              className="border-b border-border-hairline last:border-0"
            >
              <th scope="row" className="px-2 py-2 font-normal">
                <span className="font-mono text-[13px]">
                  {item.model === 'other' ? t('logs.usage.stats.other') : item.model}
                </span>
                {/* The bar is decorative; the percentage column is the text
                    equivalent, so no information lives in colour or width. */}
                <span
                  aria-hidden="true"
                  className="mt-1 block h-1 w-full rounded bg-surface-subtle"
                >
                  <span
                    className="block h-1 rounded bg-brand"
                    style={{ width: `${percent}%` }}
                  />
                </span>
              </th>
              <td className="px-2 py-2 text-right font-mono text-[13px] tabular-nums">
                {formatCredits(item.cost, locale)}
              </td>
              <td className="px-2 py-2 text-right font-mono text-[13px] tabular-nums">
                {formatTokenAmount(item.tokens, locale)}
              </td>
              <td className="px-2 py-2 text-right tabular-nums">
                {percentFormat.format(percent)}%
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

function RatesStrip({ rates }: { rates: UsageStatsRates }) {
  const { t, locale } = useI18n()
  const formatRate = (value: number): string =>
    new Intl.NumberFormat(locale, {
      minimumFractionDigits: 0,
      maximumFractionDigits: 1,
    }).format(value)

  return (
    <section
      data-testid="stats-rates"
      aria-label={t('logs.usage.stats.rates.title')}
      className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-xl border border-border-hairline bg-surface-subtle px-4 py-2 text-[13px]"
    >
      <span className="font-medium text-ink-1">
        {t('logs.usage.stats.rates.title')}
      </span>
      <span className="text-ink-2">
        {t('logs.usage.stats.rates.rpm_avg')}{' '}
        <span className="font-mono tabular-nums text-ink-1">
          {formatRate(rates.rpm_avg)}
        </span>
      </span>
      <span className="text-ink-2">
        {t('logs.usage.stats.rates.rpm_peak')}{' '}
        <span className="font-mono tabular-nums text-ink-1">
          {formatRate(rates.rpm_peak)}
        </span>
      </span>
      <span className="text-ink-2">
        {t('logs.usage.stats.rates.tpm_avg')}{' '}
        <span className="font-mono tabular-nums text-ink-1">
          {formatRate(rates.tpm_avg)}
        </span>
      </span>
    </section>
  )
}
