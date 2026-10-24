import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import type { ChartConfig, ChartSeries } from '@/shared/lib/chart-data'

import { AnalyticsChart, type AnalyticsChartProps } from './analytics-chart'

/**
 * R-01: M6 ships no chart library. `AnalyticsChart` renders a contract-driven
 * placeholder SVG from the exact props a future library adapter will receive,
 * so replacing the renderer must never change this surface. GC-4 requires the
 * chart to expose `role="img"` with a summary label and an equivalent data
 * table; both are asserted below alongside the three contract kinds.
 */

const series: ChartSeries[] = [
  {
    id: 'credits',
    label: 'Credits',
    points: [
      { ts: '2026-01-31T23:00:00Z', value: 10 },
      { ts: '2026-02-01T00:00:00Z', value: 30 },
    ],
  },
]

const config: ChartConfig = { kind: 'line', unit: 'credits' }

/**
 * jsdom 不做布局，`getBoundingClientRect()` 默认全为 0；给 SVG 固定一个
 * 320×180 的盒子（与 viewBox 一致），指针坐标才能换算回绘图坐标。
 */
function mockChartBox(svg: SVGSVGElement): void {
  vi.spyOn(svg, 'getBoundingClientRect').mockReturnValue({
    x: 0,
    y: 0,
    left: 0,
    top: 0,
    right: 320,
    bottom: 180,
    width: 320,
    height: 180,
    toJSON: () => ({}),
  })
}

describe('AnalyticsChart', () => {
  it('renders an empty state for an empty series list without crashing', async () => {
    await renderWithProviders(<AnalyticsChart series={[]} config={config} />)

    expect(screen.getByTestId('analytics-chart-empty').textContent).toBe(
      'No statistics yet',
    )
    expect(screen.queryByRole('img')).toBeNull()
  })

  it('renders an empty state when every series has no points', async () => {
    await renderWithProviders(
      <AnalyticsChart
        series={[{ id: 'credits', label: 'Credits', points: [] }]}
        config={config}
      />,
    )

    expect(screen.getByTestId('analytics-chart-empty')).toBeTruthy()
  })

  it.each([
    ['line', 'polyline'],
    ['bar', 'rect[data-bar]'],
    ['area', 'polygon'],
  ] as const)('renders the %s kind as its placeholder shape', async (kind, selector) => {
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={{ ...config, kind }} />,
    )

    const chart = screen.getByTestId('analytics-chart')
    expect(chart.getAttribute('data-kind')).toBe(kind)
    expect(container.querySelector(`svg ${selector}`)).not.toBeNull()
  })

  it.each(['line', 'area'] as const)(
    'renders a visible marker for a single-bucket %s chart',
    async (kind) => {
      const { container } = await renderWithProviders(
        <AnalyticsChart
          series={[
            {
              id: 'credits',
              label: 'Credits',
              points: [{ ts: '2026-01-01T00:00:00Z', value: 5 }],
            },
          ]}
          config={{ ...config, kind }}
        />,
      )

      expect(container.querySelector('svg circle[data-marker]')).not.toBeNull()
      if (kind === 'area') {
        expect(
          container.querySelector('svg rect[data-area-min-height]'),
        ).not.toBeNull()
      }
    },
  )

  it('exposes an accessible summary label for the rendered chart', async () => {
    await renderWithProviders(<AnalyticsChart series={series} config={config} />)

    const image = screen.getByRole('img')
    const label = image.getAttribute('aria-label') ?? ''
    expect(label).toContain('Credits')
    expect(label).toContain('credits')
  })

  it('provides an equivalent data table', async () => {
    await renderWithProviders(<AnalyticsChart series={series} config={config} />)

    const table = screen.getByRole('table')
    expect(table.textContent).toContain('Credits')
    expect(table.textContent).toContain('30')
  })

  it('draws dashed reference lines and appends their labels to the summary', async () => {
    const { container } = await renderWithProviders(
      <AnalyticsChart
        series={series}
        config={config}
        referenceLines={[
          { value: 50, label: 'Tolerance +50 bp' },
          { value: -50, label: 'Tolerance -50 bp' },
        ]}
      />,
    )

    // 参考线是水平虚线，共享同一根 Y 轴：竖向轴线仍然只有一条。
    const reference = container.querySelectorAll('line[data-reference-line="true"]')
    expect(reference).toHaveLength(2)
    reference.forEach((line) => {
      expect(line.getAttribute('stroke-dasharray')).not.toBeNull()
    })
    const verticals = [...container.querySelectorAll('svg line')].filter(
      (line) => line.getAttribute('x1') === line.getAttribute('x2'),
    )
    expect(verticals).toHaveLength(1)

    const label = screen.getByRole('img').getAttribute('aria-label') ?? ''
    expect(label).toContain('Tolerance +50 bp')
    expect(label).toContain('Tolerance -50 bp')
  })

  it('renders no reference lines when the prop is absent', async () => {
    // 回归护栏：不传 referenceLines 时必须与旧行为完全一致。
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={config} />,
    )

    expect(container.querySelectorAll('line[data-reference-line="true"]')).toHaveLength(
      0,
    )
    expect(screen.getByRole('img').getAttribute('aria-label')).not.toContain(
      'Tolerance',
    )
  })

  it('names a shared reference label only once in the aria summary', async () => {
    await renderWithProviders(
      <AnalyticsChart
        series={series}
        config={config}
        // 两条带共用「±」标签：摘要里不能重复播报同一个名字。
        referenceLines={[
          { value: 50, label: 'Tolerance ±50 bp' },
          { value: -50, label: 'Tolerance ±50 bp' },
        ]}
      />,
    )

    expect(screen.getByRole('img').getAttribute('aria-label')).toBe(
      'line chart with 1 series: Credits. Unit: credits. Tolerance ±50 bp',
    )
  })

  it('places the +tolerance line strictly above the -tolerance line inside the plot', async () => {
    const { container } = await renderWithProviders(
      <AnalyticsChart
        series={series}
        config={{ ...config, domain: [-100, 100] }}
        referenceLines={[
          { value: 50, label: 'Tolerance +50 bp' },
          { value: -50, label: 'Tolerance -50 bp' },
        ]}
      />,
    )

    const svg = screen.getByRole('img')
    // 竖直的 Y 轴给出绘图区的上下边界（PLOT_TOP / BASELINE_Y）。
    const axis = [...svg.querySelectorAll('line')].find(
      (line) => line.getAttribute('x1') === line.getAttribute('x2'),
    )
    expect(axis).not.toBeUndefined()
    const top = Number(axis?.getAttribute('y1'))
    const bottom = Number(axis?.getAttribute('y2'))

    const lines = container.querySelectorAll('line[data-reference-line="true"]')
    expect(lines).toHaveLength(2)
    // 绘制顺序即 +tolerance、−tolerance：纵坐标更小 = 画得更靠上。
    const plusY = Number(lines[0]?.getAttribute('y1'))
    const minusY = Number(lines[1]?.getAttribute('y1'))
    expect(plusY).toBeLessThan(minusY)
    for (const y of [plusY, minusY]) {
      expect(y).toBeGreaterThanOrEqual(top)
      expect(y).toBeLessThanOrEqual(bottom)
    }
  })

  it('shows the nearest point on hover and clears it on leave', async () => {
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={config} tooltip />,
    )
    const svg = container.querySelector('svg')
    expect(svg).not.toBeNull()
    if (svg === null) throw new Error('chart svg missing')
    mockChartBox(svg)

    // 两个点分别在 viewBox 的 x=44 与 x=312；pointer 在右端选后者（值 30）。
    fireEvent.mouseMove(svg, { clientX: 312 })
    expect(screen.getByTestId('analytics-chart-tooltip').textContent).toBe(
      '30 credits',
    )
    expect(container.querySelector('line[data-crosshair="true"]')).not.toBeNull()
    expect(
      container.querySelector('circle[data-hover-marker="true"]'),
    ).not.toBeNull()

    // 靠左端的指针选第一个点（值 10）：取「最近」而不是固定索引。
    fireEvent.mouseMove(svg, { clientX: 50 })
    expect(screen.getByTestId('analytics-chart-tooltip').textContent).toBe(
      '10 credits',
    )

    fireEvent.mouseLeave(svg)
    expect(screen.queryByTestId('analytics-chart-tooltip')).toBeNull()
    expect(container.querySelector('line[data-crosshair="true"]')).toBeNull()
  })

  it('moves the selection with the keyboard and announces it politely', async () => {
    await renderWithProviders(
      <AnalyticsChart series={series} config={config} tooltip />,
    )
    const svg = screen.getByRole('img')
    const live = screen.getByTestId('analytics-chart-live')

    expect(svg.getAttribute('tabindex')).toBe('0')
    expect(live.getAttribute('aria-live')).toBe('polite')

    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(live.textContent).toContain('10 credits')
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(live.textContent).toContain('30 credits')

    // 已到最后一个点：再按右方向键被 clamp 在末端。
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(live.textContent).toContain('30 credits')

    // 左方向键回退，并在第一个点上被 clamp。
    fireEvent.keyDown(svg, { key: 'ArrowLeft' })
    expect(live.textContent).toContain('10 credits')
    fireEvent.keyDown(svg, { key: 'ArrowLeft' })
    expect(live.textContent).toContain('10 credits')

    fireEvent.keyDown(svg, { key: 'Home' })
    expect(live.textContent).toContain('10 credits')
    fireEvent.keyDown(svg, { key: 'End' })
    expect(live.textContent).toContain('30 credits')

    fireEvent.keyDown(svg, { key: 'Escape' })
    expect(screen.queryByTestId('analytics-chart-tooltip')).toBeNull()
    expect(live.textContent).toBe('')

    // 尚未选中时按左方向键从右端进入（与右方向键从左端进入对称）。
    fireEvent.keyDown(svg, { key: 'ArrowLeft' })
    expect(live.textContent).toContain('30 credits')

    // 重新选中后失焦也必须清除。
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(screen.getByTestId('analytics-chart-tooltip')).not.toBeNull()
    fireEvent.blur(svg)
    expect(screen.queryByTestId('analytics-chart-tooltip')).toBeNull()
  })

  it('opens on pointerdown and clears on pointerleave', async () => {
    // jsdom 没有完整的 PointerEvent：用带 clientX 的 MouseEvent 走同一条
    // React 事件通道，验证 pointerdown / pointerleave 这两个处理器本身，
    // 而不是只验证鼠标的 mousemove / mouseleave。
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={config} tooltip />,
    )
    const svg = container.querySelector('svg')
    if (svg === null) throw new Error('chart svg missing')
    mockChartBox(svg)

    fireEvent(svg, new MouseEvent('pointerdown', { clientX: 312, bubbles: true }))
    expect(screen.getByTestId('analytics-chart-tooltip').textContent).toBe(
      '30 credits',
    )

    // React 把 onPointerLeave 合成在原生 `pointerout` 上（relatedTarget 在
    // 元素外时才算 leave），所以这里发 pointerout 而不是 pointerleave。
    fireEvent(
      svg,
      new MouseEvent('pointerout', {
        bubbles: true,
        relatedTarget: document.body,
      }),
    )
    expect(screen.queryByTestId('analytics-chart-tooltip')).toBeNull()

    // pointermove 单独接一条：触摸拖动时没有 mousemove 兼容事件可依赖。
    fireEvent(svg, new MouseEvent('pointermove', { clientX: 50, bubbles: true }))
    expect(screen.getByTestId('analytics-chart-tooltip').textContent).toBe(
      '10 credits',
    )
  })

  it('ignores hover before the SVG has a measurable box', async () => {
    // jsdom 的 getBoundingClientRect 宽度为 0；此时没有可用的坐标映射，
    // 宁可什么都不选中，也不能把每个点都算成同一个索引。
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={config} tooltip />,
    )
    const svg = container.querySelector('svg')
    if (svg === null) throw new Error('chart svg missing')

    fireEvent.mouseMove(svg, { clientX: 312 })
    expect(screen.queryByTestId('analytics-chart-tooltip')).toBeNull()
  })

  it('adds no interactive affordance when the tooltip prop is absent', async () => {
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={config} />,
    )
    const svg = container.querySelector('svg')
    expect(svg).not.toBeNull()
    if (svg === null) throw new Error('chart svg missing')

    // 关闭时 DOM 与旧版一致：没有 tabindex、没有 live region、没有浮层。
    expect(svg.hasAttribute('tabindex')).toBe(false)
    expect(container.querySelector('[aria-live]')).toBeNull()
    expect(container.querySelector('[data-testid="analytics-chart-tooltip"]')).toBeNull()
    expect(container.querySelector('[data-crosshair]')).toBeNull()

    mockChartBox(svg)
    fireEvent.mouseMove(svg, { clientX: 312 })
    fireEvent.keyDown(svg, { key: 'ArrowRight' })
    expect(container.querySelector('[data-testid="analytics-chart-tooltip"]')).toBeNull()
    expect(container.querySelector('[data-crosshair]')).toBeNull()
  })

  it('keeps the public props contract independent of the renderer', async () => {
    // A future chart library must slot in behind exactly these two props.
    const props: AnalyticsChartProps = { series, config }
    const publicProps: { series: ChartSeries[]; config: ChartConfig } = props

    const { container } = await renderWithProviders(<AnalyticsChart {...publicProps} />)
    expect(container.querySelector('svg')).not.toBeNull()
  })

  it('renders no animations so reduced-motion users see the final values', async () => {
    const { container } = await renderWithProviders(
      <AnalyticsChart series={series} config={config} />,
    )

    expect(container.querySelector('animate')).toBeNull()
    expect(screen.getByTestId('analytics-chart').className).not.toContain('animate')
  })
})
