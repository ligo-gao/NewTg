import type { components } from '@/api/schema'
import { smoothPath } from '@/shared/lib/chart-path'
import { useI18n } from '@/shared/lib/i18n-context'

import type {
  UsageStatsRates,
  UsageStatsSummary,
  UsageStatsTimeseriesItem,
} from './api'
import {
  UNAVAILABLE,
  formatCreditsValue,
  formatCurrencyValue,
  formatInteger,
  formatRpm,
  formatSuccessRate,
  formatTokensValue,
} from './format'

type WalletSummary = components['schemas']['WalletSummary']

export interface CoreMetricsProps {
  role: 'admin' | 'user'
  summary: UsageStatsSummary | undefined
  rates: UsageStatsRates | undefined
  wallet: WalletSummary | undefined
  timeseries: UsageStatsTimeseriesItem[] | undefined
  loading: boolean
  error: boolean
  showCurrency: boolean
  quotaPerUnit: number
  currencySymbol: string
  locale: string
  onRetry: () => void
}

function formatLatency(value: number | undefined): string {
  if (value === undefined || !Number.isFinite(value) || value <= 0) {
    return UNAVAILABLE
  }
  return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${Math.round(value)}ms`
}

function Sparkline({ values }: { values: number[] }) {
  if (values.length < 2) return null
  const width = 92
  const height = 26
  const max = Math.max(...values, 1)
  const min = Math.min(...values, 0)
  const span = max - min || 1
  const points = values.map((value, index) => ({
    x: (index / (values.length - 1)) * width,
    y: 3 + (1 - (value - min) / span) * (height - 6),
  }))
  const last = points[points.length - 1]
  return (
    <svg
      aria-hidden="true"
      viewBox={`0 0 ${width} ${height}`}
      className="h-[26px] w-[92px] shrink-0"
    >
      <path
        d={smoothPath(points)}
        fill="none"
        stroke="var(--brand)"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      {last !== undefined ? (
        <circle cx={last.x} cy={last.y} r="2" fill="var(--brand)" />
      ) : null}
    </svg>
  )
}

interface MetricItem {
  id: string
  label: string
  value: string
  meta?: string
  spark?: number[]
}

/**
 * 仪表台第一层：四个核心指标。管理员看消费/请求/成功率/P95，
 * 普通用户把余额放在第一位。所有格式化都走 `format.ts` 的总函数，
 * 无数据渲染 `--`，绝不出现 NaN。
 */
export function CoreMetrics({
  role,
  summary,
  rates,
  wallet,
  timeseries,
  loading,
  error,
  showCurrency,
  quotaPerUnit,
  currencySymbol,
  locale,
  onRetry,
}: CoreMetricsProps) {
  const { t } = useI18n()

  const credits = (value: number | undefined) =>
    value === undefined
      ? UNAVAILABLE
      : showCurrency
        ? formatCurrencyValue(value, quotaPerUnit, currencySymbol, locale)
        : formatCreditsValue(value, locale)

  const failureCount =
    summary === undefined
      ? undefined
      : Math.max(
          0,
          summary.request_count -
            Math.round(summary.success_rate * summary.request_count),
        )

  const costSeries = timeseries?.map((item) => item.cost) ?? []
  const requestSeries = timeseries?.map((item) => item.requests) ?? []

  const items: MetricItem[] =
    role === 'admin'
      ? [
          {
            id: 'spend',
            label: t('common.dashboard.overview.metrics.spend'),
            value: credits(summary?.cost_total),
            spark: costSeries,
          },
          {
            id: 'requests',
            label: t('common.dashboard.overview.metrics.requests'),
            value:
              summary === undefined
                ? UNAVAILABLE
                : formatInteger(summary.request_count, locale),
            spark: requestSeries,
          },
          {
            id: 'success',
            label: t('common.dashboard.overview.metrics.success'),
            value:
              summary === undefined
                ? UNAVAILABLE
                : formatSuccessRate(summary.success_rate, summary.request_count),
            meta:
              failureCount === undefined
                ? undefined
                : t('common.dashboard.overview.metrics.failures', {
                    count: failureCount,
                  }),
          },
          {
            id: 'p95',
            label: t('common.dashboard.overview.metrics.p95'),
            value: formatLatency(summary?.p95_latency_ms),
            meta:
              summary === undefined
                ? undefined
                : t('common.dashboard.overview.metrics.p50', {
                    value: formatLatency(summary.p50_latency_ms),
                  }),
          },
        ]
      : [
          {
            id: 'balance',
            label: t('common.dashboard.overview.metrics.balance'),
            value:
              wallet === undefined
                ? UNAVAILABLE
                : formatCurrencyValue(
                    wallet.balance,
                    quotaPerUnit,
                    currencySymbol,
                    locale,
                  ),
            meta: t('common.dashboard.overview.metrics.held', {
              value:
                wallet === undefined
                  ? UNAVAILABLE
                  : formatCurrencyValue(
                      wallet.held_credits,
                      quotaPerUnit,
                      currencySymbol,
                      locale,
                    ),
            }),
          },
          {
            id: 'spend',
            label: t('common.dashboard.overview.metrics.spend'),
            value: credits(summary?.cost_total),
            spark: costSeries,
          },
          {
            id: 'requests',
            label: t('common.dashboard.overview.metrics.requests'),
            value:
              summary === undefined
                ? UNAVAILABLE
                : formatInteger(summary.request_count, locale),
            spark: requestSeries,
          },
          {
            id: 'success',
            label: t('common.dashboard.overview.metrics.success'),
            value:
              summary === undefined
                ? UNAVAILABLE
                : formatSuccessRate(summary.success_rate, summary.request_count),
            meta:
              failureCount === undefined
                ? undefined
                : t('common.dashboard.overview.metrics.failures', {
                    count: failureCount,
                  }),
          },
        ]

  const secondary =
    role === 'admin'
      ? [
          [
            t('common.dashboard.overview.metrics.rpm'),
            rates === undefined
              ? UNAVAILABLE
              : `${formatRpm(rates.rpm_avg, locale)} / ${formatInteger(rates.rpm_peak, locale)}`,
          ],
          [
            t('common.dashboard.overview.metrics.active_users'),
            summary === undefined
              ? UNAVAILABLE
              : formatInteger(summary.active_users, locale),
          ],
          [
            t('common.dashboard.overview.metrics.tokens'),
            summary === undefined
              ? UNAVAILABLE
              : formatTokensValue(
                  summary.prompt_tokens_total + summary.completion_tokens_total,
                  locale,
                ),
          ],
        ]
      : [
          [
            t('common.dashboard.overview.metrics.total_consumed'),
            wallet === undefined
              ? UNAVAILABLE
              : formatCurrencyValue(
                  wallet.total_consumed,
                  quotaPerUnit,
                  currencySymbol,
                  locale,
                ),
          ],
          [
            t('common.dashboard.overview.metrics.tokens'),
            summary === undefined
              ? UNAVAILABLE
              : formatTokensValue(
                  summary.prompt_tokens_total + summary.completion_tokens_total,
                  locale,
                ),
          ],
          [
            t('common.dashboard.overview.metrics.threshold'),
            wallet === undefined
              ? UNAVAILABLE
              : formatCurrencyValue(
                  wallet.threshold,
                  quotaPerUnit,
                  currencySymbol,
                  locale,
                ),
          ],
        ]

  return (
    <section aria-label={t('common.dashboard.overview.metrics.title')}>
      <div className="mt-4 overflow-hidden rounded-xl border border-border-hairline bg-surface-card">
        <div className="grid grid-cols-2 divide-x divide-y divide-border-hairline lg:grid-cols-4 lg:divide-y-0">
          {items.map((item) => (
            <div key={item.id} className="min-w-0 p-4">
              <p className="text-badge text-ink-3">{item.label}</p>
              {loading ? (
                <div
                  data-testid={`${item.id}-skeleton`}
                  className="mt-2 h-7 w-24 animate-pulse rounded bg-surface-hover motion-reduce:animate-none"
                />
              ) : (
                <p
                  data-testid={`metric-${item.id}`}
                  className="mt-1 font-mono text-stat tabular-nums tracking-[-0.03em] text-ink-1"
                >
                  {error ? UNAVAILABLE : item.value}
                </p>
              )}
              <div className="mt-1 flex items-end justify-between gap-2">
                <span className="text-badge text-ink-3">
                  {error ? '' : item.meta}
                </span>
                {!error && item.spark !== undefined ? (
                  <Sparkline values={item.spark} />
                ) : null}
              </div>
            </div>
          ))}
        </div>
        <div className="flex flex-wrap items-baseline gap-x-7 gap-y-1 border-t border-border-hairline px-4 py-2.5">
          {secondary.map(([label, value]) => (
            <div key={label} className="flex items-baseline gap-1.5 text-badge text-ink-3">
              <span>{label}</span>
              <span className="font-mono text-badge tabular-nums text-ink-2">
                {error ? UNAVAILABLE : value}
              </span>
            </div>
          ))}
          {error ? (
            <button
              type="button"
              onClick={onRetry}
              className="ml-auto text-badge text-brand hover:underline"
            >
              {t('common.action.retry')}
            </button>
          ) : null}
        </div>
      </div>
    </section>
  )
}
