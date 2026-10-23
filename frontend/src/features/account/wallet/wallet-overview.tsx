import { useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { PLANS_KEY } from '@/features/account/subscription/subscription-section'
import { bucketSeries, type ChartSeries } from '@/shared/lib/chart-data'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { fillBucketSpine } from '@/shared/lib/timeseries-spine'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'

import { centsToCredits, formatYuan } from '../topup/money'
import { topupConfigQueryOptions, walletSummaryQueryOptions } from '../topup/queries'
import { BalanceCard } from '../topup/balance-card'
import type { TopupSelection } from '../topup/topup-page'
import {
  WALLET_STATUS_BADGE_VARIANTS,
  WALLET_TYPE_TAG_VARIANTS,
  statusLabelKey,
  typeLabelKey,
} from './labels'
import {
  overviewSubscriptionsQueryOptions,
  recentTransactionsQueryOptions,
  walletUsageSummaryQueryOptions,
  walletUsageTimeseriesQueryOptions,
  type WalletUsageRange,
} from './overview-queries'
import type { WalletTab } from './search'
import type { WalletTransaction } from './types'
import { WalletSpendChart } from './wallet-spend-chart'

export interface WalletOverviewProps {
  /** Switches the page-level tab (subscription / transactions). */
  onSelectTab: (tab: WalletTab) => void
  /**
   * Opens the top-up tab, carrying the amount picked here. Called by the hero
   * and quick top-up actions; the header's plain "Top up" omits it so the
   * top-up tab seeds from the server config.
   */
  onTopup: (selection?: TopupSelection) => void
}

const RANGE_DAYS: Record<WalletUsageRange, number> = { '7d': 7, '30d': 30 }

function amountText(transaction: WalletTransaction, locale: string): string {
  const sign = transaction.amount < 0 ? '−' : '+'
  return `${sign}${formatCredits(Math.abs(transaction.amount), locale)}`
}

function formatDate(iso: string, locale: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat(locale, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  }).format(date)
}

/**
 * Ledger timestamps are absolute local time ("10-23 14:32") so a row keeps its
 * meaning when read later; the title carries the exact ISO instant.
 */
function formatLedgerTime(iso: string, locale: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat(locale, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date)
}

function isoTitle(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? iso : date.toISOString()
}

/**
 * Wallet overview (default `/wallet` tab).
 *
 * Answers "how much do I have, where did it go, and what came in" in one
 * screen. Every block owns its loading/error state; the top-up and
 * subscription tabs remain the place for the full write flows.
 */
export function WalletOverview({ onSelectTab, onTopup }: WalletOverviewProps) {
  const { t, locale } = useI18n()
  const [range, setRange] = useState<WalletUsageRange>('7d')
  const [selectedCents, setSelectedCents] = useState<number | null>(null)

  const summaryQuery = useQuery(walletSummaryQueryOptions())
  const recentQuery = useQuery(recentTransactionsQueryOptions())
  const usageSummaryQuery = useQuery(walletUsageSummaryQueryOptions(range))
  const timeseriesQuery = useQuery(walletUsageTimeseriesQueryOptions(range))
  const configQuery = useQuery(topupConfigQueryOptions())
  const subscriptionsQuery = useQuery(overviewSubscriptionsQueryOptions())
  const plansQuery = useQuery({
    queryKey: PLANS_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/subscription/plans')),
  })

  const summary = summaryQuery.data
  const recent = recentQuery.data?.items ?? []
  const usage = usageSummaryQuery.data
  const config = configQuery.data

  // The server reports `depleted` for every zero balance, including accounts
  // that never topped up. Distinguish the two only once both reads have
  // settled: a zero balance with nothing held and no lifetime activity is
  // "empty". Ledger rows alone cannot prove history — the recent-transactions
  // endpoint defaults to a 30-day window, so a user who spent down months ago
  // would look brand new. The wallet summary carries lifetime counters
  // (`total_consumed`, `total_requests`), which is the durable signal. Until
  // both reads settle keep the server health (or undefined), so the loading
  // state never flashes "No balance yet".
  const lifetimeActivity =
    (summary?.total_consumed ?? 0) > 0 || (summary?.total_requests ?? 0) > 0
  const healthSettled = summaryQuery.isSuccess && recentQuery.isSuccess
  const balanceHealth =
    healthSettled &&
    summary?.balance === 0 &&
    (summary?.held_credits ?? 0) === 0 &&
    recent.length === 0 &&
    !lifetimeActivity
      ? 'empty'
      : summary?.health

  const activeSubscription = useMemo(
    () =>
      (subscriptionsQuery.data?.items ?? []).find(
        (item) => item.status === 'active',
      ),
    [subscriptionsQuery.data],
  )

  const series: ChartSeries[] = useMemo(() => {
    const points = (timeseriesQuery.data ?? []).flatMap((item) => {
      const ms = Date.parse(item.bucket)
      if (Number.isNaN(ms) || !Number.isFinite(item.cost)) return []
      return [{ ts: new Date(ms).toISOString(), value: item.cost }]
    })
    if (points.length === 0) return []
    return bucketSeries(
      [
        {
          id: 'spend',
          label: t('account.wallet.overview.trend_spend'),
          points: fillBucketSpine(points, range, 'day'),
        },
      ],
      'day',
    )
  }, [timeseriesQuery.data, range, t])

  const presets = config?.amount_options.slice(0, 4) ?? []
  const quickCents = selectedCents ?? presets[1] ?? presets[0] ?? null
  const quickCredits =
    quickCents === null || config === undefined
      ? null
      : centsToCredits(quickCents, config.quota_per_unit, config.credits_ratio)
  const dailySpend =
    usage === undefined ? null : Math.round(usage.cost_total / RANGE_DAYS[range])
  const chartAriaLabel = `${t('account.wallet.overview.trend_title')} · ${t(
    range === '7d'
      ? 'account.wallet.overview.range_7d'
      : 'account.wallet.overview.range_30d',
  )}`

  const subscriptionRemaining =
    activeSubscription === undefined
      ? null
      : activeSubscription.amount_total === 0
        ? null
        : (activeSubscription.amount_remain ??
          Math.max(
            0,
            activeSubscription.amount_total - activeSubscription.amount_used,
          ))
  const subscriptionPercent =
    activeSubscription === undefined || activeSubscription.amount_total === 0
      ? null
      : Math.min(
          100,
          Math.round(
            ((subscriptionRemaining ?? 0) / activeSubscription.amount_total) *
              100,
          ),
        )
  // The subscription tab renders nothing when the product has no plans, so
  // Manage would land on an empty page. Only a successful empty read hides it.
  const subscriptionOffered = !(
    plansQuery.isSuccess && (plansQuery.data?.items.length ?? 0) === 0
  )

  return (
    <div className="flex flex-col gap-3.5">
      {summaryQuery.isError ? (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-sm text-danger"
        >
          <span>{t('account.wallet.overview.summary_error')}</span>
          <Button
            variant="secondary"
            className="h-8 px-3 text-xs"
            onClick={() => void summaryQuery.refetch()}
          >
            {t('account.wallet.retry')}
          </Button>
        </div>
      ) : null}

      <div
        className="grid grid-cols-1 gap-3.5 min-[721px]:grid-cols-2 min-[1181px]:grid-cols-[1.5fr_repeat(3,minmax(0,1fr))]"
        data-testid="wallet-hero"
      >
        <BalanceCard
          balance={summary?.balance ?? null}
          health={balanceHealth}
          heldCredits={summary?.held_credits}
          threshold={summary?.threshold}
          quotaPerUnit={config?.quota_per_unit}
          actions={
            <>
              <Button
                variant="primary"
                className="h-8 px-3 text-xs"
                aria-label={t('account.wallet.overview.action_topup_hero')}
                onClick={() =>
                  onTopup({ cents: quickCents, custom: false })
                }
              >
                {t('account.wallet.overview.action_topup')}
              </Button>
              <Button
                variant="secondary"
                className="h-8 px-3 text-xs"
                aria-label={t('account.wallet.overview.action_redeem_hero')}
                onClick={() => onSelectTab('topup')}
              >
                {t('account.wallet.overview.action_redeem')}
              </Button>
            </>
          }
        />

        <OverviewStat
          label={t('account.wallet.overview.stat_consumed')}
          value={
            summary === undefined
              ? '—'
              : formatCredits(summary.total_consumed, locale)
          }
          foot={
            <UsageFoot
              isError={usageSummaryQuery.isError}
              onRetry={() => void usageSummaryQuery.refetch()}
            >
              {usage === undefined
                ? t('account.wallet.overview.stat_consumed_pending')
                : t('account.wallet.overview.stat_consumed_foot', {
                    spend: formatCredits(usage.cost_total, locale),
                  })}
            </UsageFoot>
          }
        />
        <OverviewStat
          label={t('account.wallet.overview.stat_requests')}
          value={
            summary === undefined
              ? '—'
              : formatCredits(summary.total_requests, locale)
          }
          foot={
            <UsageFoot
              isError={usageSummaryQuery.isError}
              onRetry={() => void usageSummaryQuery.refetch()}
            >
              {usage === undefined
                ? t('account.wallet.overview.stat_requests_foot_pending')
                : t('account.wallet.overview.stat_requests_foot', {
                    rate: `${(usage.success_rate * 100).toFixed(1)}%`,
                  })}
            </UsageFoot>
          }
        />
        <OverviewStat
          label={t('account.wallet.overview.stat_held')}
          value={
            summary === undefined
              ? '—'
              : formatCredits(summary.held_credits, locale)
          }
          foot={t('account.wallet.overview.stat_held_foot')}
        />
      </div>

      <div
        className="grid grid-cols-1 gap-3.5 min-[1181px]:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]"
        data-testid="wallet-grid"
      >
        <div className="flex min-w-0 flex-col gap-3.5">
          <section className="rounded-xl border border-border-hairline bg-surface-card">
            <div className="flex flex-wrap items-center justify-between gap-2 p-5 pb-3">
              <div>
                <h2 className="text-[15px] font-semibold text-ink-1">
                  {t('account.wallet.overview.trend_title')}
                </h2>
                <p className="mt-0.5 text-xs text-ink-2">
                  {t('account.wallet.overview.trend_sub')}
                </p>
              </div>
              <div
                role="group"
                aria-label={t('account.wallet.overview.trend_title')}
                className="flex overflow-hidden rounded-lg border border-border-default bg-surface-card"
              >
                {(['7d', '30d'] as const).map((option) => (
                  <button
                    key={option}
                    type="button"
                    aria-pressed={range === option}
                    onClick={() => setRange(option)}
                    className={[
                      'h-8 px-3 text-xs font-medium transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                      option === '7d'
                        ? 'border-r border-border-hairline'
                        : '',
                      range === option
                        ? 'bg-brand-subtle text-brand'
                        : 'text-ink-2 hover:text-ink-1',
                    ].join(' ')}
                  >
                    {t(
                      option === '7d'
                        ? 'account.wallet.overview.range_7d'
                        : 'account.wallet.overview.range_30d',
                    )}
                  </button>
                ))}
              </div>
            </div>

            <div className="flex flex-wrap gap-7 px-5">
              <Metric
                label={t('account.wallet.overview.trend_spend')}
                value={
                  usage === undefined
                    ? '—'
                    : formatCredits(usage.cost_total, locale)
                }
              />
              <Metric
                label={t('account.wallet.overview.trend_requests')}
                value={
                  usage === undefined
                    ? '—'
                    : formatCredits(usage.request_count, locale)
                }
              />
              <Metric
                label={t('account.wallet.overview.trend_daily')}
                value={
                  dailySpend === null ? '—' : formatCredits(dailySpend, locale)
                }
              />
            </div>

            {usageSummaryQuery.isError ? (
              <div className="mt-1.5 flex items-center gap-2 px-5 text-xs text-ink-2">
                <span>{t('account.wallet.overview.stat_usage_error')}</span>
                <button
                  type="button"
                  onClick={() => void usageSummaryQuery.refetch()}
                  className="font-medium text-brand underline underline-offset-2 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                >
                  {t('account.wallet.retry')}
                </button>
              </div>
            ) : null}

            <div className="px-3.5 pb-3.5 pt-2">
              {timeseriesQuery.isPending ? (
                <div
                  data-testid="wallet-trend-loading"
                  role="status"
                  aria-busy="true"
                  aria-label={t('account.wallet.overview.loading')}
                  className="h-[220px] animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
                />
              ) : timeseriesQuery.isError ? (
                <div className="flex min-h-[220px] flex-col items-center justify-center gap-2 text-sm text-ink-2">
                  <p>{t('account.wallet.overview.trend_error')}</p>
                  <Button
                    variant="secondary"
                    className="h-8 px-3 text-xs"
                    onClick={() => {
                      // The spend/request metrics come from the sibling query;
                      // retrying the trend should recover both.
                      void usageSummaryQuery.refetch()
                      void timeseriesQuery.refetch()
                    }}
                  >
                    {t('account.wallet.retry')}
                  </Button>
                </div>
              ) : series.length === 0 ? (
                <div className="flex min-h-[220px] items-center justify-center rounded-lg border border-dashed border-neutral-line text-sm text-neutral">
                  {t('account.wallet.overview.trend_empty')}
                </div>
              ) : (
                <div data-testid="wallet-spend-chart">
                  <WalletSpendChart
                    series={series}
                    ariaLabel={chartAriaLabel}
                    unit={t('common.dashboard.rates.credits')}
                  />
                </div>
              )}
            </div>
          </section>

          <section className="rounded-xl border border-border-hairline bg-surface-card">
            <div className="flex flex-wrap items-center justify-between gap-2 p-5 pb-3">
              <div>
                <h2 className="text-[15px] font-semibold text-ink-1">
                  {t('account.wallet.overview.recent_title')}
                </h2>
                <p className="mt-0.5 text-xs text-ink-2">
                  {t('account.wallet.overview.recent_sub')}
                </p>
              </div>
              <Button
                variant="ghost"
                className="text-[13px] text-brand"
                onClick={() => onSelectTab('transactions')}
              >
                {t('account.wallet.overview.recent_view_all')}
              </Button>
            </div>

            {recentQuery.isPending ? (
              <div
                role="status"
                aria-busy="true"
                aria-label={t('account.wallet.overview.loading')}
                className="flex flex-col gap-2 p-5 pt-0"
              >
                {[0, 1, 2, 3, 4, 5].map((row) => (
                  <div
                    key={row}
                    className="h-10 animate-pulse rounded-lg bg-surface-subtle motion-reduce:animate-none"
                  />
                ))}
              </div>
            ) : recentQuery.isError ? (
              <div className="flex items-center gap-3 p-5 pt-0 text-sm text-ink-2">
                <span>{t('account.wallet.error')}</span>
                <Button
                  variant="secondary"
                  className="h-8 px-3 text-xs"
                  onClick={() => void recentQuery.refetch()}
                >
                  {t('account.wallet.retry')}
                </Button>
              </div>
            ) : recent.length === 0 ? (
              <p className="p-5 pt-0 text-sm text-ink-2">
                {t('account.wallet.overview.ledger_empty')}
              </p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full border-collapse text-sm">
                  <caption className="sr-only">
                    {t('account.wallet.overview.recent_sub')}
                  </caption>
                  <thead>
                    <tr className="bg-surface-subtle text-xs text-ink-2">
                      <th scope="col" className="h-9 px-4 text-left font-medium">
                        {t('account.wallet.column_time')}
                      </th>
                      <th scope="col" className="h-9 px-4 text-left font-medium">
                        {t('account.wallet.column_type')}
                      </th>
                      <th scope="col" className="h-9 px-4 text-left font-medium">
                        {t('account.wallet.overview.column_description')}
                      </th>
                      <th scope="col" className="h-9 px-4 text-right font-medium">
                        {t('account.wallet.column_amount')}
                      </th>
                      <th scope="col" className="h-9 px-4 text-left font-medium">
                        {t('account.wallet.column_status')}
                      </th>
                      <th scope="col" className="h-9 px-4 text-right font-medium">
                        {t('account.wallet.column_balance')}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {recent.map((transaction) => {
                      const reference = transaction.reference_id ?? '—'
                      return (
                        <tr
                          key={transaction.id}
                          className="h-11 border-b border-border-hairline transition-colors last:border-0 hover:bg-surface-hover focus-within:bg-surface-hover"
                        >
                          <td className="whitespace-nowrap px-4 py-2 text-ink-2">
                            <time
                              dateTime={transaction.created_at}
                              title={isoTitle(transaction.created_at)}
                            >
                              {formatLedgerTime(transaction.created_at, locale)}
                            </time>
                          </td>
                          <td className="px-4 py-2">
                            <Tag
                              variant={
                                WALLET_TYPE_TAG_VARIANTS[transaction.type]
                              }
                            >
                              {t(typeLabelKey(transaction.type))}
                            </Tag>
                          </td>
                          <td className="px-4 py-2 text-ink-2">
                            <span
                              className="block max-w-[180px] truncate"
                              title={reference}
                            >
                              {reference}
                            </span>
                          </td>
                          <td
                            className={[
                              'px-4 py-2 text-right font-semibold tabular-nums',
                              transaction.amount < 0
                                ? 'text-amount-out'
                                : 'text-amount-in',
                            ].join(' ')}
                          >
                            {amountText(transaction, locale)}
                          </td>
                          <td className="px-4 py-2">
                            <StatusBadge
                              variant={
                                WALLET_STATUS_BADGE_VARIANTS[transaction.status]
                              }
                              className={
                                transaction.status === 'hold'
                                  ? 'motion-safe:animate-pulse'
                                  : undefined
                              }
                            >
                              {t(statusLabelKey(transaction.status))}
                            </StatusBadge>
                          </td>
                          <td className="px-4 py-2 text-right tabular-nums text-ink-2">
                            {formatCredits(transaction.balance_after, locale)}
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </div>

        <aside className="flex min-w-0 flex-col gap-3.5">
          <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
            <h2 className="text-[15px] font-semibold text-ink-1">
              {t('account.wallet.overview.quick_title')}
            </h2>
            <p className="mt-0.5 text-xs text-ink-2">
              {t('account.wallet.overview.quick_sub')}
            </p>

            {configQuery.isSuccess ? (
              <>
                <div
                  role="group"
                  aria-label={t('account.wallet.overview.quick_title')}
                  className="mt-3.5 flex flex-wrap gap-2"
                >
                  {presets.map((option) => (
                    <button
                      key={option}
                      type="button"
                      aria-pressed={quickCents === option}
                      onClick={() => setSelectedCents(option)}
                      className={[
                        'h-9 min-w-16 rounded-lg border px-3.5 text-[13px] transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                        quickCents === option
                          ? 'border-brand bg-brand-subtle font-semibold text-brand'
                          : 'border-border-default bg-surface-card text-ink-1 hover:border-brand-line',
                      ].join(' ')}
                    >
                      {formatYuan(option, { decimals: false, locale })}
                    </button>
                  ))}
                  <button
                    type="button"
                    onClick={() =>
                      onTopup({ cents: null, custom: true })
                    }
                    className="h-9 min-w-16 rounded-lg border border-border-default bg-surface-card px-3.5 text-[13px] text-ink-1 hover:border-brand-line focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  >
                    {t('account.wallet.overview.quick_custom')}
                  </button>
                </div>
                <p className="mt-3 rounded-lg bg-surface-subtle px-3 py-2.5 text-xs leading-[18px] text-ink-2">
                  {quickCredits === null
                    ? t('account.wallet.overview.quick_preview_empty')
                    : t('account.wallet.overview.quick_preview', {
                        credits: formatCredits(quickCredits, locale),
                      })}
                </p>
                <Button
                  variant="primary"
                  className="mt-3 w-full"
                  aria-label={t('account.wallet.overview.quick_cta_aria')}
                  onClick={() =>
                    onTopup({ cents: quickCents, custom: false })
                  }
                >
                  {t('account.wallet.overview.quick_cta')}
                </Button>
                <a
                  href="/orders"
                  className="mt-2.5 flex items-center justify-between rounded-sm text-[12.5px] text-ink-2 transition-colors hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                >
                  {t('account.wallet.overview.view_orders')}
                  <span aria-hidden="true">→</span>
                </a>
              </>
            ) : configQuery.isError ? (
              <div className="mt-3 flex items-center gap-3 text-sm text-ink-2">
                <span>{t('account.topup.config_error')}</span>
                <Button
                  variant="secondary"
                  className="h-8 px-3 text-xs"
                  onClick={() => void configQuery.refetch()}
                >
                  {t('account.topup.retry')}
                </Button>
              </div>
            ) : (
              <div
                role="status"
                aria-busy="true"
                aria-label={t('account.wallet.overview.loading')}
                className="mt-3 h-[215px] animate-pulse rounded-lg bg-surface-subtle motion-reduce:animate-none"
              />
            )}
          </section>

          <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
            <h2 className="text-[15px] font-semibold text-ink-1">
              {t('account.wallet.overview.sub_title')}
            </h2>

            {subscriptionsQuery.isPending ? (
              <div
                role="status"
                aria-busy="true"
                aria-label={t('account.wallet.overview.loading')}
                className="mt-3 h-[150px] animate-pulse rounded-lg bg-surface-subtle motion-reduce:animate-none"
              />
            ) : subscriptionsQuery.isError ? (
              <div className="mt-3 flex flex-wrap items-center gap-3 text-sm text-ink-2">
                <span>{t('account.wallet.overview.sub_error')}</span>
                <Button
                  variant="secondary"
                  className="h-8 px-3 text-xs"
                  onClick={() => void subscriptionsQuery.refetch()}
                >
                  {t('account.wallet.retry')}
                </Button>
              </div>
            ) : activeSubscription === undefined ? (
              <p className="mt-2 text-[13px] text-ink-2">
                {t('account.wallet.overview.sub_none')}
              </p>
            ) : (
              <>
                <p className="mt-2 text-[15px] font-semibold text-ink-1">
                  {activeSubscription.plan_title_snapshot}
                </p>
                <p className="mt-0.5 text-xs text-ink-2">
                  {t('account.wallet.overview.sub_until', {
                    date: formatDate(activeSubscription.end_at, locale),
                  })}
                </p>
                <p className="mt-2.5 text-xs text-ink-2">
                  {activeSubscription.amount_total === 0
                    ? t('account.wallet.overview.sub_unlimited')
                    : t('account.wallet.overview.sub_remaining', {
                        remaining: formatCredits(
                          subscriptionRemaining ?? 0,
                          locale,
                        ),
                        total: formatCredits(
                          activeSubscription.amount_total,
                          locale,
                        ),
                      })}
                </p>
                {subscriptionPercent !== null ? (
                  <div
                    aria-hidden="true"
                    className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface-subtle"
                  >
                    <div
                      className="h-full rounded-full bg-brand"
                      style={{ width: `${subscriptionPercent}%` }}
                    />
                  </div>
                ) : null}
              </>
            )}

            {subscriptionOffered ? (
              <Button
                variant="secondary"
                className="mt-3 w-full"
                onClick={() => onSelectTab('subscription')}
              >
                {t('account.wallet.overview.sub_manage')}
              </Button>
            ) : null}
          </section>

          <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
            <h2 className="text-[15px] font-semibold text-ink-1">
              {t('account.wallet.overview.bills_title')}
            </h2>
            <div className="mt-2 flex flex-col">
              <a
                href="/orders"
                className="flex items-center justify-between rounded-sm border-b border-border-hairline py-2 text-[12.5px] text-ink-2 transition-colors hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('account.wallet.overview.bills_orders')}
                <span aria-hidden="true">→</span>
              </a>
              <button
                type="button"
                onClick={() => onSelectTab('transactions')}
                className="flex items-center justify-between border-b border-border-hairline py-2 text-left text-[12.5px] text-ink-2 hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('account.wallet.overview.bills_ledger')}
                <span aria-hidden="true">→</span>
              </button>
              <a
                href="/dashboard/usage-logs"
                className="flex items-center justify-between rounded-sm py-2 text-[12.5px] text-ink-2 transition-colors hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('account.wallet.overview.bills_logs')}
                <span aria-hidden="true">→</span>
              </a>
            </div>
          </section>
        </aside>
      </div>
    </div>
  )
}

function OverviewStat({
  label,
  value,
  foot,
}: {
  label: string
  value: string
  foot: ReactNode
}) {
  return (
    <div className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <div className="text-xs leading-[18px] text-ink-2">{label}</div>
      <div className="mt-1.5 text-2xl font-semibold tabular-nums text-ink-1">
        {value}
      </div>
      <div className="mt-1.5 text-xs leading-[18px] text-ink-2">{foot}</div>
    </div>
  )
}

/** Stat footer that turns "loading" into an explicit error + retry. */
function UsageFoot({
  isError,
  onRetry,
  children,
}: {
  isError: boolean
  onRetry: () => void
  children: ReactNode
}) {
  const { t } = useI18n()
  if (!isError) return <>{children}</>
  return (
    <span>
      {t('account.wallet.overview.stat_usage_error')}{' '}
      <button
        type="button"
        onClick={onRetry}
        className="font-medium text-brand underline underline-offset-2 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('account.wallet.retry')}
      </button>
    </span>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-[11px] leading-4 text-ink-2">{label}</span>
      <b className="text-[17px] font-semibold leading-6 tabular-nums text-ink-1">
        {value}
      </b>
    </div>
  )
}
