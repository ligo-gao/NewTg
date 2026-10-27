import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import admin from '@/locales/en/admin.json'
import { I18nContextProvider } from '@/shared/lib/i18n-context'

import { formatClock } from '../utils'
import { LineChart, type LineChartSeries } from './line-chart'

const MESSAGES: Record<string, string> = {
  'admin.performance.detail.table_toggle':
    admin.performance.detail.table_toggle,
  'admin.performance.detail.table_time': admin.performance.detail.table_time,
}

const TITLE = 'TTFT trend'
const UNIT = 'ms'
const T0 = Date.UTC(2026, 9, 28, 10, 0, 0)
const HOUR = 60 * 60 * 1000

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
    <I18nContextProvider
      value={{
        locale: 'en',
        t: (key) => MESSAGES[key] ?? key,
      }}
    >
      <LineChart series={series} title={TITLE} unit={UNIT} />
    </I18nContextProvider>,
  )
}

function resolveSvg(): SVGSVGElement {
  const svg = screen.getByRole('img', { name: TITLE })
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

const ORIGINAL_MATCH_MEDIA = window.matchMedia

afterEach(() => {
  window.matchMedia = ORIGINAL_MATCH_MEDIA
})

describe('LineChart', () => {
  it('exposes an accessible name and one legend entry per series', () => {
    const { container } = renderChart()
    const svg = screen.getByRole('img', { name: TITLE })

    expect(svg.querySelector('title')?.textContent).toBe(TITLE)
    const legendItems = container.querySelectorAll('ul li')
    expect(legendItems).toHaveLength(2)
    expect(legendItems[0]?.textContent).toBe('TTFT p50')
    expect(legendItems[1]?.textContent).toBe('TTFT p95')
    // 色块与文字同时出现，顺序与 series 一致。
    expect(legendItems[0]?.querySelector('span')?.getAttribute('style')).toContain(
      '--chart-cat-1',
    )
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

    // viewBox 是 720 宽，最后一点在 x = 720 - 12。jsdom 没有 PointerEvent
    // 构造器，这里用 MouseEvent 派发同名事件，clientX 才会进入 React 的
    // onPointerMove 处理器。
    fireEvent(
      svg,
      new MouseEvent('pointermove', { bubbles: true, clientX: 708 }),
    )

    const tip = screen.getByTestId('chart-tip')
    expect(tip.textContent).toContain(formatClock(T0 + 2 * HOUR, 'en'))
    expect(tip.textContent).toContain('TTFT p50: 140 ms')
    expect(tip.textContent).toContain('TTFT p95: 400 ms')
    expect(
      svg.querySelector('line[data-crosshair="true"]'),
    ).not.toBeNull()
  })

  it('moves the selected point with the left/right arrow keys', () => {
    renderChart()
    const svg = resolveSvg()

    svg.focus()
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(screen.getByTestId('chart-tip').textContent).toContain(
      'TTFT p50: 100 ms',
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

    const details = container.querySelector('details')
    expect(details).not.toBeNull()
    expect(details?.open).toBe(false)
    expect(details?.querySelector('summary')?.textContent).toBe(
      'View data table',
    )

    expect(
      screen.getByRole('columnheader', { name: 'Window' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('columnheader', { name: 'TTFT p50 (ms)' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('columnheader', { name: 'TTFT p95 (ms)' }),
    ).toBeTruthy()
    expect(container.querySelectorAll('table tbody tr')).toHaveLength(
      SERIES[0]?.points.length ?? 0,
    )
  })

  it('adds no animation when prefers-reduced-motion is set', () => {
    window.matchMedia = ((query: string) =>
      ({
        matches: true,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia

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
