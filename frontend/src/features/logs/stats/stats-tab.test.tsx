import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { DEFAULT_USAGE_LOG_SEARCH, type UsageLogSearch } from '../search'
import {
  type FetchRecord,
  installFetch,
  jsonResponse,
  latestRequest,
  problemResponse,
  renderWithProviders,
  requestsFor,
} from '../test-utils'
import { fillTimeseries } from './series'
import { UsageStatsTab, type UsageStatsTabProps } from './stats-tab'

const SUMMARY = {
  cost_total: 12480,
  request_count: 1024,
  success_rate: 0.982,
  prompt_tokens_total: 1_200_000,
  completion_tokens_total: 356_000,
  pre_deducted_total: 14200,
  refunded_total: 1700,
  p50_latency_ms: 380,
  p95_latency_ms: 2310,
  latency_samples: 1006,
  active_users: 3,
  cache_read_tokens_total: 48_000,
  success_count: 1006,
  error_count: 17,
  refunded_count: 0,
  refused_count: 1,
}

const BY_MODEL = [
  { model: 'gpt-4o', cost: 6240, requests: 640, tokens: 400_000, percent: 50 },
  { model: 'claude-4', cost: 3744, requests: 288, tokens: 200_000, percent: 30 },
  { model: 'other', cost: 2496, requests: 96, tokens: 100_000, percent: 20 },
]

const BY_TOKEN = [
  {
    token_id: 1,
    token_name: 'prod-key',
    cost: 8000,
    requests: 700,
    tokens: 1_000_000,
    percent: 64.1,
  },
  {
    token_id: 0,
    token_name: 'other',
    cost: 4480,
    requests: 324,
    tokens: 556_000,
    percent: 35.9,
  },
]

const BY_USER = [
  {
    user_id: 4,
    username: 'alice',
    cost: 12480,
    requests: 1024,
    tokens: 1_556_000,
    percent: 100,
  },
]

const BY_GROUP = [
  {
    group: 'default',
    cost: 12480,
    requests: 1024,
    tokens: 1_556_000,
    percent: 100,
  },
]

const FAILURES = {
  items: [
    {
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
    },
  ],
  total: 74,
  page: 1,
  page_size: 10,
}

const EMPTY_FAILURES = { items: [], total: 0, page: 1, page_size: 10 }

const RATES = {
  rpm_avg: 12.5,
  rpm_peak: 48,
  tpm_avg: 980.4,
  cost_total: 12480,
  pre_deducted_total: 14200,
  refunded_total: 1700,
}

const REQUEST_ID = '9f3a8d2e-1b2c-4d5e-8f90-112233445566'

function hourBucket(offsetHours: number): string {
  const now = Date.now()
  const hour = Math.floor(now / 3_600_000) * 3_600_000
  return new Date(hour + offsetHours * 3_600_000).toISOString()
}

const TIMESERIES = [
  { bucket: hourBucket(-2), cost: 100, requests: 2 },
  { bucket: hourBucket(0), cost: 300, requests: 6 },
]

type Overrides = {
  summary?: (request: FetchRecord) => Response
  byModel?: (request: FetchRecord) => Response
  timeseries?: (request: FetchRecord) => Response
  rates?: (request: FetchRecord) => Response
  byToken?: (request: FetchRecord) => Response
  byUser?: (request: FetchRecord) => Response
  byGroup?: (request: FetchRecord) => Response
  logs?: (request: FetchRecord) => Response
}

function installStats(overrides: Overrides = {}): FetchRecord[] {
  return installFetch((request) => {
    const path = request.url.pathname
    if (path === '/api/usage-stats/summary') {
      return overrides.summary?.(request) ?? jsonResponse(SUMMARY)
    }
    if (path === '/api/usage-stats/by-model') {
      return overrides.byModel?.(request) ?? jsonResponse(BY_MODEL)
    }
    if (path === '/api/usage-stats/timeseries') {
      return overrides.timeseries?.(request) ?? jsonResponse(TIMESERIES)
    }
    if (path === '/api/usage-stats/rates') {
      return overrides.rates?.(request) ?? jsonResponse(RATES)
    }
    if (path === '/api/usage-stats/by-token') {
      return (
        overrides.byToken?.(request) ??
        jsonResponse({ items: BY_TOKEN, total: SUMMARY.cost_total })
      )
    }
    if (path === '/api/usage-stats/by-user') {
      return (
        overrides.byUser?.(request) ??
        jsonResponse({ items: BY_USER, total: SUMMARY.cost_total })
      )
    }
    if (path === '/api/usage-stats/by-group') {
      return (
        overrides.byGroup?.(request) ??
        jsonResponse({ items: BY_GROUP, total: SUMMARY.cost_total })
      )
    }
    if (path === '/api/usage-logs') {
      return overrides.logs?.(request) ?? jsonResponse(EMPTY_FAILURES)
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

type TabOptions = Partial<
  Pick<
    UsageStatsTabProps,
    'isAdmin' | 'canFilterByUsername' | 'onOpenLog' | 'onViewAllFailures'
  >
>

function renderTab(
  overrides: Partial<UsageLogSearch> = {},
  options: TabOptions = {},
) {
  return renderWithProviders(
    <UsageStatsTab
      search={{ ...DEFAULT_USAGE_LOG_SEARCH, ...overrides }}
      canFilterByUsername={options.canFilterByUsername ?? false}
      isAdmin={options.isAdmin ?? false}
      active
      ready
      onOpenLog={options.onOpenLog ?? vi.fn()}
      onViewAllFailures={options.onViewAllFailures ?? vi.fn()}
    />,
  )
}

describe('usage stats tab (F-05-03)', () => {
  it('renders the KPI band from the summary', async () => {
    installStats()
    renderTab({ range: '7d' })

    expect(await screen.findByText('Total spent')).toBeTruthy()
    // The unit is its own span so the value keeps the numeric type scale.
    expect(screen.getByText('12,480')).toBeTruthy()
    expect(screen.getByText('1,024')).toBeTruthy()
    expect(
      screen.getByText('Success 1,006 · Failed 17 · Refunded 0 · Refused 1'),
    ).toBeTruthy()
    expect(screen.getByText('98.2%')).toBeTruthy()
    expect(screen.getByText('1.6M')).toBeTruthy()
    expect(screen.getByText('Input 1.2M · Output 356,000')).toBeTruthy()
    expect(screen.getByText('0.4s')).toBeTruthy()
    expect(screen.getByText('P95 2.3s · 1,006 samples')).toBeTruthy()
  })

  it('hides admin-only KPIs and cards from a normal user', async () => {
    installStats()
    renderTab()

    await screen.findByText('Total spent')
    expect(screen.queryByText('Active users')).toBeNull()
    expect(screen.queryByText('User ranking')).toBeNull()
    expect(screen.queryByText('Group usage')).toBeNull()
    expect(screen.getByText('Token usage')).toBeTruthy()
    expect(screen.getByText('Recent failures')).toBeTruthy()
  })

  it('draws the KPI separators on the cells, never as a gap background', async () => {
    installStats()
    renderTab()

    const band = await screen.findByRole('region', {
      name: 'Usage overview cards',
    })
    const bandClasses = band.className.split(/\s+/)
    // A 1px gap over a `bg-border-hairline` band turned every unfilled slot
    // (5 user cells in a 3-column row) into a solid gray card.
    expect(bandClasses).not.toContain('gap-px')
    expect(bandClasses).not.toContain('bg-border-hairline')

    const cells = [...band.children]
    expect(cells).toHaveLength(5)
    for (const cell of cells) {
      expect(cell.className.split(/\s+/)).toContain('bg-surface-card')
    }
  })

  it('uses the same separator-free band for the loading skeleton', () => {
    installStats()
    renderWithProviders(
      <UsageStatsTab
        search={DEFAULT_USAGE_LOG_SEARCH}
        canFilterByUsername={false}
        isAdmin={false}
        active
        ready={false}
        onOpenLog={vi.fn()}
        onViewAllFailures={vi.fn()}
      />,
    )

    const band = screen.getByTestId('stats-cards-loading')
    const bandClasses = band.className.split(/\s+/)
    expect(bandClasses).not.toContain('gap-px')
    expect(bandClasses).not.toContain('bg-border-hairline')
    const cells = [...band.children]
    expect(cells).toHaveLength(5)
    for (const cell of cells) {
      expect(cell.className.split(/\s+/)).toContain('bg-surface-card')
    }
  })

  it('passes each model row its own request count', async () => {
    installStats()
    renderTab()

    const modelCard = await screen.findByRole('region', {
      name: 'Model distribution',
    })
    expect(within(modelCard).getByText('640')).toBeTruthy()
  })

  it('drops the input-to-output clause when completion tokens are zero', async () => {
    installStats({
      summary: () =>
        jsonResponse({ ...SUMMARY, completion_tokens_total: 0 }),
    })
    renderTab()

    await screen.findByText('Total spent')
    // total = 1,200,000 prompt + 48,000 cache; / 1,024 requests → 1,219.
    expect(screen.getByText('1,219 tokens per request')).toBeTruthy()
    expect(screen.queryByText(/input is/)).toBeNull()
    expect(screen.queryByText(/NaN/)).toBeNull()
  })

  it('sends the shared filters to every stats endpoint', async () => {
    const requests = installStats()
    renderTab({
      range: '14d',
      model: 'gpt-4o',
      group: 'default',
      token_id: 12,
      request_id: REQUEST_ID,
    })

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
    await waitFor(() => {
      expect(
        latestRequest(
          requests,
          '/api/usage-stats/timeseries',
        )?.url.searchParams.get('granularity'),
      ).toBe('day')
    })
  })

  it('queries by-user and by-group for an admin', async () => {
    const requests = installStats()
    renderTab({}, { isAdmin: true, canFilterByUsername: true })

    await screen.findByText('Total spent')
    await waitFor(() => {
      expect(
        requestsFor(requests, '/api/usage-stats/by-user'),
      ).toHaveLength(1)
      expect(
        requestsFor(requests, '/api/usage-stats/by-group'),
      ).toHaveLength(1)
    })
    // The admin view never renders the token card, so it must not fetch it.
    expect(requestsFor(requests, '/api/usage-stats/by-token')).toHaveLength(0)
  })

  it('queries the token split and failures for a normal user', async () => {
    const requests = installStats()
    renderTab()

    await screen.findByText('Total spent')
    await waitFor(() => {
      expect(
        requestsFor(requests, '/api/usage-stats/by-token'),
      ).toHaveLength(1)
      expect(requestsFor(requests, '/api/usage-logs')).toHaveLength(1)
    })
    expect(requestsFor(requests, '/api/usage-stats/by-user')).toHaveLength(0)
    expect(requestsFor(requests, '/api/usage-stats/by-group')).toHaveLength(0)

    const failures = latestRequest(requests, '/api/usage-logs')
    expect(failures?.url.searchParams.get('status')).toBe('error')
    expect(failures?.url.searchParams.get('page_size')).toBe('10')
  })

  it('links the admin success card to failure triage', async () => {
    installStats()
    renderTab({}, { isAdmin: true, canFilterByUsername: true })

    const link = await screen.findByRole('link', {
      name: /failures → Failure triage/,
    })
    expect(link.getAttribute('href')).toBe('/admin/system/failure-triage')
  })

  it('routes the user failure rows to the drawer and the full list', async () => {
    const user = userEvent.setup()
    const onOpenLog = vi.fn()
    const onViewAllFailures = vi.fn()
    installStats({ logs: () => jsonResponse(FAILURES) })
    renderTab({}, { onOpenLog, onViewAllFailures })

    await screen.findByText('Recent failures')
    await user.click(await screen.findByText('deepseek-flash · prod-key'))
    expect(onOpenLog).toHaveBeenCalledWith(7)

    // Both the KPI foot and the failure card footer jump to the same list.
    await user.click(screen.getByText('18 failures → View details'))
    await user.click(screen.getByText('View all 74 failures'))
    expect(onViewAllFailures).toHaveBeenCalledTimes(2)
  })

  it('keeps the zero-filled trend table', async () => {
    const user = userEvent.setup()
    installStats()
    renderTab()

    await user.click(await screen.findByText('View data table'))
    const table = await screen.findByTestId('stats-trend-table')
    const expected = fillTimeseries(
      TIMESERIES,
      { ...DEFAULT_USAGE_LOG_SEARCH },
      'hour',
    ).length
    expect(within(table).getAllByRole('row')).toHaveLength(expected + 1)
  })

  it('switches the average label to the bucket unit', async () => {
    installStats()
    const first = renderTab({ range: '7d' })

    await screen.findByText('Total spent')
    expect(screen.getByText('Avg per day')).toBeTruthy()
    expect(screen.getByText(/credits \/ day$/)).toBeTruthy()
    first.unmount()

    renderTab({ range: '24h' })
    await screen.findByText('Total spent')
    expect(screen.getByText('Avg per hour')).toBeTruthy()
    expect(screen.getByText(/credits \/ hour$/)).toBeTruthy()
  })

  it('renders the quota note inside the token card only', async () => {
    installStats()
    renderTab()

    const tokenCard = await screen.findByRole('region', {
      name: 'Token usage',
    })
    expect(within(tokenCard).getByText(/quota runs out/)).toBeTruthy()

    const modelCard = screen.getByRole('region', {
      name: 'Model distribution',
    })
    expect(within(modelCard).queryByText(/quota runs out/)).toBeNull()
  })

  it('keeps an inline retry when the rates request fails', async () => {
    const user = userEvent.setup()
    const requests = installStats({
      rates: () => problemResponse(500, { type: 'server_error', title: 'boom' }),
    })
    renderTab()

    await screen.findByText('Total spent')
    const rates = await screen.findByTestId('stats-rates')
    expect(within(rates).getByText('Failed to load logs.')).toBeTruthy()

    await user.click(within(rates).getByRole('button', { name: 'Retry' }))
    await waitFor(() => {
      expect(requestsFor(requests, '/api/usage-stats/rates')).toHaveLength(2)
    })
  })

  it('never prints NaN when optional summary fields are absent', async () => {
    installStats({
      // Out-of-contract body: latency_samples, success_rate and active_users
      // are optional on the wire even though the generated type marks them
      // required.
      summary: () =>
        jsonResponse({
          cost_total: 0,
          request_count: 0,
          prompt_tokens_total: 0,
          completion_tokens_total: 0,
          pre_deducted_total: 0,
          refunded_total: 0,
        }),
    })
    renderTab({}, { isAdmin: true })

    await screen.findByText('Total spent')
    expect(screen.queryByText(/NaN/)).toBeNull()
    expect(screen.getByText('0.0%')).toBeTruthy()
    expect(screen.getByText('P95 — · 0 samples')).toBeTruthy()
    expect(screen.getByText('Active users')).toBeTruthy()
  })

  it('isolates a failing section', async () => {
    installStats({
      byModel: () => problemResponse(500, { type: 'server_error', title: 'boom' }),
    })
    renderTab()

    expect(await screen.findByText('Total spent')).toBeTruthy()
    expect(await screen.findByText('Failed to load logs.')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })

  it('renders — instead of 0.0s when there is no latency sample', async () => {
    installStats({
      summary: () =>
        jsonResponse({
          ...SUMMARY,
          p50_latency_ms: 0,
          p95_latency_ms: 0,
          latency_samples: 0,
        }),
    })
    renderTab()

    await screen.findByText('Total spent')
    expect(screen.getByText('—')).toBeTruthy()
    expect(screen.getByText('P95 — · 0 samples')).toBeTruthy()
    expect(screen.queryByText(/NaN/)).toBeNull()
  })

  it('hides the rates line for an empty window', async () => {
    const requests = installStats({
      summary: () =>
        jsonResponse({ ...SUMMARY, request_count: 0, cost_total: 0 }),
      timeseries: () => jsonResponse([]),
    })
    renderTab()

    expect((await screen.findAllByText('No statistics yet')).length).toBeGreaterThan(
      0,
    )
    expect(screen.queryByTestId('stats-rates')).toBeNull()
    expect(screen.queryByTestId('trend-avg')).toBeNull()
    expect(requestsFor(requests, '/api/usage-stats/rates')).toHaveLength(0)
  })
})
