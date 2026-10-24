import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { UsageLogItem } from '../search'
import { renderWithProviders } from '../test-utils'
import { DistributionCard, RecentFailures } from './breakdowns'

const ITEMS = [
  { key: 'a', label: 'gpt-4o', cost: 750, requests: 30, tokens: 400_000, percent: 75 },
  { key: 'b', label: 'other', cost: 250, requests: 10, tokens: 100_000, percent: 25 },
]

const FAILURE: UsageLogItem = {
  id: 7,
  request_id: '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
  model: 'deepseek-flash',
  token_name: 'prod-key',
  prompt_tokens: 10,
  completion_tokens: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  cost: 0,
  refunded: 0,
  stream: false,
  status: 'error',
  created_at: '2026-10-24T14:51:09Z',
  latency_ms: 5020,
  error_message: 'Upstream timed out',
}

describe('DistributionCard', () => {
  it('renders the full table with cost, share, tokens and requests', () => {
    renderWithProviders(
      <DistributionCard title="Model mix" subtitle="By spend" items={ITEMS} mode="full" emptyLabel="Nothing" />,
    )
    expect(screen.getByText('gpt-4o')).toBeTruthy()
    expect(screen.getByText('750')).toBeTruthy()
    expect(screen.getByText('75.0%')).toBeTruthy()
    expect(screen.getByText('400,000')).toBeTruthy()
    expect(screen.getByText('30')).toBeTruthy()
  })

  it('renders the compact list with cost over requests and share', () => {
    renderWithProviders(
      <DistributionCard title="Groups" subtitle="By spend" items={ITEMS} mode="compact" emptyLabel="Nothing" />,
    )
    expect(screen.getByText('250')).toBeTruthy()
    expect(screen.getByText(/10.*25\.0%/)).toBeTruthy()
  })

  it('falls back to the empty label when there are no rows', () => {
    renderWithProviders(
      <DistributionCard title="Groups" subtitle="By spend" items={[]} mode="compact" emptyLabel="Nothing here" />,
    )
    expect(screen.getByText('Nothing here')).toBeTruthy()
  })

  it('renders the optional note at the card foot', () => {
    renderWithProviders(
      <DistributionCard
        title="Tokens"
        subtitle="By spend"
        items={ITEMS}
        mode="compact"
        emptyLabel="Nothing"
        note="Quota exhausted requests are refused"
      />,
    )
    expect(screen.getByText('Quota exhausted requests are refused')).toBeTruthy()
  })
})

describe('RecentFailures', () => {
  it('opens a row and the full list', async () => {
    const user = userEvent.setup()
    const onOpen = vi.fn()
    const onViewAll = vi.fn()
    renderWithProviders(
      <RecentFailures items={[FAILURE]} total={74} onOpen={onOpen} onViewAll={onViewAll} />,
    )
    expect(screen.getByText('Upstream timed out · 5.0s')).toBeTruthy()
    await user.click(screen.getByText('deepseek-flash · prod-key'))
    expect(onOpen).toHaveBeenCalledWith(7)
    await user.click(screen.getByText('View all 74 failures'))
    expect(onViewAll).toHaveBeenCalled()
  })

  it('renders a real failure message instead of the status label', () => {
    renderWithProviders(
      <RecentFailures
        items={[{ ...FAILURE, error_message: 'upstream exploded' }]}
        total={1}
        onOpen={vi.fn()}
        onViewAll={vi.fn()}
      />,
    )
    expect(screen.getByText('upstream exploded · 5.0s')).toBeTruthy()
    expect(screen.queryByText(/^Failed ·/)).toBeNull()
  })

  it('falls back to error_class when error_message is only the mask', () => {
    renderWithProviders(
      <RecentFailures
        items={[
          {
            ...FAILURE,
            error_message: '••••••',
            error_class: 'upstream_5xx',
          },
        ]}
        total={1}
        onOpen={vi.fn()}
        onViewAll={vi.fn()}
      />,
    )
    expect(screen.getByText('upstream_5xx · 5.0s')).toBeTruthy()
    expect(screen.queryByText(/••••••/)).toBeNull()
  })

  it('falls back to the status label when both message and class are empty', () => {
    renderWithProviders(
      <RecentFailures
        items={[{ ...FAILURE, error_message: '***', error_class: null }]}
        total={1}
        onOpen={vi.fn()}
        onViewAll={vi.fn()}
      />,
    )
    expect(screen.getByText('Failed · 5.0s')).toBeTruthy()
  })

  it('prints failure times on a 24-hour clock', () => {
    renderWithProviders(
      <RecentFailures items={[FAILURE]} total={1} onOpen={vi.fn()} onViewAll={vi.fn()} />,
    )
    const time = screen.getByText(/^\d{2}:\d{2}$/)
    expect(time.textContent).toMatch(/^\d{2}:\d{2}$/)
    expect(screen.queryByText(/AM|PM/)).toBeNull()
  })

  it('shows the empty note when nothing failed', () => {
    renderWithProviders(
      <RecentFailures items={[]} total={0} onOpen={vi.fn()} onViewAll={vi.fn()} />,
    )
    expect(screen.getByText('No failed requests in this window')).toBeTruthy()
    // The card is visible for every user on a healthy window; a live link to
    // an empty list would be a dead end.
    expect(screen.queryByText(/View all/)).toBeNull()
  })
})
