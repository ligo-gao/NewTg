import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatCredits, formatTokenAmount } from '@/shared/lib/format'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'

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
import { TrendCard } from './trend-chart'

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
 * F-05-06 会话内敏感开关。OpenAPI 冻结稿尚未收录该参数，但后端已实现；
 * 缺省即打码，只有显式 1 才会请求原文（服务端仍重校验 log.prompt.view）。
 * The list page already carries the same narrow extension in its own
 * `UsageLogsQuery`; the contract gap is a separate follow-up.
 */
type FailuresQuery = ListQuery & { reveal_sensitive?: 0 | 1 }

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

function statsErrorText(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'offline') return t('logs.usage.error.offline')
    if (error.kind === 'validation') return t('logs.usage.error.invalid_range')
    // The stats tab fires several requests on entry, so the limiter is a
    // reachable state. The generic retry copy would just invite another 429;
    // name the condition instead (same key the list uses).
    if (error.kind === 'rate_limited') {
      return t('logs.usage.error.too_many', {
        seconds: error.retry_after ?? 60,
      })
    }
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
  /** F-05-06 reveal state owned by the page; only the failures list uses it. */
  revealSensitive: boolean
  /** `log.prompt.view`: without it the server always masks error text. */
  canViewSensitive: boolean
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
  revealSensitive,
  canViewSensitive,
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
  const failureQuery: FailuresQuery = {
    ...usageRangeParams(search),
    status: 'error',
    p: 1,
    page_size: 10,
    ...(search.model !== undefined ? { model: search.model } : {}),
    ...(search.group !== undefined ? { group: search.group } : {}),
    ...(search.token_id !== undefined ? { token_id: search.token_id } : {}),
    // Without the flag the backend masks `error_message`; the card falls back
    // to `error_class`, so a viewer who may see the text should ask for it.
    ...(revealSensitive && canViewSensitive ? { reveal_sensitive: 1 } : {}),
  }
  const failureCountInSummary =
    summaryQuery.data === undefined
      ? null
      : summaryQuery.data.error_count ?? 0
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
  const activeBuckets = topActiveBuckets(trendRows, 5)

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

  const isHour = granularity === 'hour'
  // The activity card ranks the busiest buckets by the spend they produced;
  // whatever the top five leave out is summarised in the card's note.
  const activeItems: DistributionItem[] = activeBuckets.map((row) => ({
    key: row.bucket,
    label: formatBucketTick(row.bucket, granularity, locale),
    cost: row.cost,
    requests: row.requests,
    tokens: 0,
    percent: insights.totalCost > 0 ? (row.cost / insights.totalCost) * 100 : 0,
  }))
  const restActiveBuckets = insights.activeBuckets - activeBuckets.length
  const activeNote =
    restActiveBuckets > 0
      ? t(
          isHour
            ? 'logs.usage.stats.active.note_hour'
            : 'logs.usage.stats.active.note_day',
          {
            count: count(restActiveBuckets),
            percent: oneDecimal(
              100 - activeItems.reduce((sum, item) => sum + item.percent, 0),
            ),
          },
        )
      : t('logs.usage.stats.active.note_all')

  const spendTooltip =
    summary === undefined
      ? undefined
      : t('logs.usage.stats.spend_tooltip', {
          pre: formatCredits(summary.pre_deducted_total, locale),
          refunded: formatCredits(summary.refunded_total, locale),
        })

  // The failure card and its destination are `status=error` only, so the
  // printed count must not fold in refusals: refusals stay visible in the
  // status counts above and in the token card's quota note.
  const failureCount = summary === undefined ? 0 : summary.error_count ?? 0
  const retryLabel = t('logs.usage.error.retry')

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
        ? item.token_id === 0
          ? t('logs.usage.stats.token_key.unassigned')
          : `#${String(item.token_id)}`
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
                : `${oneDecimal((summary.success_rate ?? 0) * 100)}%`
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

      {/* Two independent trend cards: one measure per card, each with its own
          scale. RPM rides with requests and TPM with the token mix, so neither
          unit has to share a card with a measure it does not belong to. */}
      {timeseriesQuery.isError ? (
        <InlineError
          message={statsErrorText(timeseriesQuery.error, t)}
          onRetry={() => void timeseriesQuery.refetch()}
          retryLabel={retryLabel}
        />
      ) : (
        <div className="grid gap-3.5 xl:grid-cols-2">
          <TrendCard
            metric="cost"
            rows={trendRows}
            granularity={granularity}
            loading={timeseriesQuery.isPending}
            emptyLabel={t('logs.usage.stats.empty')}
          />
          <TrendCard
            metric="requests"
            rows={trendRows}
            granularity={granularity}
            loading={timeseriesQuery.isPending}
            emptyLabel={t('logs.usage.stats.empty')}
            rpmPeak={ratesQuery.data?.rpm_peak}
          />
        </div>
      )}

      {ratesQuery.isError ? (
        <div data-testid="stats-rates">
          <InlineError
            message={statsErrorText(ratesQuery.error, t)}
            onRetry={() => void ratesQuery.refetch()}
            retryLabel={retryLabel}
          />
        </div>
      ) : null}

      {/* Where the spend went: token mix beside the model mix. */}
      <div className="grid gap-3.5 xl:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
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
            <div className="flex flex-1 flex-col gap-3 px-4 pb-3">
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
              <p className="mt-auto border-t border-border-hairline pt-2.5 text-badge text-ink-3">
                {split.inputToOutput === null
                  ? t('logs.usage.stats.token.note_no_ratio', {
                      avg: formatTokenAmount(split.avgPerRequest, locale),
                    })
                  : t('logs.usage.stats.token.note', {
                      avg: formatTokenAmount(split.avgPerRequest, locale),
                      ratio: split.inputToOutput,
                    })}
                {ratesQuery.data === undefined ? null : (
                  <>
                    {' · '}
                    {t('logs.usage.stats.token.tpm', {
                      value: compact(ratesQuery.data.tpm_avg),
                    })}
                  </>
                )}
              </p>
            </div>
          )}
        </section>

        <div className="min-w-0">
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
      </div>

      {/* Ranked row: role-aware, one shared height. A long ranking scrolls
          inside its own card; the others keep the same height. */}
      <div className="grid gap-3.5 lg:grid-cols-2 xl:grid-cols-3">
        {isAdmin ? (
          <>
            <DistributionPanel
              title={t('logs.usage.stats.group.title')}
              subtitle={t('logs.usage.stats.group.sub', {
                count: groupItems.length,
              })}
              items={groupItems}
              mode="compact"
              fixed
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
              fixed
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
          <DistributionPanel
            title={t('logs.usage.stats.token_key.title')}
            subtitle={t('logs.usage.stats.token_key.sub')}
            tag={t('logs.usage.stats.token_key.tag', {
              count: tokenItems.length,
            })}
            note={t('logs.usage.stats.token_key.note')}
            items={tokenItems}
            mode="compact"
            fixed
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
        )}

        <DistributionPanel
          title={t(
            isHour
              ? 'logs.usage.stats.active.title_hour'
              : 'logs.usage.stats.active.title_day',
          )}
          subtitle={t(
            isHour
              ? 'logs.usage.stats.active.sub_hour'
              : 'logs.usage.stats.active.sub_day',
            { count: count(insights.activeBuckets) },
          )}
          items={activeItems}
          mode="compact"
          note={activeNote}
          fixed
          emptyLabel={t('logs.usage.stats.empty')}
          pending={timeseriesQuery.isPending}
          error={
            timeseriesQuery.isError
              ? statsErrorText(timeseriesQuery.error, t)
              : null
          }
          retryLabel={retryLabel}
          onRetry={() => void timeseriesQuery.refetch()}
        />

        {isAdmin ? null : (
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
  fixed = false,
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
  /** Ranked-row card: one shared height, the ranking scrolls inside. */
  fixed?: boolean
}): ReactNode {
  if (pending) {
    return (
      <section
        className={`flex min-w-0 flex-col overflow-hidden rounded-xl border border-border-hairline bg-surface-card ${
          fixed ? 'h-80' : ''
        }`}
      >
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
      <section
        className={`flex min-w-0 flex-col overflow-hidden rounded-xl border border-border-hairline bg-surface-card ${
          fixed ? 'h-80' : ''
        }`}
      >
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
      fixed={fixed}
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
      <section className="flex h-80 min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
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
      <section className="flex h-80 min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card">
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
