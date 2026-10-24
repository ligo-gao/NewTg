import { useId, useMemo, useRef, useState, type MouseEvent } from 'react'

import type { ChartSeries } from '@/shared/lib/chart-data'
import { smoothAreaPath, smoothPath, niceCeil } from '@/shared/lib/chart-path'
import { useI18n } from '@/shared/lib/i18n-context'

export interface DashboardChartProps {
  series: ChartSeries
  unit: string
  granularity: 'hour' | 'day'
}

const WIDTH = 760
const HEIGHT = 260
const LEFT = 56
const RIGHT = 18
const TOP = 16
const BOTTOM = 30
const PLOT_WIDTH = WIDTH - LEFT - RIGHT
const PLOT_HEIGHT = HEIGHT - TOP - BOTTOM
const BASELINE = TOP + PLOT_HEIGHT

function compact(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

function full(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    maximumFractionDigits: 2,
  }).format(value)
}

function tickTime(ts: string, granularity: 'hour' | 'day', locale: string): string {
  const ms = Date.parse(ts)
  if (Number.isNaN(ms)) return ts
  return new Intl.DateTimeFormat(locale, {
    timeZone: 'UTC',
    hour: granularity === 'hour' ? '2-digit' : undefined,
    minute: granularity === 'hour' ? '2-digit' : undefined,
    month: granularity === 'day' ? 'short' : undefined,
    day: granularity === 'day' ? 'numeric' : undefined,
  }).format(new Date(ms))
}

/**
 * 仪表台专用趋势图。与共享的 `AnalyticsChart` 不同，它使用固定的
 * 760×260 画布、平滑的单调三次曲线和渐变面积，线宽不会随容器放大；
 * 悬停显示十字线与数值。数据表仍作为无障碍替代。
 */
export function DashboardChart({ series, unit, granularity }: DashboardChartProps) {
  const { t, locale } = useI18n()
  const gradientId = `trend-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`
  const svgRef = useRef<SVGSVGElement | null>(null)
  const [hover, setHover] = useState<number | null>(null)

  const values = useMemo(
    () => series.points.map((point) => point.value),
    [series.points],
  )
  const max = useMemo(() => niceCeil(Math.max(...values, 0) * 1.08), [values])
  const ticks = useMemo(() => [0, 0.25, 0.5, 0.75, 1].map((f) => max * f), [max])
  const points = series.points.map((point, index) => ({
    ts: point.ts,
    value: point.value,
    x:
      series.points.length <= 1
        ? LEFT + PLOT_WIDTH / 2
        : LEFT + (index / (series.points.length - 1)) * PLOT_WIDTH,
    y: TOP + (1 - point.value / max) * PLOT_HEIGHT,
  }))
  const line = smoothPath(points)
  const area = smoothAreaPath(points, BASELINE)
  const labelIndexes = [
    ...new Set(
      points.length === 0
        ? []
        : [0, Math.floor((points.length - 1) / 4), Math.floor((points.length - 1) / 2), Math.floor(((points.length - 1) * 3) / 4), points.length - 1],
    ),
  ]
  const last = points[points.length - 1]
  const active = hover === null ? null : (points[hover] ?? null)

  function handleMove(event: MouseEvent<SVGRectElement>) {
    const svg = svgRef.current
    if (svg === null || points.length === 0) return
    const bounds = svg.getBoundingClientRect()
    if (bounds.width === 0) return
    const ratio = (event.clientX - bounds.left) / bounds.width
    const x = ratio * WIDTH
    const index = Math.round(((x - LEFT) / PLOT_WIDTH) * (points.length - 1))
    setHover(Math.max(0, Math.min(points.length - 1, index)))
  }

  if (points.length === 0) return null

  return (
    <div className="relative" data-testid="dashboard-chart">
      <div className="overflow-x-auto">
        <svg
          ref={svgRef}
          role="img"
          aria-label={t('common.chart.aria', {
            kind: 'area',
            count: 1,
            series: series.label,
            unit,
          })}
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="h-auto w-full min-w-[560px]"
        >
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--brand)" stopOpacity="0.18" />
              <stop offset="100%" stopColor="var(--brand)" stopOpacity="0.01" />
            </linearGradient>
          </defs>

          {/* Quiet horizontal grid. No vertical rules: the trace carries the eye. */}
          {ticks.map((tick) => {
            const y = TOP + (1 - tick / max) * PLOT_HEIGHT
            return (
              <g key={tick}>
                <line
                  x1={LEFT}
                  y1={y}
                  x2={LEFT + PLOT_WIDTH}
                  y2={y}
                  stroke="var(--border-hairline)"
                  strokeWidth="1"
                  opacity={tick === 0 ? 1 : 0.7}
                />
                <text
                  x={LEFT - 10}
                  y={y + 3.5}
                  textAnchor="end"
                  fontSize="10.5"
                  fill="var(--ink-3)"
                  className="tabular-nums"
                >
                  {compact(tick, locale)}
                </text>
              </g>
            )
          })}

          <path d={area} fill={`url(#${gradientId})`} />
          <path
            d={line}
            fill="none"
            stroke="var(--brand)"
            strokeWidth="2.2"
            strokeLinecap="round"
            strokeLinejoin="round"
          />

          {last !== undefined ? (
            <>
              <circle
                cx={last.x}
                cy={last.y}
                r="6"
                fill="var(--brand)"
                opacity="0.14"
              />
              <circle
                cx={last.x}
                cy={last.y}
                r="3.4"
                fill="var(--brand)"
                stroke="var(--surface-card)"
                strokeWidth="2"
              />
            </>
          ) : null}

          {labelIndexes.map((index) => {
            const point = points[index]
            if (point === undefined) return null
            return (
              <text
                key={point.ts}
                x={point.x}
                y={BASELINE + 18}
                textAnchor={index === 0 ? 'start' : index === points.length - 1 ? 'end' : 'middle'}
                fontSize="10.5"
                fill="var(--ink-3)"
                className="tabular-nums"
              >
                {tickTime(point.ts, granularity, locale)}
              </text>
            )
          })}

          {active !== null ? (
            <>
              <line
                x1={active.x}
                y1={TOP}
                x2={active.x}
                y2={BASELINE}
                stroke="var(--ink-3)"
                strokeWidth="1"
                strokeDasharray="3 4"
              />
              <circle
                cx={active.x}
                cy={active.y}
                r="3.6"
                fill="var(--brand)"
                stroke="var(--surface-card)"
                strokeWidth="2"
              />
            </>
          ) : null}

          <rect
            x={LEFT}
            y={TOP}
            width={PLOT_WIDTH}
            height={PLOT_HEIGHT}
            fill="transparent"
            onMouseMove={handleMove}
            onMouseLeave={() => setHover(null)}
          />
        </svg>
      </div>

      {active !== null ? (
        <div
          className="pointer-events-none absolute z-10 -translate-x-1/2 -translate-y-full rounded-md border border-border-hairline bg-surface-card px-2 py-1 text-badge shadow-sm"
          style={{
            left: `${(active.x / WIDTH) * 100}%`,
            top: `${(active.y / HEIGHT) * 100}%`,
          }}
        >
          <span className="font-mono tabular-nums text-ink-1">
            {full(active.value, locale)} {unit}
          </span>
          <span className="ml-2 text-ink-3">
            {tickTime(active.ts, granularity, locale)}
          </span>
        </div>
      ) : null}

      <table className="sr-only">
        <caption>
          {t('common.chart.aria', {
            kind: 'area',
            count: 1,
            series: series.label,
            unit,
          })}
        </caption>
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
          {points.map((point) => (
            <tr key={point.ts}>
              <td>{series.label}</td>
              <td>{point.ts}</td>
              <td>{point.value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
