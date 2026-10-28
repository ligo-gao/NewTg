import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
  type PointerEvent as ReactPointerEvent,
  type RefObject,
} from 'react'

import { nextIndex, selectNearest } from '@/shared/lib/chart-selection'
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
 * 只有「指针/键盘选最近点」这两段纯逻辑与 AnalyticsChart 共用
 * `shared/lib/chart-selection.ts`；渲染与样式不共享。
 *
 * 刻意不做动画（没有 transition / animation），所以在
 * `prefers-reduced-motion` 下不需要额外分支。
 */

const VIEW_WIDTH_FALLBACK = 720
const VIEW_HEIGHT = 220
const PLOT_LEFT = 48
const PLOT_RIGHT = 12
const PLOT_TOP = 12
const PLOT_BOTTOM = 28
const Y_TICK_COUNT = 4
const X_TICK_COUNT = 5
const HOUR_MS = 60 * 60 * 1000

export interface LineChartSeries {
  key: string
  label: string
  /** CSS 颜色表达式，由调用方按设计稿指定，例如 `var(--chart-cat-1)`。 */
  color: string
  /** 数据表表头文案（例如 `detail.table_p50`）；缺省时用 `label`。 */
  tableLabel?: string
  /**
   * `[x, value]`；x 是 epoch 毫秒，组件按数值定位而不是分桶。value 为
   * `null` 表示该点没有有限值（例如越过 30s 的分位）：折线在 null 处断开。
   * 未列入 `overAt` 的 null 点才显示 `—`；y 域始终忽略这些点。
   */
  points: readonly [number, number | null][]
  /** 该序列里「已知 ≥30s、但无有限值」的 x 集合；点仍为 null（断线），
   *  但 tooltip 与数据表显示 `>30s` 而不是 `—`。 */
  overAt?: ReadonlySet<number>
  /** 与 `points` 按索引对齐的样本数；提供时才渲染样本列。 */
  samples?: (number | null)[]
}

export interface LineChartProps {
  series: LineChartSeries[]
  /** 图表的无障碍名（`aria-label` 的 `{kind}` 与 `<title>`）。 */
  title: string
  /** tooltip 与图例的单位，例如 `'ms'`；空串表示无单位。 */
  unit?: string
  /** y 轴刻度文案；默认取整后直接显示。 */
  tickFormatter?: (value: number) => string
}

/**
 * 量出容器宽度。jsdom 没有 ResizeObserver，测量前也先退回 720，保证
 * 服务端渲染与测试都拿到确定的 viewBox；真实浏览器首帧后立即按实际宽度
 * 重绘，viewBox 宽高比与渲染盒一致，文字不会被拉伸。
 */
function useElementWidth(): [RefObject<HTMLDivElement | null>, number] {
  const ref = useRef<HTMLDivElement | null>(null)
  const [width, setWidth] = useState(VIEW_WIDTH_FALLBACK)

  useEffect(() => {
    const element = ref.current
    if (element === null || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0]
      if (entry === undefined) return
      const measured =
        entry.borderBoxSize?.[0]?.inlineSize ?? entry.contentRect.width
      if (Number.isFinite(measured) && measured > 0) setWidth(measured)
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [])

  return [ref, width]
}

function formatValue(value: number, locale: string): string {
  if (!Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(
    value,
  )
}

function withUnit(value: string, unit: string): string {
  return unit === '' ? value : `${value} ${unit}`
}

/**
 * 某序列在 x 处的读数。有限值按 locale 格式化；没有有限值但在 `overAt` 里
 * 说明「已知 ≥30s」——这是读数，不是缺样本，必须显示 `>30s`。其余才是 `—`。
 */
function readout(
  item: LineChartSeries,
  x: number,
  value: number | undefined,
  locale: string,
  unit: string,
): string {
  if (value !== undefined) return withUnit(formatValue(value, locale), unit)
  return item.overAt?.has(x) ? '>30s' : '—'
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

/** 跨本地午夜或窗口超过 12 小时时补月/日，否则只显示时分。 */
function crossesLocalMidnight(min: number, max: number): boolean {
  const start = new Date(min)
  const end = new Date(max)
  return (
    start.getFullYear() !== end.getFullYear() ||
    start.getMonth() !== end.getMonth() ||
    start.getDate() !== end.getDate()
  )
}

export function LineChart({
  series,
  title,
  unit = '',
  tickFormatter,
}: LineChartProps) {
  const { t, locale } = useI18n()
  const [containerRef, width] = useElementWidth()
  const [selected, setSelected] = useState<number | null>(null)
  /**
   * tooltip 的实测尺寸，用于判断贴边翻转。没测到之前（jsdom 的
   * offsetWidth/Height 全是 0、或浮层刚挂载）退回 120×72 的安全值。
   */
  const tooltipRef = useRef<HTMLDivElement | null>(null)
  const [tooltipSize, setTooltipSize] = useState({ width: 120, height: 72 })

  const plotWidth = Math.max(width - PLOT_LEFT - PLOT_RIGHT, 1)
  const plotHeight = VIEW_HEIGHT - PLOT_TOP - PLOT_BOTTOM

  const xs = [
    ...new Set(
      series.flatMap((item) =>
        item.points.map(([x]) => x).filter((x) => Number.isFinite(x)),
      ),
    ),
  ].sort((a, b) => a - b)

  const valueMaps = new Map(
    series.map((item) => {
      const map = new Map<number, number>()
      for (const [x, y] of item.points) {
        if (Number.isFinite(x) && typeof y === 'number' && Number.isFinite(y)) {
          map.set(x, y)
        }
      }
      return [item.key, map] as const
    }),
  )

  const allValues = series.flatMap((item) =>
    item.points.flatMap(([, y]) =>
      typeof y === 'number' && Number.isFinite(y) ? [y] : [],
    ),
  )
  const rawMax = allValues.length > 0 ? Math.max(...allValues) : 0
  const yMax = niceMax(rawMax > 0 ? rawMax : 1)

  const xMin = xs[0] ?? 0
  const xMax = xs[xs.length - 1] ?? 0
  const axisSpan = xMax - xMin
  const spansDays = xs.length > 1 && crossesLocalMidnight(xMin, xMax)
  const formatXAxis = (timestamp: number): string =>
    new Intl.DateTimeFormat(
      locale,
      spansDays || axisSpan > 12 * HOUR_MS
        ? {
            month: 'numeric',
            day: 'numeric',
            hour: '2-digit',
            minute: '2-digit',
          }
        : { hour: '2-digit', minute: '2-digit' },
    ).format(timestamp)

  const formatYTick = tickFormatter ?? ((value: number) => formatValue(value, locale))

  const hasSamples = series.some((item) => item.samples !== undefined)
  const rowSamples = hasSamples ? xs.map((x) => sampleAt(series, x)) : []

  const seriesLabels = series.map((item) => item.label).join(', ')
  const ariaLabel = t('common.chart.aria', {
    kind: title,
    count: series.length,
    series: seriesLabels,
    // 没有单位时给一个占位符，避免无障碍名读成 “Unit: .”。
    unit: unit === '' ? '—' : unit,
  })

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
    // 24h 跨度的日期+时间刻度约 80px 宽；窄容器只放 3 个，避免互相压字。
    const tickCount = width < 480 ? 3 : X_TICK_COUNT
    const count = Math.min(tickCount, xs.length)
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
  const activeX = selectedIndex === null ? null : (xs[selectedIndex] ?? null)

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
  const activeTop = activeYs.length > 0 ? Math.min(...activeYs) : PLOT_TOP
  const activeXPos = activeX === null ? 0 : xFor(activeX)

  // 浮层内容（时间 + 每序列一行）会改变自身高度，不能只看数据点的 y。
  useLayoutEffect(() => {
    const node = tooltipRef.current
    if (node === null) return
    const measuredWidth = node.offsetWidth
    const measuredHeight = node.offsetHeight
    if (measuredWidth > 0 && measuredHeight > 0) {
      setTooltipSize((current) =>
        current.width === measuredWidth && current.height === measuredHeight
          ? current
          : { width: measuredWidth, height: measuredHeight },
      )
    }
  }, [activeX])

  const activeAnnouncement =
    activeX === null
      ? ''
      : `${formatClock(activeX, locale)} · ${activeEntries
          .map(
            ({ item, value }) =>
              `${item.label}: ${readout(item, activeX, value, locale, unit)}`,
          )
          .join(' · ')}`

  /** 指针位置换算到 viewBox 坐标后，取水平距离最近的那个 x。 */
  function selectNearestPoint(svg: SVGSVGElement, clientX: number): void {
    if (xs.length === 0) return
    const bounds = svg.getBoundingClientRect()
    if (bounds.width === 0) return
    const localX = ((clientX - bounds.left) / bounds.width) * width
    setSelected(
      selectNearest(
        xs.map((x) => xFor(x)),
        localX,
        width,
      ),
    )
  }

  // jsdom 没有完整的 PointerEvent 实现，真实鼠标也可能只发 mousemove；
  // 两个事件都接，触摸则走 pointerdown，行为一致。
  function handlePointer(
    event: ReactPointerEvent<SVGSVGElement> | ReactMouseEvent<SVGSVGElement>,
  ): void {
    selectNearestPoint(event.currentTarget, event.clientX)
  }

  function handleKeyDown(event: ReactKeyboardEvent<SVGSVGElement>): void {
    if (xs.length === 0) return
    if (event.key === 'Escape') {
      setSelected(null)
      return
    }
    const next = nextIndex(event.key, selected, xs.length)
    if (next === null) return
    event.preventDefault()
    setSelected((current) => nextIndex(event.key, current, xs.length) ?? current)
  }

  return (
    <div data-testid="line-chart" ref={containerRef} className="w-full">
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
          aria-label={ariaLabel}
          viewBox={`0 0 ${width} ${VIEW_HEIGHT}`}
          width="100%"
          height={VIEW_HEIGHT}
          className="h-[220px] w-full focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          tabIndex={0}
          onPointerMove={handlePointer}
          onPointerDown={handlePointer}
          onMouseMove={handlePointer}
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
                {formatYTick(tick)}
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
                {formatXAxis(tick)}
              </text>
            ))}
          </g>

          {series.map((item) => {
            // x 升序后逐点铺路径；null 或非法值结束当前子路径，下一个有限
            // 点重新起一条 `M`——折线在空洞处真正断开，而不是跨洞连线。
            const ordered = [...item.points].sort((a, b) => a[0] - b[0])
            const commands: string[] = []
            let drawing = false
            let last: [number, number] | null = null
            for (const [x, y] of ordered) {
              if (
                !Number.isFinite(x) ||
                typeof y !== 'number' ||
                !Number.isFinite(y)
              ) {
                drawing = false
                continue
              }
              commands.push(
                `${drawing ? 'L' : 'M'}${xFor(x).toFixed(1)},${yFor(y).toFixed(1)}`,
              )
              drawing = true
              last = [x, y]
            }
            const d = commands.join(' ')
            if (d === '' || last === null) return null
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
                <circle
                  cx={xFor(last[0])}
                  cy={yFor(last[1])}
                  r={3.5}
                  fill={item.color}
                  stroke="var(--surface-card)"
                  strokeWidth={2}
                  vectorEffect="non-scaling-stroke"
                />
              </g>
            )
          })}

          {activeX === null ? null : (
            <g data-testid="chart-crosshair" aria-hidden="true">
              <line
                data-crosshair="true"
                x1={activeXPos}
                y1={PLOT_TOP}
                x2={activeXPos}
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
                    cx={activeXPos}
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
            ref={tooltipRef}
            className={[
              'pointer-events-none absolute z-10 rounded-md border border-border-hairline bg-surface-card px-2 py-1 text-badge shadow-sm',
              activeXPos + tooltipSize.width + 8 > plotWidth
                ? '-translate-x-full -ml-1'
                : 'ml-1',
              activeTop < PLOT_TOP + tooltipSize.height + 8
                ? 'mt-1'
                : '-translate-y-full -mt-1',
            ].join(' ')}
            style={{
              left: `${(activeXPos / width) * 100}%`,
              top: `${(activeTop / VIEW_HEIGHT) * 100}%`,
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
                {readout(item, activeX, value, locale, unit)}
              </span>
            ))}
          </div>
        )}
      </div>

      {/* 屏幕阅读器播报当前选中点；浮层只服务视觉用户，且始终挂载。 */}
      <p data-testid="chart-live" aria-live="polite" className="sr-only">
        {activeAnnouncement}
      </p>

      <details className="mt-3">
        <summary className="cursor-pointer text-badge text-ink-2">
          {t('admin.performance.detail.table_toggle')}
        </summary>
        <div className="mt-2 overflow-x-auto">
          <table className="w-full text-badge tabular-nums">
            <caption className="sr-only">
              {`${title} — ${t('common.chart.table.time')}, ${t('common.chart.table.series')}, ${t('common.chart.table.value')}`}
            </caption>
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
                    {item.tableLabel ?? item.label}
                  </th>
                ))}
                {hasSamples ? (
                  <th scope="col" className="text-right font-normal text-ink-3">
                    {t('admin.performance.detail.table_samples')}
                  </th>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {xs.map((x, rowIndex) => {
                const sample = rowSamples[rowIndex]
                return (
                  <tr key={x}>
                    <th
                      scope="row"
                      className="text-left font-normal text-ink-2"
                    >
                      {formatXAxis(x)}
                    </th>
                    {series.map((item) => {
                      const value = valueMaps.get(item.key)?.get(x)
                      return (
                        <td key={item.key} className="text-right text-ink-1">
                          {readout(item, x, value, locale, '')}
                        </td>
                      )
                    })}
                    {hasSamples ? (
                      <td className="text-right text-ink-1">
                        {sample === null || sample === undefined
                          ? '—'
                          : formatValue(sample, locale)}
                      </td>
                    ) : null}
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </details>
    </div>
  )
}

/**
 * 行级样本数：`samples` 与 `points` 按索引对齐，所以先找到该 x 在
 * series.points 中的位置；缺样本或该序列没有点都返回 undefined。
 */
function sampleAt(
  series: LineChartSeries[],
  x: number,
): number | null | undefined {
  let sawMissing = false
  for (const item of series) {
    if (item.samples === undefined) continue
    const index = item.points.findIndex(([px]) => px === x)
    if (index === -1) continue
    const sample = item.samples[index]
    if (sample === undefined) continue
    // 该序列在这个 x 上没有样本：继续找其它序列，不能把 null 当成答案。
    if (sample === null) {
      sawMissing = true
      continue
    }
    return sample
  }
  return sawMissing ? null : undefined
}
