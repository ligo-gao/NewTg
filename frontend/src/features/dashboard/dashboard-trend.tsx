import { useMemo } from 'react'

import { bucketSeries, type ChartSeries } from '@/shared/lib/chart-data'
import { useI18n } from '@/shared/lib/i18n-context'
import type { UsageGranularity, UsageRange, UsageStatsTimeseriesItem } from './api'
import { DashboardChart } from './dashboard-chart'

export type TrendMetric = 'cost' | 'requests'

export interface DashboardTrendProps {
  items: UsageStatsTimeseriesItem[]
  range: UsageRange
  granularity: UsageGranularity
  metric: TrendMetric
  onMetricChange: (metric: TrendMetric) => void
  loading: boolean
  error: boolean
  onRetry: () => void
  selectedModel?: string
  onClearModel: () => void
}

/**
 * 仪表台主趋势图。只有一个序列，指标在消费/请求之间切换；模型筛选由
 * 模型构成面板写入。服务端的 UTC 桶经 `bucketSeries` 契约处理，不在
 * 本地重新分桶。
 */
export function DashboardTrend({
  items,
  range,
  granularity,
  metric,
  onMetricChange,
  loading,
  error,
  onRetry,
  selectedModel,
  onClearModel,
}: DashboardTrendProps) {
  const { t } = useI18n()

  const series: ChartSeries[] = useMemo(() => {
    const points = items.flatMap((item) => {
      const ms = Date.parse(item.bucket)
      const value = metric === 'cost' ? item.cost : item.requests
      if (Number.isNaN(ms) || !Number.isFinite(value)) return []
      return [{ ts: new Date(ms).toISOString(), value }]
    })
    if (points.length === 0) return []
    return bucketSeries(
      [
        {
          id: metric,
          label:
            metric === 'cost'
              ? t('common.dashboard.overview.trend.metric_cost')
              : t('common.dashboard.overview.trend.metric_requests'),
          points,
        },
      ],
      granularity,
    )
  }, [items, metric, granularity, t])

  const hasPoints = (series[0]?.points.length ?? 0) > 0

  return (
    <section
      aria-label={t('common.dashboard.overview.trend.title')}
      className="rounded-xl border border-border-hairline bg-surface-card p-4"
      data-testid="dashboard-trend"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="text-body font-semibold text-ink-1">
            {metric === 'cost'
              ? t('common.dashboard.overview.trend.cost_title')
              : t('common.dashboard.overview.trend.request_title')}
          </h2>
          <p className="mt-0.5 text-badge text-ink-3">
            {t('common.dashboard.overview.trend.subtitle')}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <div
            role="group"
            aria-label={t('common.dashboard.overview.trend.metric_label')}
            className="inline-flex overflow-hidden rounded-lg border border-border-default"
          >
            {(['cost', 'requests'] as const).map((value) => (
              <button
                key={value}
                type="button"
                data-testid={`trend-metric-${value}`}
                aria-pressed={metric === value}
                onClick={() => onMetricChange(value)}
                className={[
                  'px-2.5 py-1 text-badge focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                  metric === value
                    ? 'bg-brand text-on-brand'
                    : 'bg-surface-card text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                ].join(' ')}
              >
                {t(`common.dashboard.overview.trend.metric_${value}`)}
              </button>
            ))}
          </div>
          {selectedModel !== undefined ? (
            <span className="inline-flex items-center gap-1 rounded-md border border-brand-line bg-brand-subtle px-2 py-0.5 text-badge text-brand">
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
        </div>
      </div>

      <div className="mt-3">
        {loading ? (
          <div
            data-testid="dashboard-trend-skeleton"
            className="h-[180px] animate-pulse rounded bg-surface-hover motion-reduce:animate-none"
          />
        ) : error ? (
          <div className="flex h-[180px] flex-col items-center justify-center gap-2 text-body text-ink-3">
            <p>{t('common.dashboard.overview.trend.error')}</p>
            <button
              type="button"
              onClick={onRetry}
              className="text-brand hover:underline"
            >
              {t('common.action.retry')}
            </button>
          </div>
        ) : hasPoints && series[0] !== undefined ? (
          <DashboardChart
            series={series[0]}
            unit={
              metric === 'cost'
                ? t('common.dashboard.rates.credits')
                : t('common.dashboard.overview.trend.metric_requests')
            }
            granularity={granularity}
          />
        ) : (
          <div className="flex h-[180px] flex-col items-center justify-center gap-2 text-body text-ink-3">
            <p>{t('common.dashboard.overview.trend.empty')}</p>
            <a href="/tokens" className="text-brand hover:underline">
              {t('common.dashboard.trend.empty_action')}
            </a>
          </div>
        )}
      </div>
      <p className="mt-1 text-badge text-ink-3">
        {range} · {granularity === 'hour' ? t('common.dashboard.trend.aggregated_hour') : t('common.dashboard.trend.aggregated_day')}
      </p>
    </section>
  )
}
