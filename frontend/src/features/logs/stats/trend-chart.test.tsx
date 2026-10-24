import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '../test-utils'
import { TrendChart } from './trend-chart'

const ROWS = [
  { bucket: '2026-10-24T01:00:00Z', cost: 100, requests: 2 },
  { bucket: '2026-10-24T02:00:00Z', cost: 0, requests: 0 },
  { bucket: '2026-10-24T03:00:00Z', cost: 300, requests: 6 },
]

function hours(count: number) {
  return Array.from({ length: count }, (_, index) => ({
    bucket: new Date(Date.UTC(2026, 9, 24, index)).toISOString(),
    cost: index * 10,
    requests: index,
  }))
}

describe('trend chart', () => {
  it('renders one cost and one request bar per bucket, zeros included', () => {
    renderWithProviders(<TrendChart rows={ROWS} granularity="hour" />)
    const costs = screen.getAllByTestId('trend-bar-cost')
    expect(costs).toHaveLength(3)
    expect(costs[1]!.dataset.zero).toBe('true')
    expect(costs[0]!.dataset.zero).toBe('false')
    expect(screen.getAllByTestId('trend-bar-requests')).toHaveLength(3)
  })

  it('exposes every bucket through the screen-reader table', () => {
    renderWithProviders(<TrendChart rows={ROWS} granularity="hour" />)
    const table = screen.getByTestId('trend-chart-table')
    expect(within(table).getAllByRole('row')).toHaveLength(4) // header + 3
  })

  it('labels the requests pane at max, mid and zero like the spend pane', () => {
    renderWithProviders(
      <TrendChart
        rows={[
          { bucket: '2026-10-24T01:00:00Z', cost: 10, requests: 6800 },
          { bucket: '2026-10-24T02:00:00Z', cost: 20, requests: 3400 },
        ]}
        granularity="hour"
      />,
    )
    const labels = screen
      .getAllByTestId('trend-y-request-label')
      .map((label) => label.textContent)
    expect(labels).toEqual(['6,800', '3,400', '0'])
    expect(labels).toHaveLength(3)
  })

  it('draws the average line only when the window has spend', () => {
    const { unmount } = renderWithProviders(<TrendChart rows={ROWS} granularity="hour" />)
    expect(screen.getByTestId('trend-avg')).toBeTruthy()
    unmount()
    renderWithProviders(
      <TrendChart rows={[{ bucket: '2026-10-24T01:00:00Z', cost: 0, requests: 3 }]} granularity="hour" />,
    )
    expect(screen.queryByTestId('trend-avg')).toBeNull()
  })

  it('fills the plot for a single-bucket window', () => {
    renderWithProviders(
      <TrendChart rows={[{ bucket: '2026-10-24T01:00:00Z', cost: 42, requests: 1 }]} granularity="hour" />,
    )
    const bar = screen.getByTestId('trend-bar-cost')
    expect(bar.style.height).toBe('100%')
    expect(screen.getAllByTestId('trend-x-tick')).toHaveLength(1)
  })

  it('caps the x-axis at seven ticks for a 24-bucket window', () => {
    renderWithProviders(<TrendChart rows={hours(24)} granularity="hour" />)
    const ticks = screen.getAllByTestId('trend-x-tick')
    expect(ticks.length).toBeGreaterThan(1)
    expect(ticks.length).toBeLessThanOrEqual(7)
  })

  it('handles an all-zero window and a single bucket without dividing by zero', () => {
    const allZero = hours(24).map((row) => ({ ...row, cost: 0, requests: 0 }))
    const { unmount } = renderWithProviders(<TrendChart rows={allZero} granularity="hour" />)

    expect(screen.queryByTestId('trend-avg')).toBeNull()
    const bars = screen.getAllByTestId('trend-bar-cost')
    expect(bars).toHaveLength(24)
    for (const bar of bars) {
      expect(bar.dataset.zero).toBe('true')
      expect(bar.style.height).toBe('2px')
    }

    unmount()
    renderWithProviders(
      <TrendChart rows={[{ bucket: '2026-10-24T01:00:00Z', cost: 42, requests: 1 }]} granularity="hour" />,
    )
    expect(screen.getByTestId('trend-bar-cost').style.height).toBe('100%')
  })

  it('shows the bucket values on hover', async () => {
    const user = userEvent.setup()
    renderWithProviders(<TrendChart rows={ROWS} granularity="hour" />)
    await user.hover(screen.getAllByTestId('trend-bar-cost')[2]!)
    const tip = screen.getByTestId('trend-tip')
    expect(tip.textContent).toContain('300')
    expect(tip.textContent).toContain('6')
  })
})
