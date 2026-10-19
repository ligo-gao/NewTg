import { useMemo } from 'react'

import { bucketSeries, type ChartSeries } from '@/shared/lib/chart-data'
import { useI18n } from '@/shared/lib/i18n-context'
import { AnalyticsChart } from '@/shared/ui/analytics-chart'

import type { UsageGranularity, UsageRange, UsageStatsTimeseriesItem } from './api'
import { fillBucketSpine } from './timeseries-spine'

export interface ConsumptionTrendProps {
  items: UsageStatsTimeseriesItem[]
  range: UsageRange
  granularity: UsageGranularity
  loading: boolean
  error: boolean
  onRetry: () => void
  selectedModel?: string
  onClearModel: () => void
}

/**
 * Consumption area chart (F-05-03 §3.4). The server's UTC bucket strings are
 * fed through the shared `bucketSeries` contract with the range's granularity
 * — no local-time re-bucketing, no second Y axis. A single model drill-down
 * is reflected in a removable chip so the active filter is never invisible.
 */
export function ConsumptionTrend({
  items,
  range,
  granularity,
  loading,
  error,
  onRetry,
  selectedModel,
  onClearModel,
}: ConsumptionTrendProps) {
  const { t } = useI18n()

  const series: ChartSeries[] = useMemo(() => {
    // Drop malformed rows before the contract sees them and materialise the
    // full UTC bucket spine (the API only sends existing buckets). One bad
    // upstream row must not blank the whole overview.
    const points = items.flatMap((item) => {
      const ms = Date.parse(item.bucket)
      if (Number.isNaN(ms) || !Number.isFinite(item.cost)) return []
      return [{ ts: new Date(ms).toISOString(), value: item.cost }]
    })
    if (points.length === 0) return []
    return bucketSeries(
      [
        {
          id: 'cost',
          label: t('common.dashboard.trend.series'),
          points: fillBucketSpine(points, range, granularity),
        },
      ],
      granularity,
    )
  }, [items, range, granularity, t])

  const hasPoints = (series[0]?.points.length ?? 0) > 0

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-semibold text-ink-1">
          {t('common.dashboard.trend.title')}
        </h3>
        <div className="flex flex-wrap items-center gap-2">
          {selectedModel !== undefined ? (
            <span
              data-testid="trend-filter-chip"
              className="inline-flex items-center gap-1 rounded-md border border-brand-line bg-brand-subtle px-2 py-0.5 text-xs text-brand"
            >
              {t('common.dashboard.trend.filter_chip', { model: selectedModel })}
              <button
                type="button"
                aria-label={t('common.dashboard.trend.reset')}
                onClick={onClearModel}
                className="rounded focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                ×
              </button>
            </span>
          ) : null}
          <span className="rounded-md bg-surface-subtle px-2 py-0.5 text-xs text-ink-2">
            {granularity === 'hour'
              ? t('common.dashboard.trend.aggregated_hour')
              : t('common.dashboard.trend.aggregated_day')}
          </span>
        </div>
      </div>

      {loading ? (
        <div
          data-testid="trend-loading"
          aria-busy="true"
          className="mt-4 h-60 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
        />
      ) : error ? (
        <div data-testid="trend-error" className="mt-4 rounded-lg border border-danger-line bg-danger-bg p-3 text-sm text-danger">
          <p>{t('common.dashboard.error')}</p>
          <button
            type="button"
            data-testid="trend-retry"
            onClick={onRetry}
            className="mt-2 rounded border border-danger-line px-2 py-0.5 text-xs focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('common.action.retry')}
          </button>
        </div>
      ) : !hasPoints ? (
        <div
          data-testid="trend-empty"
          className="mt-4 flex min-h-60 flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-neutral-line text-sm text-neutral"
        >
          <p>{t('common.chart.empty')}</p>
          <a href="/tokens" className="text-brand underline">
            {t('common.dashboard.trend.empty_action')}
          </a>
        </div>
      ) : (
        <div data-testid="consumption-trend" className="mt-4">
          <AnalyticsChart
            series={series}
            config={{ kind: 'area', unit: t('common.dashboard.rates.credits') }}
          />
        </div>
      )}
    </section>
  )
}
