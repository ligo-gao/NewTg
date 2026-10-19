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
