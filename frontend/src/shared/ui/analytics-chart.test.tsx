import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

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
