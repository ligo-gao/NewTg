import { useMemo } from 'react'

import {
  formatAxis,
  type ChartConfig,
  type ChartSeries,
} from '@/shared/lib/chart-data'
import { useI18n } from '@/shared/lib/i18n-context'

/**
 * Contract-driven chart placeholder (R-01). M6 deliberately ships no chart
 * library: the component receives exactly the props a future adapter will
 * receive (`series` + `config`) and renders axes plus a polyline / bars / area
 * from them. Swapping in a library later must not change this public surface.
 *
 * Accessibility (GC-4, web-00 §10.3): the SVG is `role="img"` with a summary
 * label, and an equivalent data table is rendered visually hidden. There are
 * no animations, so `prefers-reduced-motion` needs no special branch — the
 * values are always the final values (the ledger discipline also forbids
 * count-up animation).
 */

export interface AnalyticsChartProps {
  series: ChartSeries[]
  config: ChartConfig
}

const VIEW_WIDTH = 320
const VIEW_HEIGHT = 180
const PLOT_LEFT = 44
const PLOT_RIGHT = 8
const PLOT_TOP = 10
const PLOT_BOTTOM = 26
const PLOT_WIDTH = VIEW_WIDTH - PLOT_LEFT - PLOT_RIGHT
const PLOT_HEIGHT = VIEW_HEIGHT - PLOT_TOP - PLOT_BOTTOM
const BASELINE_Y = PLOT_TOP + PLOT_HEIGHT

/** Eight fixed categorical slots, assigned in order and never cycled. */
const MAX_CATEGORY_SLOTS = 8

function colorForSeries(index: number): string {
  // web-00 §10.2 / TC-F-01: never generate a ninth colour. Clamping keeps the
  // slot count fixed; extra series reuse the last declared slot rather than
  // inventing one.
  return `var(--chart-cat-${Math.min(index + 1, MAX_CATEGORY_SLOTS)})`
}

interface ParsedPoint {
  ts: string
  ms: number
  value: number
}

interface ChartModel {
  hasData: boolean
  seriesPoints: ParsedPoint[][]
  buckets: number[]
  bucketIndex: Map<number, number>
  min: number
  max: number
}

/**
 * Normalises the contract data once per render. Invalid points are dropped
 * here (rather than throwing inside a component) so a bad upstream row cannot
 * crash a dashboard; `bucketSeries` still rejects them at the data boundary.
 */
function buildModel(series: ChartSeries[], kind: ChartConfig['kind']): ChartModel {
  const seriesPoints = series.map((item) =>
    item.points
      .flatMap((point) => {
        const ms = Date.parse(point.ts)
        if (Number.isNaN(ms) || !Number.isFinite(point.value)) return []
        return [{ ts: new Date(ms).toISOString(), ms, value: point.value }]
      })
      .sort((a, b) => a.ms - b.ms),
  )

  const buckets = [
    ...new Set(seriesPoints.flatMap((points) => points.map((point) => point.ms))),
  ].sort((a, b) => a - b)

  const bucketIndex = new Map(buckets.map((bucket, index) => [bucket, index]))
  const values = seriesPoints.flatMap((points) =>
    points.map((point) => point.value),
  )

  let min = 0
  let max = 1
  if (values.length > 0) {
    // Bars must have a zero baseline; lines and areas use their own range so a
    // narrow band of large credits values is still readable.
    min = kind === 'bar' ? Math.min(0, ...values) : Math.min(...values)
    max = kind === 'bar' ? Math.max(0, ...values) : Math.max(...values)
    if (max === min) max = min + 1
  }

  return {
    hasData: buckets.length > 0,
    seriesPoints,
    buckets,
    bucketIndex,
    min,
    max,
  }
}

function xForIndex(index: number, count: number): number {
  if (count <= 1) return PLOT_LEFT + PLOT_WIDTH / 2
  return PLOT_LEFT + (index / (count - 1)) * PLOT_WIDTH
}

function yFor(value: number, min: number, max: number): number {
  return PLOT_TOP + (1 - (value - min) / (max - min)) * PLOT_HEIGHT
}

function formatTick(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

export function AnalyticsChart({ series, config }: AnalyticsChartProps) {
  const { t, locale } = useI18n()
  const model = useMemo(() => buildModel(series, config.kind), [series, config.kind])

  const ariaLabel = t('common.chart.aria', {
    kind: config.kind,
    count: series.length,
    series: series.map((item) => item.label).join(', '),
    unit: config.unit,
  })

  if (!model.hasData) {
    return (
      <div
        data-testid="analytics-chart"
        data-kind={config.kind}
        data-empty="true"
        className="flex min-h-40 items-center justify-center rounded-lg border border-dashed border-neutral-line text-sm text-neutral"
      >
        <p data-testid="analytics-chart-empty">{t('common.chart.empty')}</p>
      </div>
    )
  }

  const { buckets, bucketIndex, seriesPoints, min, max } = model
  const bucketCount = buckets.length
  const seriesCount = Math.max(series.length, 1)
  const baseline = yFor(0, min, max)
  const step = PLOT_WIDTH / Math.max(bucketCount, 1)
  const barWidth = (step * 0.6) / seriesCount

  // Show at most three X labels (first / middle / last); more would collide in
  // the narrow placeholder and the table view carries the full timeline.
  const tickIndexes = [
    ...new Set([0, Math.floor((bucketCount - 1) / 2), bucketCount - 1]),
  ]

  return (
    <div
      data-testid="analytics-chart"
      data-kind={config.kind}
      data-series-count={series.length}
      className="w-full"
    >
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
        className="h-auto w-full"
      >
        {/* Axes: a single Y axis (web-00 §10.2 forbids dual Y axes). */}
        <line
          x1={PLOT_LEFT}
          y1={PLOT_TOP}
          x2={PLOT_LEFT}
          y2={BASELINE_Y}
          stroke="var(--border-hairline)"
        />
        <line
          x1={PLOT_LEFT}
          y1={BASELINE_Y}
          x2={PLOT_LEFT + PLOT_WIDTH}
          y2={BASELINE_Y}
          stroke="var(--border-hairline)"
        />
        {[0, 0.5, 1].map((fraction) => {
          const y = PLOT_TOP + fraction * PLOT_HEIGHT
          return (
            <line
              key={fraction}
              x1={PLOT_LEFT}
              y1={y}
              x2={PLOT_LEFT + PLOT_WIDTH}
              y2={y}
              stroke="var(--border-hairline)"
            />
          )
        })}
        <text
          x={PLOT_LEFT - 6}
          y={PLOT_TOP + 4}
          textAnchor="end"
          className="fill-neutral text-[8px] tabular-nums"
        >
          {formatTick(max, locale)}
        </text>
        <text
          x={PLOT_LEFT - 6}
          y={BASELINE_Y}
          textAnchor="end"
          className="fill-neutral text-[8px] tabular-nums"
        >
          {formatTick(min, locale)}
        </text>
        {tickIndexes.map((index) => {
          const bucket = buckets[index]
          if (bucket === undefined) return null
          return (
            <text
              key={bucket}
              x={xForIndex(index, bucketCount)}
              y={BASELINE_Y + 12}
              textAnchor={
                index === 0 ? 'start' : index === bucketCount - 1 ? 'end' : 'middle'
              }
              className="fill-neutral text-[8px] tabular-nums"
            >
              {formatAxis(new Date(bucket).toISOString(), locale)}
            </text>
          )
        })}
        {seriesPoints.map((points, seriesIndex) => {
          const color = colorForSeries(seriesIndex)
          const item = series[seriesIndex]
          if (item === undefined) return null

          if (config.kind === 'bar') {
            return points.map((point, pointIndex) => {
              const bucket = bucketIndex.get(point.ms)
              if (bucket === undefined) return null
              const valueY = yFor(point.value, min, max)
              const y = Math.min(valueY, baseline)
              const height = Math.abs(valueY - baseline)
              return (
                <rect
                  key={`${item.id}-${point.ts}-${pointIndex}`}
                  data-bar="true"
                  data-series-id={item.id}
                  x={
                    PLOT_LEFT +
                    bucket * step +
                    (step - barWidth * seriesCount) / 2 +
                    seriesIndex * barWidth
                  }
                  y={y}
                  width={Math.max(barWidth, 0.5)}
                  height={height}
                  fill={color}
                />
              )
            })
          }

          const coordinates = points.flatMap((point) => {
            const bucket = bucketIndex.get(point.ms)
            if (bucket === undefined) return []
            return [
              {
                x: xForIndex(bucket, bucketCount),
                y: yFor(point.value, min, max),
              },
            ]
          })

          if (config.kind === 'area') {
            const first = coordinates[0]
            const last = coordinates[coordinates.length - 1]
            if (first === undefined || last === undefined) return null
            const polygon = [
              ...coordinates.map((point) => `${point.x},${point.y}`),
              `${last.x},${BASELINE_Y}`,
              `${first.x},${BASELINE_Y}`,
            ].join(' ')
            return (
              <polygon
                key={item.id}
                data-series-id={item.id}
                points={polygon}
                fill={color}
                fillOpacity={0.18}
                stroke={color}
                strokeWidth={2}
              />
            )
          }

          return (
            <polyline
              key={item.id}
              data-series-id={item.id}
              points={coordinates.map((point) => `${point.x},${point.y}`).join(' ')}
              fill="none"
              stroke={color}
              strokeWidth={2}
            />
          )
        })}
      </svg>

      {/* Relief rule / GC-4: the table is the equivalent text alternative for
          every rendered series, including buckets the axes label sparsely. */}
      <table className="sr-only">
        <caption>{ariaLabel}</caption>
        <thead>
          <tr>
            <th scope="col">{t('common.chart.table.series')}</th>
            <th scope="col">{t('common.chart.table.time')}</th>
            <th scope="col">
              {t('common.chart.table.value')} ({config.unit})
            </th>
          </tr>
        </thead>
        <tbody>
          {seriesPoints.map((points, seriesIndex) =>
            points.map((point, pointIndex) => (
              <tr key={`${series[seriesIndex]?.id ?? seriesIndex}-${point.ts}-${pointIndex}`}>
                <td>{series[seriesIndex]?.label ?? ''}</td>
                <td>{point.ts}</td>
                <td>{point.value}</td>
              </tr>
            )),
          )}
        </tbody>
      </table>
    </div>
  )
}
