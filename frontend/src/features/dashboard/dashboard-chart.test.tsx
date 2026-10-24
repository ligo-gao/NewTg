import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { DashboardChart } from './dashboard-chart'
import { renderWithProviders } from './test-utils'

const series = {
  id: 'cost',
  label: 'Spend',
  points: [
    { ts: '2026-10-23T01:00:00Z', value: 10 },
    { ts: '2026-10-23T02:00:00Z', value: 30 },
    { ts: '2026-10-23T03:00:00Z', value: 20 },
  ],
}

describe('DashboardChart', () => {
  it('renders a smooth monotone path, an endpoint marker and the data table', async () => {
    await renderWithProviders(
      <DashboardChart series={series} unit="credits" granularity="hour" />,
    )
    const chart = screen.getByTestId('dashboard-chart')
    const paths = chart.querySelectorAll('path')
    expect(paths.length).toBeGreaterThanOrEqual(2)
    const line = Array.from(paths).find((path) => path.getAttribute('fill') === 'none')
    expect(line?.getAttribute('d')).toContain('C')
    expect(chart.querySelector('circle[stroke="var(--surface-card)"]')).not.toBeNull()
    const table = chart.querySelector('table')
    expect(table?.querySelectorAll('tbody tr').length).toBe(3)
  })

  it('renders nothing for an empty series', async () => {
    await renderWithProviders(
      <DashboardChart
        series={{ id: 'cost', label: 'Spend', points: [] }}
        unit="credits"
        granularity="hour"
      />,
    )
    expect(screen.queryByTestId('dashboard-chart')).toBeNull()
  })
})
