import {
  useMemo,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react'

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
  /**
   * 水平参考线（例如对账容差带）。它们画在绘图区内、共用同一根 Y 轴，
   * 只是视觉参考而不是第二个数据序列，因此不进入 `series`。标签会追加到
   * `aria-label` 摘要里，保证信息不只靠颜色/虚线传达。
   */
  referenceLines?: { value: number; label: string }[]
  /**
   * Opt-in hover/keyboard readout. 关闭时组件渲染的 DOM 与旧版完全一致（没有
   * tabindex、事件处理器或额外节点）；开启后悬停/方向键选中最近的数据点，
   * 显示十字线、标记与数值标签，并通过 aria-live 播报。
   */
  tooltip?: boolean
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

/** 可被悬停/键盘选中的数据点（已换算成绘图坐标）。 */
interface HoverPoint {
  ts: string
  value: number
  x: number
  y: number
  color: string
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
function buildModel(
  series: ChartSeries[],
  kind: ChartConfig['kind'],
  domain?: readonly [number, number],
): ChartModel {
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
  if (domain !== undefined) {
    // 固定值域（例如百分比 0–100）：轴不随数据缩放，0% 与 100% 始终在
    // 同一位置，跨窗口/跨模型可比较。
    min = domain[0]
    max = domain[1]
    if (max === min) max = min + 1
  } else if (values.length > 0) {
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

export function AnalyticsChart({
  series,
  config,
  referenceLines,
  tooltip,
}: AnalyticsChartProps) {
  const { t, locale } = useI18n()
  const [selected, setSelected] = useState<number | null>(null)
  const model = useMemo(
    () => buildModel(series, config.kind, config.domain),
    [series, config.kind, config.domain],
  )

  // 参考线标签是纯文本替代的一部分：读屏用户听得到「容差 ±50 bp」，
  // 不必依赖视觉上的虚线位置。相同标签只播报一次（± 带共用一个标签）。
  const referenceSummary =
    referenceLines === undefined
      ? ''
      : [...new Set(referenceLines.map((line) => line.label))].join(', ')
  const baseAriaLabel = t('common.chart.aria', {
    kind: config.kind,
    count: series.length,
    series: series.map((item) => item.label).join(', '),
    unit: config.unit,
  })
  const ariaLabel =
    referenceSummary === ''
      ? baseAriaLabel
      : `${baseAriaLabel} ${referenceSummary}`

  if (!model.hasData) {
    return (
      <div
        data-testid="analytics-chart"
        data-kind={config.kind}
        data-empty="true"
        className="flex min-h-40 items-center justify-center rounded-lg border border-dashed border-neutral-line text-body text-neutral"
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

  // 悬停/键盘读出的候选集：所有序列的点按绘图 x 升序，方向键顺序与视觉
  // 从左到右一致；多序列同 x 时保留声明顺序。
  const hoverPoints: HoverPoint[] = seriesPoints
    .flatMap((points, seriesIndex) =>
      points.flatMap((point) => {
        const bucket = bucketIndex.get(point.ms)
        if (bucket === undefined) return []
        return [
          {
            ts: point.ts,
            value: point.value,
            x: xForIndex(bucket, bucketCount),
            y: yFor(point.value, min, max),
            color: colorForSeries(seriesIndex),
          },
        ]
      }),
    )
    .sort((a, b) => a.x - b.x)

  // 读取时再 clamp 一次：数据换窗口导致候选集缩短时，保留的索引不会越界。
  const clampedSelection =
    selected === null
      ? null
      : Math.min(Math.max(selected, 0), Math.max(hoverPoints.length - 1, 0))
  const active =
    tooltip === true && clampedSelection !== null
      ? (hoverPoints[clampedSelection] ?? null)
      : null

  /** 指针位置换算到 viewBox 坐标后，取水平距离最近的数据点。 */
  function selectNearest(svg: SVGSVGElement, clientX: number): void {
    if (hoverPoints.length === 0) return
    const bounds = svg.getBoundingClientRect()
    if (bounds.width === 0) return
    const x = ((clientX - bounds.left) / bounds.width) * VIEW_WIDTH
    let nearest = 0
    let best = Number.POSITIVE_INFINITY
    hoverPoints.forEach((point, index) => {
      const distance = Math.abs(point.x - x)
      if (distance < best) {
        best = distance
        nearest = index
      }
    })
    setSelected(nearest)
  }

  // 鼠标与指针事件都接：测试环境（jsdom）没有完整 PointerEvent 实现，
  // 真实设备上触摸走 pointerdown，鼠标走 pointermove/mousemove。
  function handlePointerMove(
    event: ReactPointerEvent<SVGSVGElement> | ReactMouseEvent<SVGSVGElement>,
  ): void {
    selectNearest(event.currentTarget, event.clientX)
  }

  function handleKeyDown(event: ReactKeyboardEvent<SVGSVGElement>): void {
    if (hoverPoints.length === 0) return
    const last = hoverPoints.length - 1
    if (event.key === 'ArrowLeft') {
      event.preventDefault()
      // 尚未选中时从右端进入；之后逐步左移并 clamp 到 0。
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
    <div
      data-testid="analytics-chart"
      data-kind={config.kind}
      data-series-count={series.length}
      // 只有需要定位浮层时才加 relative：关闭 tooltip 时包装层与旧版一致。
      className={tooltip === true ? 'relative w-full' : 'w-full'}
    >
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
        className={
          tooltip === true
            ? 'h-auto w-full focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'
            : 'h-auto w-full'
        }
        tabIndex={tooltip === true ? 0 : undefined}
        onPointerMove={tooltip === true ? handlePointerMove : undefined}
        onMouseMove={tooltip === true ? handlePointerMove : undefined}
        onPointerLeave={tooltip === true ? () => setSelected(null) : undefined}
        onMouseLeave={tooltip === true ? () => setSelected(null) : undefined}
        onPointerDown={tooltip === true ? handlePointerMove : undefined}
        onKeyDown={tooltip === true ? handleKeyDown : undefined}
        onBlur={tooltip === true ? () => setSelected(null) : undefined}
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
          className="fill-neutral text-badge tabular-nums"
        >
          {formatTick(max, locale)}
        </text>
        <text
          x={PLOT_LEFT - 6}
          y={BASELINE_Y}
          textAnchor="end"
          className="fill-neutral text-badge tabular-nums"
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
              className="fill-neutral text-badge tabular-nums"
            >
              {formatAxis(new Date(bucket).toISOString(), locale)}
            </text>
          )
        })}
        {/* 参考线共用现有 Y 轴：clamp 到绘图区，绝不另起一根轴。虚线色
            使用非文本专用的 --ink-4，避免与分类色混淆。 */}
        {(referenceLines ?? []).map((line, index) => {
          const y = Math.min(
            Math.max(yFor(line.value, min, max), PLOT_TOP),
            BASELINE_Y,
          )
          return (
            <line
              key={`${line.value}-${String(index)}`}
              data-reference-line="true"
              x1={PLOT_LEFT}
              y1={y}
              x2={PLOT_LEFT + PLOT_WIDTH}
              y2={y}
              stroke="var(--ink-4)"
              strokeDasharray="4 4"
            />
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

            // A single bucket has zero-width geometry: the polygon would be a
            // vertical line with no area and no visible data mark. Draw a
            // minimum-height band plus an 8px marker so the value is visible.
            if (coordinates.length === 1) {
              const height = Math.max(Math.abs(BASELINE_Y - first.y), 2)
              const y =
                first.y <= BASELINE_Y ? Math.min(BASELINE_Y - height, first.y) : first.y
              return (
                <g key={item.id} data-series-id={item.id}>
                  <rect
                    data-area-min-height="true"
                    x={first.x - 4}
                    y={y}
                    width={8}
                    height={height}
                    fill={color}
                    fillOpacity={0.18}
                    stroke={color}
                    strokeWidth={2}
                  />
                  <circle
                    data-marker="true"
                    cx={first.x}
                    cy={first.y}
                    r={4}
                    fill={color}
                  />
                </g>
              )
            }

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

          // Matches the one-bucket area case above: a one-coordinate polyline
          // has no length and would leave the chart blank. The marker makes the
          // point visible without changing the surrounding contract.
          if (coordinates.length === 1) {
            const only = coordinates[0]
            if (only === undefined) return null
            return (
              <g key={item.id} data-series-id={item.id}>
                <polyline
                  points={`${only.x},${only.y}`}
                  fill="none"
                  stroke={color}
                  strokeWidth={2}
                />
                <circle
                  data-marker="true"
                  cx={only.x}
                  cy={only.y}
                  r={4}
                  fill={color}
                />
              </g>
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
        {active !== null ? (
          // 选中态的画内标记：十字线共用绘图区高度，圆点落在数据值上。
          <g data-testid="analytics-chart-selection">
            <line
              data-crosshair="true"
              x1={active.x}
              y1={PLOT_TOP}
              x2={active.x}
              y2={BASELINE_Y}
              stroke="var(--ink-3)"
              strokeDasharray="3 4"
            />
            <circle
              data-hover-marker="true"
              cx={active.x}
              cy={active.y}
              r={3.5}
              fill={active.color}
              stroke="var(--surface-card)"
              strokeWidth={2}
            />
          </g>
        ) : null}
      </svg>

      {/* 屏幕阅读器播报选中值；视觉用户看到的是下方浮层标签。 */}
      {tooltip === true ? (
        <p
          data-testid="analytics-chart-live"
          aria-live="polite"
          className="sr-only"
        >
          {active === null
            ? ''
            : `${formatAxis(active.ts, locale)} · ${formatTick(active.value, locale)} ${config.unit}`}
        </p>
      ) : null}

      {/* 数值标签贴近选中点；靠近绘图区右/上边缘时翻到另一侧，避免溢出。 */}
      {active !== null ? (
        <div
          data-testid="analytics-chart-tooltip"
          className={[
            'pointer-events-none absolute z-10 rounded-md border border-border-hairline bg-surface-card px-2 py-1 text-badge shadow-sm',
            active.x > PLOT_LEFT + PLOT_WIDTH * 0.65
              ? '-translate-x-full -ml-1'
              : 'ml-1',
            active.y < PLOT_TOP + 24 ? 'mt-1' : '-translate-y-full -mt-1',
          ].join(' ')}
          style={{
            left: `${(active.x / VIEW_WIDTH) * 100}%`,
            top: `${(active.y / VIEW_HEIGHT) * 100}%`,
          }}
        >
          <span className="font-mono tabular-nums text-ink-1">
            {formatTick(active.value, locale)} {config.unit}
          </span>
        </div>
      ) : null}

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
