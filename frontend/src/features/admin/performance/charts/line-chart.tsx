import {
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import { formatClock } from '../utils'

/**
 * 性能详情用的手写折线图。**不复用** shared/ui/analytics-chart.tsx，因为那份
 * 契约与这里的展示要求有三处互斥：
 * 1. 颜色槽位必须由调用方按设计稿指定（p50 = --chart-cat-1、p95 =
 *    --chart-cat-3），而 AnalyticsChart 按序列序号自动分配颜色；
 * 2. 等价数据表必须是可展开的 `<details>`（设计稿的「查看数据表」），
 *    而 AnalyticsChart 的表格是视觉隐藏的 sr-only 表；
 * 3. x 轴是数值（epoch 毫秒），而 AnalyticsChart 把时间字符串分桶到
 *    固定槽位。
 *
 * 刻意不做动画（没有 transition / animation），所以在
 * `prefers-reduced-motion` 下不需要额外分支。
 */

const VIEW_WIDTH = 720
const PLOT_LEFT = 48
const PLOT_RIGHT = 12
const PLOT_TOP = 12
const PLOT_BOTTOM = 28
const DEFAULT_HEIGHT = 220
const Y_TICK_COUNT = 4
const X_TICK_COUNT = 5

export interface LineChartSeries {
  key: string
  label: string
  /** CSS 颜色表达式，由调用方按设计稿指定，例如 `var(--chart-cat-1)`。 */
  color: string
  /** `[x, value]`；x 是 epoch 毫秒，组件按数值定位而不是分桶。 */
  points: [number, number][]
}

export interface LineChartProps {
  series: LineChartSeries[]
  /** 图表的无障碍名（`aria-label` 与 `<title>`）。 */
  title: string
  /** tooltip 与数据表表头的单位，例如 `'ms'`。 */
  unit: string
  height?: number
}

function formatValue(value: number, locale: string): string {
  if (!Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(
    value,
  )
}

/** 把原始上限向上取整到 1/1.5/2/2.5/3/4/5/7.5 倍的十次幂，刻度才可读。 */
function niceStep(raw: number): number {
  if (!Number.isFinite(raw) || raw <= 0) return 1
  const exponent = Math.floor(Math.log10(raw))
  const magnitude = 10 ** exponent
  const normalized = raw / magnitude
  const steps = [1, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10]
  const nice = steps.find((step) => normalized <= step) ?? 10
  return nice * magnitude
}

function niceMax(raw: number): number {
  return niceStep(raw / Y_TICK_COUNT) * Y_TICK_COUNT
}

export function LineChart({
  series,
  title,
  unit,
  height = DEFAULT_HEIGHT,
}: LineChartProps) {
  const { t, locale } = useI18n()
  const [selected, setSelected] = useState<number | null>(null)

  const viewHeight = Math.max(height, PLOT_TOP + PLOT_BOTTOM + 40)
  const plotWidth = VIEW_WIDTH - PLOT_LEFT - PLOT_RIGHT
  const plotHeight = viewHeight - PLOT_TOP - PLOT_BOTTOM

  const xs = [
    ...new Set(
      series.flatMap((item) =>
        item.points
          .map(([x]) => x)
          .filter((x) => Number.isFinite(x)),
      ),
    ),
  ].sort((a, b) => a - b)

  const valueMaps = new Map(
    series.map((item) => [
      item.key,
      new Map(
        item.points.filter(
          ([x, y]) => Number.isFinite(x) && Number.isFinite(y),
        ),
      ),
    ]),
  )

  const allValues = series.flatMap((item) =>
    item.points
      .map(([, y]) => y)
      .filter((value) => Number.isFinite(value)),
  )
  const rawMax = allValues.length > 0 ? Math.max(...allValues) : 0
  const yMax = niceMax(rawMax > 0 ? rawMax : 1)

  const xMin = xs[0] ?? 0
  const xMax = xs[xs.length - 1] ?? 0

  function xFor(value: number): number {
    if (xs.length <= 1) return PLOT_LEFT + plotWidth / 2
    return PLOT_LEFT + ((value - xMin) / (xMax - xMin)) * plotWidth
  }

  function yFor(value: number): number {
    const clamped = Math.min(Math.max(value, 0), yMax)
    return PLOT_TOP + (1 - clamped / yMax) * plotHeight
  }

  const yTicks = Array.from(
    { length: Y_TICK_COUNT + 1 },
    (_, index) => (yMax / Y_TICK_COUNT) * index,
  )

  const xTicks: number[] = []
  if (xs.length > 0) {
    const count = Math.min(X_TICK_COUNT, xs.length)
    for (let index = 0; index < count; index += 1) {
      const at =
        count === 1 ? 0 : Math.round((index * (xs.length - 1)) / (count - 1))
      const value = xs[at]
      if (value !== undefined && !xTicks.includes(value)) xTicks.push(value)
    }
  }

  const selectedIndex =
    selected === null
      ? null
      : Math.min(Math.max(selected, 0), Math.max(xs.length - 1, 0))
  const activeX =
    selectedIndex === null ? null : (xs[selectedIndex] ?? null)

  const activeEntries =
    activeX === null
      ? []
      : series.map((item) => ({
          item,
          value: valueMaps.get(item.key)?.get(activeX),
        }))
  const activeYs = activeEntries.flatMap(({ value }) =>
    value === undefined ? [] : [yFor(value)],
  )
  const activeTop =
    activeYs.length > 0 ? Math.min(...activeYs) : PLOT_TOP

  /** 指针位置换算到 viewBox 坐标后，取水平距离最近的那个 x。 */
  function selectNearest(svg: SVGSVGElement, clientX: number): void {
    if (xs.length === 0) return
    const bounds = svg.getBoundingClientRect()
    if (bounds.width === 0) return
    const localX = ((clientX - bounds.left) / bounds.width) * VIEW_WIDTH
    let nearest = 0
    let best = Number.POSITIVE_INFINITY
    xs.forEach((x, index) => {
      const distance = Math.abs(xFor(x) - localX)
      if (distance < best) {
        best = distance
        nearest = index
      }
    })
    setSelected(nearest)
  }

  // jsdom 没有完整的 PointerEvent 实现，真实鼠标也可能只发 mousemove；
  // 两个事件都接，行为一致。
  function handlePointerMove(
    event: ReactPointerEvent<SVGSVGElement> | ReactMouseEvent<SVGSVGElement>,
  ): void {
    selectNearest(event.currentTarget, event.clientX)
  }

  function handleKeyDown(event: ReactKeyboardEvent<SVGSVGElement>): void {
    if (xs.length === 0) return
    const last = xs.length - 1
    if (event.key === 'ArrowLeft') {
      event.preventDefault()
      setSelected((current) =>
        current === null ? last : Math.max(0, current - 1),
      )
      return
    }
    if (event.key === 'ArrowRight') {
      event.preventDefault()
      setSelected((current) =>
        current === null ? 0 : Math.min(last, current + 1),
      )
      return
    }
    if (event.key === 'Home') {
      event.preventDefault()
      setSelected(0)
      return
    }
    if (event.key === 'End') {
      event.preventDefault()
      setSelected(last)
      return
    }
    if (event.key === 'Escape') {
      setSelected(null)
    }
  }

  return (
    <div data-testid="line-chart" className="w-full">
      {series.length >= 2 ? (
        // 两条以上必须有图例：色块与文字同时出现，不靠颜色单独传达序列。
        <ul className="mb-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-badge text-ink-2">
          {series.map((item) => (
            <li key={item.key} className="flex items-center gap-1.5">
              <span
                aria-hidden="true"
                className="inline-block size-2 rounded-sm"
                style={{ backgroundColor: item.color }}
              />
              <span>{item.label}</span>
            </li>
          ))}
        </ul>
      ) : null}

      <div className="relative">
        <svg
          role="img"
          aria-label={title}
          viewBox={`0 0 ${VIEW_WIDTH} ${viewHeight}`}
          className="h-auto w-full focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          tabIndex={0}
          onPointerMove={handlePointerMove}
          onMouseMove={handlePointerMove}
          onPointerLeave={() => setSelected(null)}
          onMouseLeave={() => setSelected(null)}
          onKeyDown={handleKeyDown}
          onBlur={() => setSelected(null)}
        >
          <title>{title}</title>

          {/* 单 Y 轴：网格线与刻度文字都用中性 token，不用序列色。 */}
          <g aria-hidden="true">
            {yTicks.map((tick) => (
              <line
                key={`grid-${tick}`}
                x1={PLOT_LEFT}
                y1={yFor(tick)}
                x2={PLOT_LEFT + plotWidth}
                y2={yFor(tick)}
                stroke="var(--border-hairline)"
                strokeWidth={1}
                vectorEffect="non-scaling-stroke"
              />
            ))}
            {yTicks.map((tick) => (
              <text
                key={`y-${tick}`}
                x={PLOT_LEFT - 8}
                y={yFor(tick) + 3}
                textAnchor="end"
                fontSize={10}
                fill="var(--ink-3)"
                className="tabular-nums"
              >
                {formatValue(tick, locale)}
              </text>
            ))}
            {xTicks.map((tick, index) => (
              <text
                key={`x-${tick}`}
                x={xFor(tick)}
                y={PLOT_TOP + plotHeight + 18}
                textAnchor={
                  index === 0
                    ? 'start'
                    : index === xTicks.length - 1
                      ? 'end'
                      : 'middle'
                }
                fontSize={10}
                fill="var(--ink-3)"
                className="tabular-nums"
              >
                {formatClock(tick, locale)}
              </text>
            ))}
          </g>

          {series.map((item) => {
            const ordered = item.points
              .filter(([x, y]) => Number.isFinite(x) && Number.isFinite(y))
              .slice()
              .sort((a, b) => a[0] - b[0])
            const d = ordered
              .map(
                ([x, y], index) =>
                  `${index === 0 ? 'M' : 'L'}${xFor(x).toFixed(1)},${yFor(y).toFixed(1)}`,
              )
              .join(' ')
            if (d === '') return null
            const last = ordered[ordered.length - 1]
            return (
              <g key={item.key}>
                <path
                  data-series-key={item.key}
                  d={d}
                  fill="none"
                  stroke={item.color}
                  strokeWidth={2}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  vectorEffect="non-scaling-stroke"
                />
                {last === undefined ? null : (
                  <circle
                    cx={xFor(last[0])}
                    cy={yFor(last[1])}
                    r={3.5}
                    fill={item.color}
                    stroke="var(--surface-card)"
                    strokeWidth={2}
                    vectorEffect="non-scaling-stroke"
                  />
                )}
              </g>
            )
          })}

          {activeX === null ? null : (
            <g data-testid="chart-crosshair" aria-hidden="true">
              <line
                data-crosshair="true"
                x1={xFor(activeX)}
                y1={PLOT_TOP}
                x2={xFor(activeX)}
                y2={PLOT_TOP + plotHeight}
                stroke="var(--border-default)"
                strokeWidth={1}
                strokeDasharray="3 3"
                vectorEffect="non-scaling-stroke"
              />
              {activeEntries.map(({ item, value }) =>
                value === undefined ? null : (
                  <circle
                    key={item.key}
                    cx={xFor(activeX)}
                    cy={yFor(value)}
                    r={4}
                    fill={item.color}
                    stroke="var(--surface-card)"
                    strokeWidth={2}
                    vectorEffect="non-scaling-stroke"
                  />
                ),
              )}
            </g>
          )}
        </svg>

        {activeX === null ? null : (
          <div
            data-testid="chart-tip"
            aria-live="polite"
            className="pointer-events-none absolute z-10 -translate-y-full rounded-md border border-border-hairline bg-surface-card px-2 py-1 text-badge shadow-sm"
            style={{
              left: `${(xFor(activeX) / VIEW_WIDTH) * 100}%`,
              top: `${(activeTop / viewHeight) * 100}%`,
            }}
          >
            <span className="block font-mono tabular-nums text-ink-1">
              {formatClock(activeX, locale)}
            </span>
            {activeEntries.map(({ item, value }) => (
              <span
                key={item.key}
                className="block whitespace-nowrap text-ink-2"
              >
                {item.label}:{' '}
                {value === undefined
                  ? '—'
                  : `${formatValue(value, locale)} ${unit}`}
              </span>
            ))}
          </div>
        )}
      </div>

      <details className="mt-3">
        <summary className="cursor-pointer text-badge text-ink-2">
          {t('admin.performance.detail.table_toggle')}
        </summary>
        <div className="mt-2 overflow-x-auto">
          <table className="w-full text-badge tabular-nums">
            <thead>
              <tr>
                <th scope="col" className="text-left font-normal text-ink-3">
                  {t('admin.performance.detail.table_time')}
                </th>
                {series.map((item) => (
                  <th
                    key={item.key}
                    scope="col"
                    className="text-right font-normal text-ink-3"
                  >
                    {unit === '' ? item.label : `${item.label} (${unit})`}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {xs.map((x) => (
                <tr key={x}>
                  <td className="text-left text-ink-2">
                    {formatClock(x, locale)}
                  </td>
                  {series.map((item) => {
                    const value = valueMaps.get(item.key)?.get(x)
                    return (
                      <td
                        key={item.key}
                        className="text-right text-ink-1"
                      >
                        {value === undefined
                          ? '—'
                          : formatValue(value, locale)}
                      </td>
                    )
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </div>
  )
}
