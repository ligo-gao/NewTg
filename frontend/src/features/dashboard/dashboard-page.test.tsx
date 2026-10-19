import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { DashboardPage } from './dashboard-page'
import {
  type FetchRecord,
  installFetch,
  jsonResponse,
  latestRequest,
  problemResponse,
  renderWithProviders,
  requestsFor,
} from './test-utils'

const ME_USER = {
  username: 'alice',
  email: null,
  role: 'user' as const,
  status: 'enabled' as const,
  avatar_url: null,
  display_name: null,
}

const ME_ADMIN = { ...ME_USER, username: 'root', role: 'admin' as const }

function adminRow(permissions: string[] | null) {
  return {
    id: 1,
    username: 'root',
    display_name: null,
    email: null,
    role: 'admin' as const,
    status: 'enabled' as const,
    balance: 0,
    group: 'default',
    admin_permissions: permissions,
    totp_enabled: false,
    must_change_password: false,
    token_count: 0,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
  }
}

function emptySummary() {
  return {
    cost_total: 0,
    request_count: 0,
    success_rate: 0,
    prompt_tokens_total: 0,
    completion_tokens_total: 0,
    pre_deducted_total: 0,
    refunded_total: 0,
  }
}

function emptyRates() {
  return {
    rpm_avg: 0,
    rpm_peak: 0,
    tpm_avg: 0,
    cost_total: 0,
    pre_deducted_total: 0,
    refunded_total: 0,
  }
}

interface Scenario {
  me?: object
  users?: Response
  summary?: () => Response
  byModel?: () => Response
  timeseries?: () => Response
  rates?: () => Response
}

function installScenario(scenario: Scenario): FetchRecord[] {
  return installFetch((request) => {
    switch (request.url.pathname) {
      case '/api/me':
        return jsonResponse(scenario.me ?? ME_USER)
      case '/api/users':
        return (
          scenario.users ??
          jsonResponse({ items: [adminRow(null)], total: 1, page: 1, page_size: 100 })
        )
      case '/api/usage-stats/summary':
        return scenario.summary?.() ?? jsonResponse(emptySummary())
      case '/api/usage-stats/by-model':
        return scenario.byModel?.() ?? jsonResponse([])
      case '/api/usage-stats/timeseries':
        return scenario.timeseries?.() ?? jsonResponse([])
      case '/api/usage-stats/rates':
        return scenario.rates?.() ?? jsonResponse(emptyRates())
      default:
        return problemResponse(404, { type: 'about:blank', title: 'not found' })
    }
  })
}

const STAT_PATHS = [
  '/api/usage-stats/summary',
  '/api/usage-stats/by-model',
  '/api/usage-stats/timeseries',
  '/api/usage-stats/rates',
] as const

describe('DashboardPage', () => {
  it('renders zero data as 0 / -- and never NaN', async () => {
    installScenario({})
    renderWithProviders(<DashboardPage />)

    await screen.findAllByText('No statistics yet')

    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe('0')
      expect(screen.getByTestId('card-requests-value').textContent).toBe('0')
      expect(screen.getByTestId('card-input-tokens-value').textContent).toBe('0')
      expect(screen.getByTestId('card-output-tokens-value').textContent).toBe('0')
      expect(screen.getByTestId('card-requests-subline').textContent).toContain('--')
      expect(screen.getByTestId('rate-usage-value').textContent).toBe('0')
      expect(screen.getByTestId('rate-rpm-value').textContent).toBe('0')
      expect(screen.getByTestId('rate-tpm-value').textContent).toBe('0')
    })

    // Empty ring: no broken arc, no fake total.
    expect(screen.getByTestId('donut-empty')).toBeTruthy()
    expect(screen.queryAllByTestId('donut-segment')).toHaveLength(0)
    expect(screen.getByTestId('trend-empty')).toBeTruthy()
    expect(document.body.textContent ?? '').not.toContain('NaN')
  })

  it('formats card values, success rate and the pre-deducted/refunded hover', async () => {
    const user = userEvent.setup()
    installScenario({
      summary: () =>
        jsonResponse({
          cost_total: 12480,
          request_count: 1024,
          success_rate: 0.982,
          prompt_tokens_total: 1_200_000,
          completion_tokens_total: 356_000,
          pre_deducted_total: 14200,
          refunded_total: 1720,
        }),
    })
    renderWithProviders(<DashboardPage />)

    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe('12,480')
      expect(screen.getByTestId('card-requests-value').textContent).toBe('1,024')
      expect(
        screen.getByTestId('card-requests-subline').textContent,
      ).toContain('98.2%')
      expect(screen.getByTestId('card-input-tokens-value').textContent).toBe('1.2M')
      expect(screen.getByTestId('card-output-tokens-value').textContent).toBe(
        '356,000',
      )
    })

    await user.hover(screen.getByTestId('card-total-spent'))
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip.textContent).toContain('14,200')
    expect(tooltip.textContent).toContain('1,720')
    expect(tooltip.textContent).toContain('Pre-deducted')
    expect(tooltip.textContent).toContain('Refunded')
  })

  it('switches the range for every query and picks hour vs day granularity', async () => {
    const user = userEvent.setup()
    const requests = installScenario({})
    renderWithProviders(<DashboardPage />)

    await waitFor(() => {
      expect(requestsFor(requests, '/api/usage-stats/summary').length).toBeGreaterThan(0)
    })

    const initialTimeseries = latestRequest(requests, '/api/usage-stats/timeseries')
    expect(initialTimeseries?.url.searchParams.get('range')).toBe('24h')
    expect(initialTimeseries?.url.searchParams.get('granularity')).toBe('hour')

    await user.click(screen.getByRole('radio', { name: '7d' }))

    await waitFor(() => {
      for (const path of STAT_PATHS) {
        expect(latestRequest(requests, path)?.url.searchParams.get('range')).toBe('7d')
      }
    })
    expect(
      latestRequest(requests, '/api/usage-stats/timeseries')?.url.searchParams.get(
        'granularity',
      ),
    ).toBe('day')

    await user.click(screen.getByRole('radio', { name: '30d' }))
    await waitFor(() => {
      for (const path of STAT_PATHS) {
        expect(latestRequest(requests, path)?.url.searchParams.get('range')).toBe('30d')
      }
    })
    expect(
      latestRequest(requests, '/api/usage-stats/timeseries')?.url.searchParams.get(
        'granularity',
      ),
    ).toBe('day')
    expect(requestsFor(requests, '/api/usage-stats/summary').length).toBeGreaterThanOrEqual(
      3,
    )
  })

  it('folds nine models into eight slices plus Other with percentages totalling 100', async () => {
    const items = Array.from({ length: 9 }, (_, index) => ({
      model: `model-${index + 1}`,
      // Descending cost so model-1 is the largest and model-9 becomes Other.
      cost: 900 - index * 100,
      tokens: 1000,
      percent: 0,
    }))
    installScenario({ byModel: () => jsonResponse(items) })
    renderWithProviders(<DashboardPage />)

    await screen.findByTestId('model-donut')

    for (let index = 1; index <= 8; index += 1) {
      expect(screen.getByTestId(`donut-legend-model-${index}`)).toBeTruthy()
    }
    expect(screen.getByTestId('donut-legend-other')).toBeTruthy()
    expect(screen.queryByTestId('donut-legend-model-9')).toBeNull()
    expect(screen.getAllByTestId('donut-segment')).toHaveLength(9)

    const percentages = screen
      .getAllByTestId(/donut-legend-/)
      .map((item) => Number(item.getAttribute('data-percent')))
    expect(percentages.reduce((sum, value) => sum + value, 0)).toBeCloseTo(100, 6)
  })

  it('keeps the username filter away from ordinary users and never sends it', async () => {
    const requests = installScenario({})
    renderWithProviders(<DashboardPage />)

    await screen.findAllByText('No statistics yet')
    expect(screen.queryByLabelText('Username')).toBeNull()
    expect(requestsFor(requests, '/api/users')).toHaveLength(0)

    const statsRequests = requests.filter((request) =>
      request.url.pathname.startsWith('/api/usage-stats/'),
    )
    expect(statsRequests.length).toBeGreaterThan(0)
    for (const request of statsRequests) {
      expect(request.url.searchParams.has('username')).toBe(false)
    }
  })

  it('shows the username filter to an admin with logs:read and refetches every query with it', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      me: ME_ADMIN,
      users: jsonResponse({
        items: [adminRow(['logs:read'])],
        total: 1,
        page: 1,
        page_size: 100,
      }),
    })
    renderWithProviders(<DashboardPage />)

    const input = await screen.findByLabelText('Username')
    await user.type(input, 'bob')
    await user.click(screen.getByRole('button', { name: 'Apply' }))

    await waitFor(() => {
      for (const path of STAT_PATHS) {
        expect(latestRequest(requests, path)?.url.searchParams.get('username')).toBe(
          'bob',
        )
      }
    })
  })

  it('hides the username filter from an admin who lacks logs:read', async () => {
    installScenario({
      me: ME_ADMIN,
      users: jsonResponse({
        items: [adminRow([])],
        total: 1,
        page: 1,
        page_size: 100,
      }),
    })
    renderWithProviders(<DashboardPage />)

    await screen.findAllByText('No statistics yet')
    expect(screen.queryByLabelText('Username')).toBeNull()
  })

  it('renders card and chart errors as -- with retries that refetch', async () => {
    const fail = () =>
      problemResponse(500, { type: 'about:blank', title: 'Internal Server Error' })
    const requests = installScenario({
      summary: fail,
      byModel: fail,
      timeseries: fail,
      rates: fail,
    })
    const user = userEvent.setup()
    renderWithProviders(<DashboardPage />)

    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe('--')
      expect(screen.getByTestId('rate-usage-value').textContent).toBe('--')
    })
    expect(await screen.findByTestId('donut-error')).toBeTruthy()
    expect(await screen.findByTestId('trend-error')).toBeTruthy()

    const before = requestsFor(requests, '/api/usage-stats/timeseries').length
    await user.click(screen.getByTestId('trend-retry'))
    await waitFor(() => {
      expect(requestsFor(requests, '/api/usage-stats/timeseries').length).toBeGreaterThan(
        before,
      )
    })
  })

  it('renders RPM/TPM values with peak and exact hover values', async () => {
    const user = userEvent.setup()
    installScenario({
      rates: () =>
        jsonResponse({
          rpm_avg: 3.4,
          rpm_peak: 12,
          tpm_avg: 86000,
          cost_total: 12480,
          pre_deducted_total: 14200,
          refunded_total: 1720,
        }),
    })
    renderWithProviders(<DashboardPage />)

    await waitFor(() => {
      expect(screen.getByTestId('rate-usage-value').textContent).toBe('12,480')
      expect(screen.getByTestId('rate-rpm-value').textContent).toBe('3')
      expect(screen.getByTestId('rate-tpm-value').textContent).toBe('86K')
    })

    await user.hover(screen.getByTestId('rate-rpm'))
    expect((await screen.findByRole('tooltip')).textContent).toContain('Peak 12')
    await user.unhover(screen.getByTestId('rate-rpm'))

    await user.hover(screen.getByTestId('rate-tpm'))
    expect((await screen.findByRole('tooltip')).textContent).toContain('86,000')
  })

  it('drills the trend into a selected model and resets the filter', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      byModel: () =>
        jsonResponse([
          { model: 'gpt-4o', cost: 60, tokens: 10, percent: 60 },
          { model: 'claude-4', cost: 40, tokens: 10, percent: 40 },
        ]),
    })
    renderWithProviders(<DashboardPage />)

    const legendItem = await screen.findByTestId('donut-legend-gpt-4o')
    await user.click(within(legendItem).getByRole('button'))

    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/usage-stats/timeseries')?.url.searchParams.get(
          'model',
        ),
      ).toBe('gpt-4o')
    })
    expect(screen.getByTestId('trend-filter-chip').textContent).toContain('gpt-4o')

    await user.click(screen.getByRole('button', { name: 'Reset model filter' }))
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/usage-stats/timeseries')?.url.searchParams.has(
          'model',
        ),
      ).toBe(false)
    })
  })

  it('feeds the trend chart UTC ISO buckets from the timeseries endpoint', async () => {
    installScenario({
      timeseries: () =>
        jsonResponse([
          { bucket: '2026-10-18T01:00:00Z', cost: 5, requests: 2 },
          { bucket: '2026-10-18T02:00:00Z', cost: 7, requests: 3 },
        ]),
    })
    renderWithProviders(<DashboardPage />)

    await screen.findByTestId('analytics-chart')
    expect(screen.getByTestId('analytics-chart').getAttribute('data-kind')).toBe('area')

    const table = screen.getByRole('table')
    expect(
      within(table).getByText('2026-10-18T01:00:00.000Z'),
    ).toBeTruthy()
    expect(within(table).getByText('2026-10-18T02:00:00.000Z')).toBeTruthy()
    expect(within(table).getByText('5')).toBeTruthy()
    expect(within(table).getByText('7')).toBeTruthy()
  })
})
