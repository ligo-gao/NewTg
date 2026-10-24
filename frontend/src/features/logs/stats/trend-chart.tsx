import { useState } from 'react'

import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'

import { formatBucketTick, trendInsights, type TrendPoint } from './insights'

export interface TrendChartProps {
  rows: TrendPoint[]
  granularity: 'hour' | 'day'
}

/** Gridlines at the pane's 0/25/50/75/100 % heights. */
const GRIDLINES = [0, 25, 50, 75, 100]

function compact(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

function formatCount(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}

function barHeight(value: number, max: number): string {
  // A zero bucket keeps its slot as a hairline stub instead of disappearing,
  // so the x-axis stays aligned between the two panes.
  return value === 0 ? '2px' : `${(value / max) * 100}%`
}

const BAR_BASE = 'rounded-t'

/**
 * Dual-pane HTML/CSS bar chart: spend above, requests below. No SVG and no
 * chart library — the panes are plain grids so zero buckets can keep their
 * slot and the x-axis stays aligned across both. Hover exposes one bucket at
 * a time; the `sr-only` table is the accessible equivalent of the whole chart.
 */
export function TrendChart({ rows, granularity }: TrendChartProps) {
  const { t, locale } = useI18n()
  const [hover, setHover] = useState<number | null>(null)

  const insights = trendInsights(rows)
  const maxCost = Math.max(...rows.map((row) => row.cost), 1)
  const maxRequests = Math.max(...rows.map((row) => row.requests), 1)
  const step = Math.max(1, Math.ceil(rows.length / 6))
  const tickIndexes = rows
    .map((_, index) => index)
    .filter((index) => index % step === 0)
  const active = hover === null ? null : (rows[hover] ?? null)
  const barColumns = {
    gridTemplateColumns: `repeat(${String(rows.length)}, minmax(0, 1fr))`,
    gap: '2px',
  }

  if (rows.length === 0) return null

  return (
    <div data-testid="trend-chart" className="relative flex flex-col gap-2">
      <div aria-hidden="true" className="flex flex-col gap-2">
        {/* Spend pane. */}
        <div className="grid grid-cols-[44px_minmax(0,1fr)] gap-1.5">
          <div className="relative h-[170px] sm:h-[286px]">
            <span
              className="absolute right-0 -translate-y-1/2 text-badge font-mono leading-none tabular-nums text-ink-3"
              style={{ top: '0%' }}
            >
              {compact(maxCost, locale)}
            </span>
            <span
              className="absolute right-0 -translate-y-1/2 text-badge font-mono leading-none tabular-nums text-ink-3"
              style={{ top: '50%' }}
            >
              {compact(maxCost / 2, locale)}
            </span>
            <span
              className="absolute right-0 -translate-y-1/2 text-badge font-mono leading-none tabular-nums text-ink-3"
              style={{ top: '100%' }}
            >
              {compact(0, locale)}
            </span>
          </div>
          <div
            className="relative h-[170px] sm:h-[286px]"
            onMouseLeave={() => {
              setHover(null)
            }}
          >
            {GRIDLINES.map((percent) => (
              <div
                key={percent}
                className="absolute inset-x-0 h-px bg-border-hairline/70"
                style={{ top: `${String(percent)}%` }}
              />
            ))}
            <div className="absolute inset-0 grid h-full items-end" style={barColumns}>
              {rows.map((row, index) => (
                <div
                  key={row.bucket}
                  data-testid="trend-bar-cost"
                  data-zero={row.cost === 0 ? 'true' : 'false'}
                  title={`${formatBucketTick(row.bucket, granularity, locale)} · ${formatCredits(row.cost, locale)} ${t('logs.usage.credits')} · ${formatCount(row.requests, locale)}`}
                  style={{ height: barHeight(row.cost, maxCost), minHeight: 2 }}
                  className={
                    row.cost === 0
                      ? `${BAR_BASE} bg-border-hairline`
                      : `${BAR_BASE} bg-[var(--chart-cat-1)]`
                  }
                  onMouseEnter={() => {
                    setHover(index)
                  }}
                />
              ))}
            </div>
            {insights.averageCost > 0 ? (
              <div
                data-testid="trend-avg"
                className="absolute inset-x-0 border-t border-dashed border-ink-3"
                style={{
                  top: `${String((1 - insights.averageCost / maxCost) * 100)}%`,
                }}
              >
                <span className="absolute right-0 -translate-y-1/2 bg-surface-card px-1 text-badge tabular-nums text-ink-3">
                  {t('logs.usage.stats.chart.average', {
                    value: compact(insights.averageCost, locale),
                  })}
                </span>
              </div>
            ) : null}
            {hover !== null && active !== null ? (
              <div
                data-testid="trend-tip"
                className="pointer-events-none absolute top-0 z-10 -translate-x-1/2 rounded-md border border-border-hairline bg-surface-card px-2 py-1 text-badge whitespace-nowrap shadow-sm"
                style={{
                  left: `${String(((hover + 0.5) / rows.length) * 100)}%`,
                }}
              >
                <span className="text-ink-3">
                  {formatBucketTick(active.bucket, granularity, locale)}
                </span>
                <span className="ml-2 font-mono tabular-nums text-ink-1">
                  {formatCredits(active.cost, locale)} {t('logs.usage.credits')}
                </span>
                <span className="ml-2 font-mono tabular-nums text-ink-2">
                  {formatCount(active.requests, locale)}
                </span>
              </div>
            ) : null}
          </div>
        </div>

        {/* Requests pane. */}
        <div className="grid grid-cols-[44px_minmax(0,1fr)] gap-1.5">
          <div className="relative h-[68px]">
            <span
              className="absolute right-0 -translate-y-1/2 text-badge font-mono leading-none tabular-nums text-ink-3"
              style={{ top: '0%' }}
            >
              {compact(maxRequests, locale)}
            </span>
          </div>
          <div
            className="relative h-[68px]"
            onMouseLeave={() => {
              setHover(null)
            }}
          >
            {GRIDLINES.map((percent) => (
              <div
                key={percent}
                className="absolute inset-x-0 h-px bg-border-hairline/70"
                style={{ top: `${String(percent)}%` }}
              />
            ))}
            <div className="absolute inset-0 grid h-full items-end" style={barColumns}>
              {rows.map((row, index) => (
                <div
                  key={row.bucket}
                  data-testid="trend-bar-requests"
                  data-zero={row.requests === 0 ? 'true' : 'false'}
                  title={`${formatBucketTick(row.bucket, granularity, locale)} · ${formatCount(row.requests, locale)} ${t('logs.usage.stats.columns.requests')}`}
                  style={{
                    height: barHeight(row.requests, maxRequests),
                    minHeight: 2,
                  }}
                  className={
                    row.requests === 0
                      ? `${BAR_BASE} bg-border-hairline`
                      : `${BAR_BASE} bg-[var(--chart-cat-2)]`
                  }
                  onMouseEnter={() => {
                    setHover(index)
                  }}
                />
              ))}
            </div>
          </div>
        </div>

        {/* X axis: labels only every `step` buckets, aligned to their slot. */}
        <div className="grid grid-cols-[44px_minmax(0,1fr)] gap-1.5">
          <div />
          <div
            className="grid"
            style={{
              gridTemplateColumns: `repeat(${String(rows.length)}, minmax(0, 1fr))`,
              gap: '2px',
            }}
          >
            {tickIndexes.map((index, tickPosition) => {
              const row = rows[index]
              if (row === undefined) return null
              return (
                <span
                  key={row.bucket}
                  data-testid="trend-x-tick"
                  className={`text-badge leading-none text-ink-3 ${
                    tickPosition === 0
                      ? 'text-left'
                      : tickPosition === tickIndexes.length - 1
                        ? 'text-right'
                        : 'text-center'
                  }`}
                  style={{ gridColumn: index + 1 }}
                >
                  {formatBucketTick(row.bucket, granularity, locale)}
                </span>
              )
            })}
          </div>
        </div>
      </div>

      <table data-testid="trend-chart-table" className="sr-only">
        <caption>{t('logs.usage.stats.trend_table_caption')}</caption>
        <thead>
          <tr>
            <th scope="col">{t('logs.usage.stats.columns.time')}</th>
            <th scope="col">{t('logs.usage.stats.columns.cost')}</th>
            <th scope="col">{t('logs.usage.stats.columns.requests')}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.bucket}>
              <td>{formatBucketTick(row.bucket, granularity, locale)}</td>
              <td>{formatCredits(row.cost, locale)}</td>
              <td>{formatCount(row.requests, locale)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
