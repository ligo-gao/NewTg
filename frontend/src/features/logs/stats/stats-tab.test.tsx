import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { DEFAULT_USAGE_LOG_SEARCH, type UsageLogSearch } from '../search'
import {
  type FetchRecord,
  installFetch,
  jsonResponse,
  latestRequest,
  problemResponse,
  renderWithProviders,
} from '../test-utils'
import { UsageStatsTab } from './stats-tab'

const SUMMARY = {
  cost_total: 12480,
  request_count: 1024,
  success_rate: 0.982,
  prompt_tokens_total: 1_200_000,
  completion_tokens_total: 356_000,
  pre_deducted_total: 14200,
  refunded_total: 1700,
}

const BY_MODEL = [
  { model: 'gpt-4o', cost: 6240, tokens: 400_000, percent: 50 },
  { model: 'claude-4', cost: 3744, tokens: 200_000, percent: 30 },
  { model: 'other', cost: 2496, tokens: 100_000, percent: 20 },
]

const RATES = {
  rpm_avg: 12.5,
  rpm_peak: 48,
  tpm_avg: 980.4,
  cost_total: 12480,
  pre_deducted_total: 14200,
  refunded_total: 1700,
}

function hourBucket(offsetHours: number): string {
  const now = Date.now()
  const hour = Math.floor(now / 3_600_000) * 3_600_000
  return new Date(hour + offsetHours * 3_600_000).toISOString()
}

function renderTab(overrides: Partial<UsageLogSearch> = {}) {
  return renderWithProviders(
    <UsageStatsTab
      search={{ ...DEFAULT_USAGE_LOG_SEARCH, ...overrides }}
      canFilterByUsername={false}
    />,
  )
}

function installStats(
  overrides: {
    summary?: (request: FetchRecord) => Response
    byModel?: (request: FetchRecord) => Response
    timeseries?: (request: FetchRecord) => Response
    rates?: (request: FetchRecord) => Response
  } = {},
): FetchRecord[] {
  return installFetch((request) => {
    const path = request.url.pathname
    if (path === '/api/usage-stats/summary') {
      return overrides.summary?.(request) ?? jsonResponse(SUMMARY)
    }
    if (path === '/api/usage-stats/by-model') {
      return overrides.byModel?.(request) ?? jsonResponse(BY_MODEL)
    }
    if (path === '/api/usage-stats/timeseries') {
      return (
        overrides.timeseries?.(request) ??
        jsonResponse([
          { bucket: hourBucket(-2), cost: 100, requests: 2 },
          { bucket: hourBucket(0), cost: 300, requests: 6 },
        ])
      )
    }
    if (path === '/api/usage-stats/rates') {
      return overrides.rates?.(request) ?? jsonResponse(RATES)
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

describe('usage stats tab (F-05-03)', () => {
  it('renders the four overview cards from the summary endpoint', async () => {
    installStats()
    renderTab({ range: '7d' })

    expect(await screen.findByText('Total spent')).toBeTruthy()
    expect(screen.getByText('12,480 credits')).toBeTruthy()
    expect(screen.getByText('1,024')).toBeTruthy()
    expect(screen.getByText('Success rate 98.2%')).toBeTruthy()
    expect(screen.getByText('1.2M')).toBeTruthy()
    expect(screen.getByText('356,000')).toBeTruthy()
  })

  it('sends the shared filters to every stats endpoint', async () => {
    const requests = installStats()
    renderTab({ range: '14d', model: 'gpt-4o', group: 'default', token_id: 12 })

    await screen.findByText('Total spent')
    await waitFor(() => {
      const request = latestRequest(requests, '/api/usage-stats/summary')
      expect(request?.url.searchParams.get('range')).toBe('14d')
      expect(request?.url.searchParams.get('model')).toBe('gpt-4o')
      expect(request?.url.searchParams.get('group')).toBe('default')
      expect(request?.url.searchParams.get('token_id')).toBe('12')
      // request_id is a row filter; the aggregates must never receive it.
      expect(request?.url.searchParams.has('request_id')).toBe(false)
    })
    expect(
      latestRequest(requests, '/api/usage-stats/timeseries')?.url.searchParams.get(
        'granularity',
      ),
    ).toBe('day')
  })

  it('renders the model distribution as an accessible table with Other', async () => {
    installStats()
    renderTab()

    const table = await screen.findByTestId('stats-model-table')
    expect(within(table).getByText('gpt-4o')).toBeTruthy()
    // The server aggregates the ninth-and-later rows into `other`; the UI
    // shows the localised label, never the raw sentinel.
    expect(within(table).getByText('Other')).toBeTruthy()
    expect(within(table).getByText('50.0%')).toBeTruthy()
  })

  it('renders the trend chart with a table alternative and zero-filled buckets', async () => {
    const user = userEvent.setup()
    installStats()
    // Four-hour custom window: hourly buckets, two of them carry data.
    renderTab({
      range: 'custom',
      start: hourBucket(-3),
      end: hourBucket(1),
    })

    const chart = await screen.findByTestId('analytics-chart')
    expect(chart.getAttribute('data-kind')).toBe('area')

    await user.click(screen.getByText('View data table'))
    const table = await screen.findByTestId('stats-trend-table')
    // Header + four buckets; the middle hour has no server row and is
    // materialised as zero instead of connecting the line across the gap.
    expect(within(table).getAllByRole('row')).toHaveLength(5)
    // The filled hour carries both a zero cost and zero requests.
    expect(within(table).getAllByText('0').length).toBeGreaterThanOrEqual(2)
  })

  it('hides the rate strip when the window has no requests', async () => {
    const requests = installStats({
      summary: () => jsonResponse({ ...SUMMARY, request_count: 0 }),
    })
    renderTab()

    await screen.findByText('Total spent')
    await waitFor(() => {
      expect(screen.queryByTestId('stats-rates')).toBeNull()
    })
    expect(latestRequest(requests, '/api/usage-stats/rates')).toBeUndefined()
  })

  it('shows the rate strip once there is data', async () => {
    installStats()
    renderTab()

    const rates = await screen.findByTestId('stats-rates')
    expect(within(rates).getByText('12.5')).toBeTruthy()
    expect(within(rates).getByText('48')).toBeTruthy()
  })

  it('keeps an inline retry when the summary fails', async () => {
    const user = userEvent.setup()
    let calls = 0
    installStats({
      summary: () => {
        calls += 1
        if (calls === 1) {
          return problemResponse(500, { type: 'server_error', title: 'boom' })
        }
        return jsonResponse(SUMMARY)
      },
    })
    renderTab()

    expect(await screen.findByText('Failed to load logs.')).toBeTruthy()
    // Cards degrade to a dash rather than showing invented zeros.
    expect(screen.getAllByText('—').length).toBeGreaterThan(0)
    expect(screen.getByTestId('stats-model-table')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('12,480 credits')).toBeTruthy()
  })
})
