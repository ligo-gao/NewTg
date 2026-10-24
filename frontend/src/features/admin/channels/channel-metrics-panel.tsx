import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'

import { getChannelMetrics } from './api'
import type {
  Channel,
  ChannelMetricsPoint,
  ChannelMetricsWindow,
} from './types'

/** 24h/7d tabs frozen by modules-06 F-06-11. */
const WINDOWS: readonly ChannelMetricsWindow[] = ['24h', '7d']

/** Format a nullable number; null means "no samples" and must render — (not 0). */
function formatNumber(value: number | null, digits = 0): string {
  return value === null ? '—' : value.toFixed(digits)
}

function formatPercent(value: number | null): string {
  return value === null ? '—' : `${(value * 100).toFixed(1)}%`
}

/**
 * Builds one SVG path per contiguous run of non-null success rates.
 *
 * Hours with no traffic have `success_rate: null`; drawing them as 0 would
 * misrepresent an idle hour as a total outage, so the line breaks instead.
 * 24 or 168 points share the same viewBox; x is evenly spaced.
 */
function successPath(points: ChannelMetricsPoint[]): string {
  const width = 240
  const height = 60
  if (points.length === 0) return ''
  const step = points.length > 1 ? width / (points.length - 1) : width
  const parts: string[] = []
  let drawing = false
  points.forEach((point, index) => {
    if (point.success_rate === null) {
      drawing = false
      return
    }
    const x = index * step
    // Invert Y: 0% sits at the bottom, 100% at the top (4px inset).
    const y = 4 + (1 - point.success_rate) * (height - 8)
    parts.push(`${drawing ? 'L' : 'M'}${x.toFixed(2)} ${y.toFixed(2)}`)
    drawing = true
  })
  return parts.join(' ')
}

/** Lightweight inline SVG sparkline — deliberately no charting dependency. */
function SuccessSparkline({ points }: { points: ChannelMetricsPoint[] }) {
  const { t } = useI18n()
  const path = useMemo(() => successPath(points), [points])
  return (
    <div className="rounded-lg border border-border-hairline bg-surface-card p-3">
      <p className="mb-2 text-meta text-ink-2">{t('common.channels.metrics.trend')}</p>
      <svg
        data-testid="channel-metrics-sparkline"
        role="img"
        aria-label={t('common.channels.metrics.trend')}
        viewBox="0 0 240 60"
        preserveAspectRatio="none"
        className="h-16 w-full"
      >
        <line
          x1="0"
          y1="56"
          x2="240"
          y2="56"
          className="text-border-hairline"
          stroke="currentColor"
          strokeWidth="1"
          vectorEffect="non-scaling-stroke"
        />
        {path === '' ? null : (
          <path
            d={path}
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinejoin="round"
            strokeLinecap="round"
            vectorEffect="non-scaling-stroke"
            className="text-success"
          />
        )}
      </svg>
    </div>
  )
}

function SummaryCell({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-meta text-ink-2">{label}</dt>
      <dd className="mt-0.5 text-body text-ink-1 tabular-nums">{value}</dd>
    </div>
  )
}

/**
 * Channel metrics panel (modules-06 F-06-11); rendered inside the expanded
 * list row. Server state lives in its own Query cache entry keyed by
 * channel + window, so switching tabs and re-expanding is cheap and never
 * blocks the channel management table.
 */
export function ChannelMetricsPanel({ channel }: { channel: Channel }) {
  const { t } = useI18n()
  const [window, setWindow] = useState<ChannelMetricsWindow>('24h')

  const metricsQuery = useQuery({
    queryKey: ['admin', 'channels', 'metrics', channel.id, window],
    queryFn: ({ signal }) => getChannelMetrics(channel.id, window, signal),
  })

  const error = metricsQuery.error
  const offline = error instanceof ApiError && error.kind === 'offline'
  const data = metricsQuery.data
  const summary = data?.summary
  const noUsage = summary !== undefined && summary.request_count === 0

  return (
    <section
      data-testid={`channel-metrics-${channel.id}`}
      className="min-w-0 rounded-lg border border-border-hairline bg-surface-card p-4"
      aria-label={t('common.channels.metrics.title')}
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-card-title tracking-[0.02em] text-ink-2">
          {t('common.channels.metrics.title')}
        </h3>
        <div className="flex items-center gap-1" role="tablist">
          {WINDOWS.map((value) => (
            <button
              key={value}
              type="button"
              role="tab"
              aria-selected={window === value}
              onClick={() => {
                setWindow(value)
              }}
              className={[
                'rounded-md px-2.5 py-1 text-badge focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                window === value
                  ? 'bg-brand-subtle text-brand'
                  : 'text-ink-2 hover:bg-surface-hover',
              ].join(' ')}
            >
              {t(`common.channels.metrics.range.${value}`)}
            </button>
          ))}
          <Button
            variant="ghost"
            onClick={() => {
              void metricsQuery.refetch()
            }}
            disabled={metricsQuery.isFetching}
          >
            {t('common.channels.metrics.refresh')}
          </Button>
        </div>
      </div>

      {metricsQuery.isPending ? (
        <div data-testid="channel-metrics-loading" className="mt-3 space-y-2">
          {Array.from({ length: 3 }, (_, index) => (
            <div
              key={index}
              className="h-4 animate-pulse rounded bg-surface-subtle"
            />
          ))}
        </div>
      ) : metricsQuery.isError ? (
        <div role="alert" className="mt-3 flex flex-col items-start gap-2 text-body">
          <p className="text-danger">
            {offline
              ? t('common.channels.metrics.offline')
              : t('common.channels.metrics.error')}
          </p>
          <Button
            variant="secondary"
            onClick={() => {
              void metricsQuery.refetch()
            }}
          >
            {t('common.channels.metrics.retry')}
          </Button>
        </div>
      ) : noUsage ? (
        <p data-testid="channel-metrics-empty" className="mt-3 text-body text-ink-2">
          {t('common.channels.metrics.empty')}
        </p>
      ) : data !== undefined && summary !== undefined ? (
        <div className="mt-3 space-y-3">
          <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
            <SummaryCell
              label={t('common.channels.metrics.requests')}
              value={formatNumber(summary.request_count)}
            />
            <SummaryCell
              label={t('common.channels.metrics.successRate')}
              value={formatPercent(summary.success_rate)}
            />
            <SummaryCell
              label={t('common.channels.metrics.avgLatency')}
              value={
                summary.avg_latency_ms === null
                  ? '—'
                  : `${formatNumber(summary.avg_latency_ms)} ms`
              }
            />
            <SummaryCell
              label={t('common.channels.metrics.avgFirstToken')}
              value={
                summary.avg_first_token_ms === null
                  ? '—'
                  : `${formatNumber(summary.avg_first_token_ms)} ms`
              }
            />
            <SummaryCell
              label={t('common.channels.metrics.avgCost')}
              value={formatNumber(summary.avg_cost, 2)}
            />
            <SummaryCell
              label={t('common.channels.metrics.usedCredits')}
              value={formatNumber(summary.used_credits_24h)}
            />
          </dl>
          <SuccessSparkline points={data.points} />
        </div>
      ) : null}
    </section>
  )
}
