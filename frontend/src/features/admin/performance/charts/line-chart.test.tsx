import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import admin from '@/locales/en/admin.json'
import common from '@/locales/en/common.json'
import { I18nContextProvider } from '@/shared/lib/i18n-context'

import { LineChart, type LineChartSeries } from './line-chart'

const MESSAGES: Record<string, string> = {
  'admin.performance.detail.table_toggle':
    admin.performance.detail.table_toggle,
  'admin.performance.detail.table_time': admin.performance.detail.table_time,
  'admin.performance.detail.table_samples':
    admin.performance.detail.table_samples,
  'common.chart.aria': common.chart.aria,
  'common.chart.table.time': common.chart.table.time,
  'common.chart.table.series': common.chart.table.series,
  'common.chart.table.value': common.chart.table.value,
}

function translate(
  key: string,
  values?: Record<string, string | number>,
): string {
  const template = MESSAGES[key] ?? key
  return template.replace(/\{(\w+)\}/g, (_, name: string) =>
    String(values?.[name] ?? ''),
  )
}

const TITLE = 'TTFT trend'
const UNIT = 'ms'
// 本地时间构造，断言里的时分秒才不依赖跑测试的时区。
const T0 = new Date(2026, 9, 28, 10, 0, 0).getTime()
const HOUR = 60 * 60 * 1000
const EXPECTED_LABEL =
  'TTFT trend chart with 2 series: TTFT p50, TTFT p95. Unit: ms.'

const SERIES: LineChartSeries[] = [
  {
    key: 'p50',
    label: 'TTFT p50',
    color: 'var(--chart-cat-1)',
    points: [
      [T0, 100],
      [T0 + HOUR, 120],
      [T0 + 2 * HOUR, 140],
    ],
  },
  {
    key: 'p95',
    label: 'TTFT p95',
    color: 'var(--chart-cat-3)',
    points: [
      [T0, 300],
      [T0 + HOUR, 520],
      [T0 + 2 * HOUR, 400],
    ],
  },
]

function renderChart(series: LineChartSeries[] = SERIES) {
  return render(
    <I18nContextProvider value={{ locale: 'en', t: translate }}>
      <LineChart series={series} title={TITLE} unit={UNIT} />
    </I18nContextProvider>,
  )
}

function resolveSvg(): SVGSVGElement {
  const svg = screen.getByRole('img')
  if (!(svg instanceof SVGSVGElement)) {
    throw new Error('chart svg missing')
  }
  return svg
}

/** jsdom 不做布局；给 SVG 一个与 viewBox 一致的 720×220 盒子。 */
function mockChartBox(svg: SVGSVGElement): void {
  vi.spyOn(svg, 'getBoundingClientRect').mockReturnValue({
    x: 0,
    y: 0,
    left: 0,
    top: 0,
    right: 720,
    bottom: 220,
    width: 720,
    height: 220,
    toJSON: () => ({}),
  })
}

/** jsdom 没有 PointerEvent 构造器：用 MouseEvent 派发同名事件带 clientX。 */
function movePointer(svg: SVGSVGElement, clientX: number): void {
  fireEvent(
    svg,
    new MouseEvent('pointermove', { bubbles: true, clientX }),
  )
}

describe('LineChart', () => {
  it('exposes the common aria summary, a fixed 220 box and one legend per series', () => {
    const { container } = renderChart()
    const svg = resolveSvg()

    expect(svg.getAttribute('aria-label')).toBe(EXPECTED_LABEL)
    expect(svg.querySelector('title')?.textContent).toBe(TITLE)
    // F1：viewBox 宽度取测量值（jsdom 无 ResizeObserver，退回 720），
    // 高度固定 220；宽高属性与 CSS 都钉住同一个盒子。
    expect(svg.getAttribute('viewBox')).toBe('0 0 720 220')
    expect(svg.getAttribute('height')).toBe('220')
    expect(svg.getAttribute('width')).toBe('100%')
    expect(svg.getAttribute('class')).toContain('h-[220px]')

    const legendItems = container.querySelectorAll('ul li')
    expect(legendItems).toHaveLength(2)
    expect(legendItems[0]?.textContent).toBe('TTFT p50')
    expect(legendItems[1]?.textContent).toBe('TTFT p95')
    expect(
      legendItems[0]?.querySelector('span')?.getAttribute('style'),
    ).toContain('--chart-cat-1')
    expect(container.querySelectorAll('path[data-series-key]')).toHaveLength(2)
  })

  it('renders no legend for a single series', () => {
    const only = SERIES[0]
    if (only === undefined) throw new Error('fixture missing')
    const { container } = renderChart([only])

    expect(container.querySelector('ul')).toBeNull()
  })

  it('shows time and every series value in the tooltip on pointermove', () => {
    renderChart()
    const svg = resolveSvg()
    mockChartBox(svg)

    // viewBox 是 720 宽，最后一点在 x = 720 - 12。
    movePointer(svg, 708)

    const tip = screen.getByTestId('chart-tip')
    expect(tip.textContent).toContain('12:00:00 PM')
    expect(tip.textContent).toContain('TTFT p50: 140 ms')
    expect(tip.textContent).toContain('TTFT p95: 400 ms')
    expect(screen.getByTestId('chart-live').textContent).toContain(
      'TTFT p50: 140 ms',
    )
  })

  it('moves the crosshair between two pointer positions', () => {
    renderChart()
    const svg = resolveSvg()
    mockChartBox(svg)

    movePointer(svg, 708)
    const right = svg.querySelector('line[data-crosshair="true"]')
    expect(Number(right?.getAttribute('x1'))).toBe(708)

    movePointer(svg, 48)
    const left = svg.querySelector('line[data-crosshair="true"]')
    expect(Number(left?.getAttribute('x1'))).toBe(48)
    expect(screen.getByTestId('chart-tip').textContent).toContain(
      'TTFT p50: 100 ms',
    )
  })

  it('moves the selected point with the left/right arrow keys', () => {
    renderChart()
    const svg = resolveSvg()

    svg.focus()
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(screen.getByTestId('chart-tip').textContent).toContain(
      'TTFT p50: 100 ms',
    )
    expect(screen.getByTestId('chart-live').textContent).toContain(
      '10:00:00 AM',
    )

    // 焦点留在 SVG 上，第二次 ArrowRight 走到第二个点。
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(screen.getByTestId('chart-tip').textContent).toContain(
      'TTFT p50: 120 ms',
    )

    fireEvent.keyDown(svg, { key: 'ArrowLeft' })
    expect(screen.getByTestId('chart-tip').textContent).toContain(
      'TTFT p50: 100 ms',
    )
  })

  it('renders an expandable data table with one row per x value', () => {
    const { container } = renderChart()
    const first = SERIES[0]
    if (first === undefined) throw new Error('fixture missing')

    const details = container.querySelector('details')
    expect(details).not.toBeNull()
    expect(details?.open).toBe(false)
    expect(details?.querySelector('summary')?.textContent).toBe(
      'View data table',
    )
    expect(details?.querySelector('caption')?.textContent).toContain(TITLE)

    expect(
      screen.getByRole('columnheader', { name: 'Window' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('columnheader', { name: 'TTFT p50' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('columnheader', { name: 'TTFT p95' }),
    ).toBeTruthy()
    expect(container.querySelectorAll('table tbody tr')).toHaveLength(3)
    expect(first.points).toHaveLength(3)
    // 时段列是行表头，便于屏幕阅读器把数值与时间关联。
    expect(container.querySelectorAll('table tbody th[scope="row"]')).toHaveLength(
      3,
    )
  })

  it('renders a samples column only when a series supplies samples', () => {
    const { container, rerender } = renderChart()
    expect(screen.queryByRole('columnheader', { name: 'Samples' })).toBeNull()

    const first = SERIES[0]
    if (first === undefined) throw new Error('fixture missing')
    rerender(
      <I18nContextProvider value={{ locale: 'en', t: translate }}>
        <LineChart
          series={[{ ...first, samples: [42, null, 7] }]}
          title={TITLE}
          unit={UNIT}
        />
      </I18nContextProvider>,
    )

    expect(screen.getByRole('columnheader', { name: 'Samples' })).toBeTruthy()
    const cells = [...container.querySelectorAll('table tbody tr')].map(
      (row) => row.querySelectorAll('td')[1]?.textContent,
    )
    // tableLabel 缺省时用 label，样本列是第二个 td（第一个 td 是 p50 值）。
    expect(cells).toEqual(['42', '—', '7'])
  })

  it('renders no animation or transition classes', () => {
    const { container } = renderChart()
    const animated = [...container.querySelectorAll<HTMLElement>('*')].filter(
      (element) => {
        const className = element.getAttribute('class') ?? ''
        const style = element.getAttribute('style') ?? ''
        return (
          /(^|\s)(animate|transition|duration|ease)(-|$)/.test(className) ||
          /(animation|transition)/.test(style)
        )
      },
    )

    expect(animated).toHaveLength(0)
  })
})
