import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { DashboardPage } from './dashboard-page'
import { formatCurrencyValue } from './format'
import {
  installFetch,
  jsonResponse,
  requestsFor,
  renderWithProviders,
} from './test-utils'

const ME_USER = {
  username: 'alice',
  email: null,
  role: 'user' as const,
  status: 'enabled' as const,
  avatar_url: null,
  display_name: null,
  permissions: [] as string[],
}

const ME_ADMIN = {
  ...ME_USER,
  username: 'root',
  role: 'admin' as const,
  permissions: ['logs:read', 'pricing:read', 'settings:manage'],
}

const PUBLIC_STATUS = {
  billing: { display_unit: 'credits', quota_per_unit: 100, currency_symbol: '¥' },
  auth: { turnstile: { enabled: false, site_key: '' } },
  checkin_enabled: false,
  playground_enabled: false,
}

const SUMMARY = {
  cost_total: 1234,
  request_count: 10,
  success_rate: 0.9,
  prompt_tokens_total: 1_000,
  completion_tokens_total: 500,
  pre_deducted_total: 1_300,
  refunded_total: 66,
  p50_latency_ms: 100,
  p95_latency_ms: 250,
  latency_samples: 9,
  active_users: 2,
}

const RATES = {
  rpm_avg: 1.2,
  rpm_peak: 4,
  tpm_avg: 300,
  cost_total: 1234,
  pre_deducted_total: 1300,
  refunded_total: 66,
}

const TIMESERIES = [
  { bucket: '2026-10-23T01:00:00Z', cost: 500, requests: 4 },
  { bucket: '2026-10-23T02:00:00Z', cost: 734, requests: 6 },
]

const BY_MODEL = [
  { model: 'deepseek-flash', cost: 900, tokens: 1_200, percent: 72.9 },
  { model: 'deepseek-v4-pro', cost: 334, tokens: 300, percent: 27.1 },
]

const BY_USER = {
  items: [
    { user_id: 1, username: 'root', cost: 800, requests: 6, tokens: 900, percent: 64.8 },
    { user_id: 2, username: 'alice', cost: 434, requests: 4, tokens: 600, percent: 35.2 },
  ],
  total: 1234,
}

const BY_GROUP = {
  items: [{ group: 'default', cost: 1234, requests: 10, tokens: 1_500, percent: 100 }],
  total: 1234,
}

const FAILURES = {
  items: [
    {
      id: 1,
      request_id: '6f6c2c1e-1111-4111-8111-111111111111',
      model: 'deepseek-flash',
      prompt_tokens: 10,
      completion_tokens: 0,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
      cost: 0,
      refunded: 0,
      stream: false,
      status: 'error',
      created_at: '2026-10-23T02:00:00Z',
      error_class: 'upstream_429',
      latency_ms: 842,
      username: 'root',
    },
  ],
  total: 1,
  page: 1,
  page_size: 5,
}

const CANDIDATES = {
  items: [
    { model: 'deepseek-flash', display_name: null, vendor: null, context_length: null, sources: ['pricing'], priced: true, mode: 'per_token' },
    { model: 'deepseek-v4-pro', display_name: null, vendor: null, context_length: null, sources: ['channel'], priced: false, mode: 'per_token' },
  ],
}

const SYSTEM = {
  dependencies: { postgres: 'ok', redis: 'ok' },
  instances: [{ name: 'gateway-1' }, { name: 'gateway-2' }],
}

const PERF = {
  window: { range: '24h', start: '', end: '', granularity: '1m' },
  buckets: [],
  summary: {},
  incidents: [],
  models: [],
  pipeline: { enabled: true, updated_at: null, stale: false },
  comparison: null,
}

const WALLET = {
  balance: 12_860,
  held_credits: 210,
  total_consumed: 184_230,
  total_requests: 6318,
  threshold: 5_000,
  health: 'ok',
  group: 'default',
}

function scenario(me: object = ME_ADMIN) {
  return installFetch((request) => {
    switch (request.url.pathname) {
      case '/api/me':
        return jsonResponse(me)
      case '/api/status':
        return jsonResponse(PUBLIC_STATUS)
      case '/api/usage-stats/summary':
        return jsonResponse(SUMMARY)
      case '/api/usage-stats/rates':
        return jsonResponse(RATES)
      case '/api/usage-stats/timeseries':
        return jsonResponse(TIMESERIES)
      case '/api/usage-stats/by-model':
        return jsonResponse(BY_MODEL)
      case '/api/usage-stats/by-user':
        return jsonResponse(BY_USER)
      case '/api/usage-stats/by-group':
        return jsonResponse(BY_GROUP)
      case '/api/usage-logs':
        return jsonResponse(FAILURES)
      case '/api/model-pricing/candidates':
        return jsonResponse(CANDIDATES)
      case '/api/system/status':
        return jsonResponse(SYSTEM)
      case '/api/model-perf':
        return jsonResponse(PERF)
      case '/api/wallet/summary':
        return jsonResponse(WALLET)
      default:
        return jsonResponse({})
    }
  })
}

describe('DashboardPage overview', () => {
  it('renders the four core metrics and the admin panels', async () => {
    const requests = scenario(ME_ADMIN)
    renderWithProviders(<DashboardPage />)

    expect((await screen.findByTestId('metric-spend')).textContent).toContain('1,234')
    expect(screen.getByTestId('metric-requests').textContent).toContain('10')
    expect(screen.getByTestId('metric-success').textContent).toContain('90.0%')
    expect(screen.getByTestId('metric-p95').textContent).toContain('250ms')

    const attention = await screen.findByTestId('attention-panel')
    await waitFor(() => {
      expect(within(attention).getByText(/deepseek-v4-pro/)).toBeTruthy()
    })

    const composition = await screen.findByTestId('composition-users')
    await waitFor(() => {
      expect(within(composition).getByText('root')).toBeTruthy()
      expect(within(composition).getByText('alice')).toBeTruthy()
    })

    const failures = await screen.findByTestId('failures-table')
    await waitFor(() => {
      expect(within(failures).getByText('upstream_429')).toBeTruthy()
    })

    await waitFor(() => {
      expect(screen.getByTestId('health-panel').textContent).toContain('PostgreSQL')
    })
    expect(requestsFor(requests, '/api/usage-stats/by-user')).toHaveLength(1)
  })

  it('shows the balance and personal tables to an ordinary user', async () => {
    const requests = scenario(ME_USER)
    renderWithProviders(<DashboardPage />)

    await screen.findByTestId('metric-balance')
    await waitFor(() => {
      expect(screen.getByTestId('metric-balance').textContent).toContain('¥128.60')
    })
    expect(screen.queryByTestId('composition-users')).toBeNull()
    expect(screen.queryByTestId('attention-panel')).toBeNull()
    const recent = await screen.findByTestId('recent-table')
    await waitFor(() => {
      expect(within(recent).getByText('deepseek-flash')).toBeTruthy()
    })
    expect(requestsFor(requests, '/api/usage-stats/by-user')).toHaveLength(0)
    expect(requestsFor(requests, '/api/wallet/summary')).toHaveLength(1)
  })

  it('switches the range for the core queries', async () => {
    const requests = scenario(ME_ADMIN)
    renderWithProviders(<DashboardPage />)
    await screen.findByTestId('metric-spend')

    await userEvent.click(screen.getByTestId('range-7d'))
    await waitFor(() => {
      const summary = requestsFor(requests, '/api/usage-stats/summary').at(-1)
      expect(summary?.url.searchParams.get('range')).toBe('7d')
    })
  })

  it('switches the trend metric without dropping the chart', async () => {
    scenario(ME_ADMIN)
    renderWithProviders(<DashboardPage />)
    await screen.findByTestId('metric-spend')

    await userEvent.click(screen.getByTestId('trend-metric-requests'))
    expect(screen.getByTestId('trend-metric-requests').getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByTestId('dashboard-trend')).toBeTruthy()
  })

  it('renders -- on a summary failure and offers a retry', async () => {
    installFetch((request) => {
      switch (request.url.pathname) {
        case '/api/me':
          return jsonResponse(ME_ADMIN)
        case '/api/status':
          return jsonResponse(PUBLIC_STATUS)
        case '/api/usage-stats/summary':
          return jsonResponse({ title: 'boom' }, 500)
        case '/api/usage-stats/rates':
          return jsonResponse(RATES)
        case '/api/usage-stats/timeseries':
          return jsonResponse(TIMESERIES)
        case '/api/usage-stats/by-model':
          return jsonResponse(BY_MODEL)
        case '/api/usage-stats/by-user':
          return jsonResponse(BY_USER)
        case '/api/usage-stats/by-group':
          return jsonResponse(BY_GROUP)
        case '/api/usage-logs':
          return jsonResponse(FAILURES)
        case '/api/model-pricing/candidates':
          return jsonResponse(CANDIDATES)
        case '/api/system/status':
          return jsonResponse(SYSTEM)
        case '/api/model-perf':
          return jsonResponse(PERF)
        default:
          return jsonResponse({})
      }
    })
    renderWithProviders(<DashboardPage />)

    expect((await screen.findByText('--', { selector: '[data-testid="metric-spend"]' })).textContent).toBe('--')
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })

  it('never renders NaN for an empty window', async () => {
    installFetch((request) => {
      switch (request.url.pathname) {
        case '/api/me':
          return jsonResponse(ME_ADMIN)
        case '/api/status':
          return jsonResponse(PUBLIC_STATUS)
        case '/api/usage-stats/summary':
          return jsonResponse({
            ...SUMMARY,
            cost_total: 0,
            request_count: 0,
            success_rate: 0,
            prompt_tokens_total: 0,
            completion_tokens_total: 0,
            p50_latency_ms: 0,
            p95_latency_ms: 0,
            latency_samples: 0,
            active_users: 0,
          })
        case '/api/usage-stats/rates':
          return jsonResponse({ ...RATES, rpm_avg: 0, rpm_peak: 0, tpm_avg: 0 })
        case '/api/usage-stats/timeseries':
          return jsonResponse([])
        case '/api/usage-stats/by-model':
          return jsonResponse([])
        case '/api/usage-stats/by-user':
          return jsonResponse({ items: [], total: 0 })
        case '/api/usage-stats/by-group':
          return jsonResponse({ items: [], total: 0 })
        case '/api/usage-logs':
          return jsonResponse({ items: [], total: 0, page: 1, page_size: 5 })
        case '/api/model-pricing/candidates':
          return jsonResponse({ items: [] })
        case '/api/system/status':
          return jsonResponse(SYSTEM)
        case '/api/model-perf':
          return jsonResponse(PERF)
        default:
          return jsonResponse({})
      }
    })
    renderWithProviders(<DashboardPage />)

    expect((await screen.findByTestId('metric-spend')).textContent).toBe('0')
    expect(screen.getByTestId('metric-success').textContent).toBe('--')
    expect(document.body.textContent).not.toMatch(/NaN/)
  })
})

describe('formatCurrencyValue', () => {
  it('converts credits to the display amount at the configured quota per unit', () => {
    expect(formatCurrencyValue(12_345, 100, '¥', 'en-US')).toBe('¥123.45')
  })

  it('returns -- for values it cannot convert', () => {
    expect(formatCurrencyValue(Number.NaN, 100, '¥', 'en-US')).toBe('--')
    expect(formatCurrencyValue(100, 0, '¥', 'en-US')).toBe('--')
  })
})
