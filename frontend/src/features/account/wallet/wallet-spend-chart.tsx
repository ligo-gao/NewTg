import { useId, useMemo } from 'react'

import type { ChartSeries } from '@/shared/lib/chart-data'
import { useI18n } from '@/shared/lib/i18n-context'

/**
 * Wallet-local spending trend.
 *
 * The shared `AnalyticsChart` is tuned for the compact dashboard cards: its
 * 320×180 viewBox stretches to the full card width, which made the wallet
 * trend ~400px tall at 1440 and pushed the ledger below the fold. The wallet
 * trend is a wide banner (760×220, the same box as the approved demo), so it
 * gets its own renderer rather than bending the shared contract.
 *
 * Everything the shared chart guarantees still applies here: one Y axis, no
 * animation (values are always final), `role="img"` with a localised summary
 * label, and an equivalent visually hidden data table.
 */

export interface WalletSpendChartProps {
  /** `bucketSeries` output; every series is drawn over the shared buckets. */
  series: ChartSeries[]
  /** Localised label for the image and the hidden table caption. */
  ariaLabel: string
  /** Unit shown in the hidden table's value column (e.g. "credits"). */
  unit: string
}

/** The approved demo's box (demos/console-redesign/wallet-preview.html). */
const VIEW_WIDTH = 760
const VIEW_HEIGHT = 220
const PLOT_LEFT = 56
const PLOT_RIGHT = 30
const PLOT_TOP = 24
const PLOT_BOTTOM = 30
const PLOT_WIDTH = VIEW_WIDTH - PLOT_LEFT - PLOT_RIGHT
const PLOT_HEIGHT = VIEW_HEIGHT - PLOT_TOP - PLOT_BOTTOM
const BASELINE_Y = PLOT_TOP + PLOT_HEIGHT

/** Eight fixed categorical slots; never invent a ninth colour. */
const MAX_CATEGORY_SLOTS = 8

/** Demo accents the single wallet series with the brand colour. */
function colorForSeries(index: number): string {
  return index === 0
    ? 'var(--brand)'
    : `var(--chart-cat-${Math.min(index + 1, MAX_CATEGORY_SLOTS)})`
}

interface PlotPoint {
  ms: number
  x: number
  y: number
}

/** Rounded axis ceiling so labels read like 75 / 150 / 300. */
function niceMax(value: number): number {
  if (!Number.isFinite(value) || value <= 0) return 1
  const exponent = Math.floor(Math.log10(value))
  const magnitude = 10 ** exponent
  const normalized = value / magnitude
  // Every step divides evenly into four grid intervals, so the quarter ticks
  // stay whole numbers (300 → 75) instead of 187.5 or 62.5.
  const steps = [1, 1.2, 1.6, 2, 2.4, 3, 4, 5, 6, 8, 10]
  const step = steps.find((candidate) => normalized <= candidate) ?? 10
  return step * magnitude
}

function formatTick(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

function formatDay(ms: number, locale: string): string {
  return new Intl.DateTimeFormat(locale, {
    timeZone: 'UTC',
    month: '2-digit',
    day: '2-digit',
  }).format(new Date(ms))
}

/**
 * At most seven X labels: the demo shows every bucket for a week, and a month
 * still needs readable gaps rather than thirty overlapping dates.
 */
function tickIndexes(count: number): number[] {
  if (count <= 7) return Array.from({ length: count }, (_, index) => index)
  const step = Math.ceil((count - 1) / 6)
  const indexes: number[] = []
  for (let index = 0; index < count; index += step) indexes.push(index)
  const last = count - 1
  if (indexes[indexes.length - 1] !== last) indexes.push(last)
  return indexes
}

function xForIndex(index: number, count: number): number {
  if (count <= 1) return PLOT_LEFT + PLOT_WIDTH / 2
  return PLOT_LEFT + (index / (count - 1)) * PLOT_WIDTH
}

export function WalletSpendChart({
  series,
  ariaLabel,
  unit,
}: WalletSpendChartProps) {
  const { t, locale } = useI18n()
  const rawId = useId()
  const gradientBase = `wallet-spend-${rawId.replace(/[^a-zA-Z0-9_-]/g, '')}`

  const model = useMemo(() => {
    const buckets = [
      ...new Set(
        series.flatMap((item) =>
          item.points
            .map((point) => Date.parse(point.ts))
            .filter((ms) => !Number.isNaN(ms)),
        ),
      ),
    ].sort((a, b) => a - b)

    const parsed = series.map((item) => {
      const byBucket = new Map<number, number>()
      for (const point of item.points) {
        const ms = Date.parse(point.ts)
        if (Number.isNaN(ms) || !Number.isFinite(point.value)) continue
        byBucket.set(ms, (byBucket.get(ms) ?? 0) + point.value)
      }
      return { item, byBucket }
    })

    const max = niceMax(
      Math.max(
        0,
        ...series.flatMap((item) =>
          item.points.flatMap((point) =>
            Number.isFinite(point.value) && point.value > 0 ? [point.value] : [],
          ),
        ),
      ),
    )

    const plotted = parsed.map(({ item, byBucket }) => {
      const points: PlotPoint[] = []
      buckets.forEach((ms, index) => {
        const value = byBucket.get(ms)
        if (value === undefined) return
        const clamped = Math.max(0, value)
        points.push({
          ms,
          x: xForIndex(index, buckets.length),
          y: BASELINE_Y - (clamped / max) * PLOT_HEIGHT,
        })
      })
      return { item, points }
    })

    return { buckets, plotted, max }
  }, [series])

  const { buckets, plotted, max } = model
  const ticks = [0, 0.25, 0.5, 0.75, 1]

  return (
    <div className="w-full">
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
        className="block h-auto max-h-[240px] min-h-[220px] w-full"
        data-testid="wallet-spend-chart-svg"
      >
        <defs>
          {plotted.map(({ item }, index) => (
            <linearGradient
              key={item.id}
              id={`${gradientBase}-${index}`}
              x1="0"
              y1="0"
              x2="0"
              y2="1"
            >
              <stop
                offset="0"
                stopColor={colorForSeries(index)}
                stopOpacity={0.2}
              />
              <stop
                offset="1"
                stopColor={colorForSeries(index)}
                stopOpacity={0}
              />
            </linearGradient>
          ))}
        </defs>

        {/* One Y axis: horizontal grid lines plus their tick labels. */}
        <g stroke="var(--border-hairline)" strokeWidth={1}>
          {ticks.map((fraction) => {
            const y = PLOT_TOP + fraction * PLOT_HEIGHT
            return (
              <line
                key={fraction}
                x1={PLOT_LEFT}
                y1={y}
                x2={PLOT_LEFT + PLOT_WIDTH}
                y2={y}
              />
            )
          })}
        </g>
        <g className="fill-ink-3 text-badge tabular-nums">
          {ticks.map((fraction) => {
            const value = max * (1 - fraction)
            return (
              <text
                key={fraction}
                x={PLOT_LEFT - 8}
                y={PLOT_TOP + fraction * PLOT_HEIGHT + 3}
                textAnchor="end"
              >
                {formatTick(value, locale)}
              </text>
            )
          })}
          {tickIndexes(buckets.length).map((index) => {
            const bucket = buckets[index]
            if (bucket === undefined) return null
            return (
              <text
                key={`x-${bucket}`}
                x={xForIndex(index, buckets.length)}
                y={BASELINE_Y + 22}
                textAnchor={
                  index === 0
                    ? 'start'
                    : index === buckets.length - 1
                      ? 'end'
                      : 'middle'
                }
              >
                {formatDay(bucket, locale)}
              </text>
            )
          })}
        </g>

        {plotted.map(({ item, points }, index) => {
          const color = colorForSeries(index)
          const first = points[0]
          const last = points[points.length - 1]
          if (first === undefined || last === undefined) return null
          const areaPoints = [
            ...points.map((point) => `${point.x},${point.y}`),
            `${last.x},${BASELINE_Y}`,
            `${first.x},${BASELINE_Y}`,
          ].join(' ')
          return (
            <g key={item.id} data-series-id={item.id}>
              <polygon
                points={areaPoints}
                fill={`url(#${gradientBase}-${index})`}
              />
              <polyline
                points={points
                  .map((point) => `${point.x},${point.y}`)
                  .join(' ')}
                fill="none"
                stroke={color}
                strokeWidth={2.5}
                strokeLinecap="round"
                strokeLinejoin="round"
              />
              <g fill={color} stroke="var(--surface-card)" strokeWidth={2}>
                {points.map((point) => (
                  <circle
                    key={point.ms}
                    data-marker="true"
                    cx={point.x}
                    cy={point.y}
                    r={3.5}
                  />
                ))}
              </g>
            </g>
          )
        })}
      </svg>

      {/* GC-4 relief: the table is the equivalent text alternative. */}
      <table className="sr-only">
        <caption>{ariaLabel}</caption>
        <thead>
          <tr>
            <th scope="col">{t('common.chart.table.series')}</th>
            <th scope="col">{t('common.chart.table.time')}</th>
            <th scope="col">
              {t('common.chart.table.value')} ({unit})
            </th>
          </tr>
        </thead>
        <tbody>
          {series.flatMap((item) =>
            item.points.flatMap((point) => {
              if (!Number.isFinite(point.value)) return []
              return [
                <tr key={`${item.id}-${point.ts}`}>
                  <td>{item.label}</td>
                  <td>{point.ts}</td>
                  <td>
                    {new Intl.NumberFormat(locale, {
                      maximumFractionDigits: 2,
                    }).format(point.value)}
                  </td>
                </tr>,
              ]
            }),
          )}
        </tbody>
      </table>
    </div>
  )
}
