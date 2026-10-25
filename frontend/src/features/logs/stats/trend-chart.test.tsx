import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '../test-utils'
import { TrendCard, type TrendMetric } from './trend-chart'

const ROWS = [
  { bucket: '2026-10-24T01:00:00Z', cost: 100, requests: 2 },
  { bucket: '2026-10-24T02:00:00Z', cost: 0, requests: 0 },
  { bucket: '2026-10-24T03:00:00Z', cost: 300, requests: 6 },
]

const EMPTY = 'No statistics yet'

function hours(count: number) {
  return Array.from({ length: count }, (_, index) => ({
    bucket: new Date(Date.UTC(2026, 9, 24, index)).toISOString(),
    cost: index * 10,
    requests: index,
  }))
}

function card(metric: TrendMetric): HTMLElement {
  return screen.getByTestId(`trend-card-${metric}`)
}

function readout(metric: TrendMetric): HTMLElement {
  return screen.getByTestId(`trend-readout-${metric}`)
}

describe('trend card', () => {
  it('gives each measure its own card, axis and bars', () => {
    const { unmount } = renderWithProviders(
      <TrendCard rows={ROWS} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    // One card holds one measure: the spend card never plots requests.
    expect(screen.queryByTestId('trend-card-requests')).toBeNull()
    expect(screen.queryAllByTestId('trend-bar-requests')).toHaveLength(0)
    expect(screen.getAllByTestId('trend-bar-cost')).toHaveLength(3)
    unmount()

    renderWithProviders(
      <TrendCard
        rows={ROWS}
        granularity="hour"
        metric="requests"
        emptyLabel={EMPTY}
      />,
    )
    expect(screen.queryAllByTestId('trend-bar-cost')).toHaveLength(0)
    expect(screen.getAllByTestId('trend-bar-requests')).toHaveLength(3)
  })

  it('keeps a zero bucket in its slot as a stub', () => {
    renderWithProviders(
      <TrendCard rows={ROWS} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    const bars = screen.getAllByTestId('trend-bar-cost')
    expect(bars[1]!.dataset.zero).toBe('true')
    expect(bars[1]!.style.height).toBe('2px')
    expect(bars[0]!.dataset.zero).toBe('false')
  })

  it('exposes every bucket through the per-card data table', () => {
    const { unmount } = renderWithProviders(
      <TrendCard rows={ROWS} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    expect(
      within(screen.getByTestId('trend-table-cost')).getAllByRole('row'),
    ).toHaveLength(4) // header + 3
    unmount()

    renderWithProviders(
      <TrendCard
        rows={ROWS}
        granularity="hour"
        metric="requests"
        emptyLabel={EMPTY}
      />,
    )
    expect(
      within(screen.getByTestId('trend-table-requests')).getAllByRole('row'),
    ).toHaveLength(4)
  })

  it('labels the requests axis with request counts, not credits', () => {
    renderWithProviders(
      <TrendCard
        rows={[
          { bucket: '2026-10-24T01:00:00Z', cost: 10, requests: 6800 },
          { bucket: '2026-10-24T02:00:00Z', cost: 20, requests: 3400 },
        ]}
        granularity="hour"
        metric="requests"
        emptyLabel={EMPTY}
      />,
    )
    const axis = within(card('requests')).getByTestId('trend-axis-requests')
    expect([...axis.children].map((label) => label.textContent)).toEqual([
      '6,800',
      '3,400',
      '0',
    ])
    expect(within(card('requests')).queryByText(/credit/)).toBeNull()
  })

  it('draws the average line against each card’s own measure', () => {
    const { unmount } = renderWithProviders(
      <TrendCard rows={ROWS} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    expect(screen.getByTestId('trend-avg-cost')).toBeTruthy()
    // 400 credits over 3 buckets averages 133.33, rounded for display only.
    expect(within(card('cost')).getByText('Avg 133')).toBeTruthy()
    unmount()

    // Requests without spend: the spend card has nothing to average, the
    // requests card still does.
    const noSpend = ROWS.map((row) => ({ ...row, cost: 0 }))
    const view = renderWithProviders(
      <>
        <TrendCard
          rows={noSpend}
          granularity="hour"
          metric="cost"
          emptyLabel={EMPTY}
        />
        <TrendCard
          rows={noSpend}
          granularity="hour"
          metric="requests"
          emptyLabel={EMPTY}
        />
      </>,
    )
    expect(screen.queryByTestId('trend-avg-cost')).toBeNull()
    expect(screen.getByTestId('trend-avg-requests')).toBeTruthy()
    view.unmount()
  })

  it('fills the plot for a single-bucket window', () => {
    renderWithProviders(
      <TrendCard
        rows={[{ bucket: '2026-10-24T01:00:00Z', cost: 42, requests: 1 }]}
        granularity="hour"
        metric="cost"
        emptyLabel={EMPTY}
      />,
    )
    expect(screen.getByTestId('trend-bar-cost').style.height).toBe('100%')
    expect(screen.getAllByTestId('trend-x-tick')).toHaveLength(1)
  })

  it('caps the x-axis at seven ticks for a 24-bucket window', () => {
    renderWithProviders(
      <TrendCard
        rows={hours(24)}
        granularity="hour"
        metric="cost"
        emptyLabel={EMPTY}
      />,
    )
    const ticks = screen.getAllByTestId('trend-x-tick')
    expect(ticks.length).toBeGreaterThan(1)
    expect(ticks.length).toBeLessThanOrEqual(7)
  })

  it('handles an all-zero window without dividing by zero', () => {
    const allZero = hours(24).map((row) => ({ ...row, cost: 0, requests: 0 }))
    const { unmount } = renderWithProviders(
      <TrendCard
        rows={allZero}
        granularity="hour"
        metric="cost"
        emptyLabel={EMPTY}
      />,
    )

    expect(screen.queryByTestId('trend-avg-cost')).toBeNull()
    const bars = screen.getAllByTestId('trend-bar-cost')
    expect(bars).toHaveLength(24)
    for (const bar of bars) {
      expect(bar.dataset.zero).toBe('true')
      expect(bar.style.height).toBe('2px')
    }
    // A refused-only window has no spend, so no "Peak … 0 credits" line.
    expect(within(card('cost')).queryByText(/Peak/)).toBeNull()
    expect(screen.queryByText(/NaN/)).toBeNull()

    unmount()
    renderWithProviders(
      <TrendCard
        rows={[{ bucket: '2026-10-24T01:00:00Z', cost: 42, requests: 1 }]}
        granularity="hour"
        metric="cost"
        emptyLabel={EMPTY}
      />,
    )
    expect(screen.getByTestId('trend-bar-cost').style.height).toBe('100%')
  })

  it('names the bucket unit in the zero note', () => {
    renderWithProviders(
      <TrendCard
        rows={[
          { bucket: '2026-10-23', cost: 0, requests: 0 },
          { bucket: '2026-10-24', cost: 0, requests: 0 },
          { bucket: '2026-10-25', cost: 10, requests: 2 },
        ]}
        granularity="day"
        metric="cost"
        emptyLabel={EMPTY}
      />,
    )
    expect(screen.getByText('2 days are zero; positions kept')).toBeTruthy()
    expect(screen.queryByText(/buckets are zero/)).toBeNull()
  })

  it('prints the hovered bucket in the subtitle, not in a floating tip', async () => {
    const user = userEvent.setup()
    renderWithProviders(
      <TrendCard rows={ROWS} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    expect(readout('cost').textContent).toContain('Hover')

    await user.hover(screen.getAllByTestId('trend-bar-cost')[2]!)
    expect(readout('cost').textContent).toContain('300')
    expect(readout('cost').textContent).toContain('credits')
    expect(readout('cost').textContent).toContain('75.0')

    await user.unhover(screen.getAllByTestId('trend-bar-cost')[2]!)
    expect(readout('cost').textContent).toContain('Hover')
  })

  it('steps through buckets with the keyboard', async () => {
    const user = userEvent.setup()
    renderWithProviders(
      <TrendCard
        rows={ROWS}
        granularity="hour"
        metric="requests"
        emptyLabel={EMPTY}
      />,
    )
    const plot = screen.getByTestId('trend-plot-requests')
    plot.focus()

    await user.keyboard('{ArrowRight}')
    expect(readout('requests').textContent).toContain('2 requests')
    await user.keyboard('{ArrowRight}')
    expect(readout('requests').textContent).toContain('0 requests')
    await user.keyboard('{ArrowLeft}')
    expect(readout('requests').textContent).toContain('2 requests')
    await user.keyboard('{End}')
    expect(readout('requests').textContent).toContain('6 requests')
    await user.keyboard('{Home}')
    expect(readout('requests').textContent).toContain('2 requests')
    await user.keyboard('{Escape}')
    expect(readout('requests').textContent).toContain('Hover')
  })

  it('names the rpm peak on the requests card only', () => {
    const { unmount } = renderWithProviders(
      <TrendCard
        rows={ROWS}
        granularity="hour"
        metric="requests"
        emptyLabel={EMPTY}
        rpmPeak={48}
      />,
    )
    expect(readout('requests').textContent).not.toContain('RPM')
    expect(within(card('requests')).getByText(/48 RPM/)).toBeTruthy()
    unmount()

    renderWithProviders(
      <TrendCard rows={ROWS} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    expect(screen.queryByText(/RPM/)).toBeNull()
  })

  it('renders the empty state and the loading skeleton', () => {
    const { unmount } = renderWithProviders(
      <TrendCard
        rows={[]}
        granularity="hour"
        metric="cost"
        emptyLabel={EMPTY}
        loading
      />,
    )
    expect(screen.getByTestId('trend-loading-cost')).toBeTruthy()
    unmount()

    renderWithProviders(
      <TrendCard rows={[]} granularity="hour" metric="cost" emptyLabel={EMPTY} />,
    )
    expect(screen.getByText(EMPTY)).toBeTruthy()
    expect(screen.queryByTestId('trend-plot-cost')).toBeNull()
  })
})
