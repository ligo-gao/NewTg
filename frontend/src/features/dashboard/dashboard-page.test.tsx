import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { DashboardPage } from './dashboard-page'
import { formatCurrencyValue } from './format'
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
  permissions: [] as string[],
}

const ME_ADMIN = {
  ...ME_USER,
  username: 'root',
  role: 'admin' as const,
  permissions: ['logs:read'] as string[],
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
  status?: () => Response
  summary?: () => Response
  byModel?: () => Response
  timeseries?: () => Response
  rates?: () => Response
  modelPerf?: () => Response
}

/**
 * 管道未启用的闸门响应（F-05-08）。概览测试不关心性能看板，但返回一个
 * 明确的闸门比让新section落到 404 错误态更接近真实部署。
 */
function disabledModelPerf() {
  return {
    window: { range: '24h', start: '', end: '', granularity: '1m' },
    buckets: [],
    summary: {},
    incidents: [],
    models: [],
    pipeline: { enabled: false, updated_at: null, stale: true },
    comparison: null,
  }
}

/**
 * Minimal `/api/status` payload carrying the billing projection the dashboard
 * reads. `auth.turnstile` is required by CheckinCard's read, so it is present
 * (disabled) even though these tests never exercise check-in.
 */
function statusWithBilling(billing: Record<string, unknown>) {
  return jsonResponse({
    auth: { turnstile: { enabled: false, site_key: '' } },
    billing,
  })
}

function installScenario(scenario: Scenario): FetchRecord[] {
  return installFetch((request) => {
    switch (request.url.pathname) {
      case '/api/me':
        return jsonResponse(scenario.me ?? ME_USER)
      case '/api/status':
        return scenario.status !== undefined
          ? scenario.status()
          : problemResponse(404, { type: 'about:blank', title: 'not found' })
      case '/api/usage-stats/summary':
        return scenario.summary?.() ?? jsonResponse(emptySummary())
      case '/api/usage-stats/by-model':
        return scenario.byModel?.() ?? jsonResponse([])
      case '/api/usage-stats/timeseries':
        return scenario.timeseries?.() ?? jsonResponse([])
      case '/api/usage-stats/rates':
        return scenario.rates?.() ?? jsonResponse(emptyRates())
      case '/api/model-perf':
        return scenario.modelPerf?.() ?? jsonResponse(disabledModelPerf())
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

afterEach(() => {
  vi.useRealTimers()
})

describe('DashboardPage', () => {
  it('renders zero data as 0 / no-data hints and never NaN', async () => {
    installScenario({})
    renderWithProviders(<DashboardPage />)

    await screen.findAllByText('No statistics yet')

    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe('0')
      expect(screen.getByTestId('card-requests-value').textContent).toBe('0')
      expect(screen.getByTestId('card-input-tokens-value').textContent).toBe('0')
      expect(screen.getByTestId('card-output-tokens-value').textContent).toBe('0')
      // No requests means no success rate to report; the card says so instead
      // of printing a misleading "--".
      expect(screen.getByTestId('card-requests-subline').textContent).toBe(
        'No data',
      )
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

  it('names the overview region after the page heading', async () => {
    installScenario({})
    renderWithProviders(<DashboardPage />)

    // The visible h2/h1 lives in PageHeader, outside this section; the region
    // keeps an accessible name of its own (deferred review #18).
    expect(
      await screen.findByRole('region', { name: 'Overview' }),
    ).toBeTruthy()
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

  it('keeps credit rendering when billing.display_unit is credits', async () => {
    installScenario({
      status: () =>
        statusWithBilling({
          display_unit: 'credits',
          quota_per_unit: 100,
          currency_symbol: '¥',
        }),
      summary: () =>
        jsonResponse({ ...emptySummary(), cost_total: 12480, request_count: 10 }),
      rates: () => jsonResponse({ ...emptyRates(), cost_total: 6400 }),
    })
    renderWithProviders(<DashboardPage />)

    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe(
        '12,480',
      )
      expect(screen.getByTestId('rate-usage-value').textContent).toBe('6,400')
    })
    expect(
      within(screen.getByTestId('card-total-spent')).getByText('credits'),
    ).toBeTruthy()
    expect(
      within(screen.getByTestId('rate-usage')).getByText('credits'),
    ).toBeTruthy()
    expect(
      within(screen.getByTestId('card-total-spent')).queryByText('¥'),
    ).toBeNull()
  })

  it('falls back to credit rendering when /api/status is unavailable', async () => {
    installScenario({
      status: () =>
        problemResponse(503, {
          type: 'about:blank',
          title: 'Service Unavailable',
        }),
      summary: () =>
        jsonResponse({ ...emptySummary(), cost_total: 12480, request_count: 10 }),
      rates: () => jsonResponse({ ...emptyRates(), cost_total: 6400 }),
    })
    renderWithProviders(<DashboardPage />)

    // An unknown unit must never blank the cards or crash the page: the
    // credit presentation (and its unit label) is the safe default.
    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe(
        '12,480',
      )
      expect(screen.getByTestId('rate-usage-value').textContent).toBe('6,400')
    })
    expect(
      within(screen.getByTestId('card-total-spent')).getByText('credits'),
    ).toBeTruthy()
    const rateCard = screen.getByTestId('rate-usage')
    expect(within(rateCard).getByText('Credits used')).toBeTruthy()
    expect(within(rateCard).queryByText('Spend')).toBeNull()
    expect(within(rateCard).queryByText('¥')).toBeNull()
  })

  it('renders spend in the configured currency while the tooltip stays in credits', async () => {
    const user = userEvent.setup()
    installScenario({
      status: () =>
        statusWithBilling({
          display_unit: 'cny',
          quota_per_unit: 100,
          currency_symbol: '¥',
        }),
      summary: () =>
        jsonResponse({
          ...emptySummary(),
          cost_total: 12480,
          request_count: 10,
          pre_deducted_total: 14200,
          refunded_total: 1720,
        }),
      rates: () =>
        jsonResponse({
          ...emptyRates(),
          cost_total: 6400,
          pre_deducted_total: 6400,
        }),
    })
    renderWithProviders(<DashboardPage />)

    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe(
        '¥124.80',
      )
      expect(screen.getByTestId('rate-usage-value').textContent).toBe('¥64.00')
    })
    // The currency value carries its own symbol; the separate "credits" unit
    // would contradict it.
    const spendCard = screen.getByTestId('card-total-spent')
    expect(within(spendCard).queryByText('credits')).toBeNull()
    const rateCard = screen.getByTestId('rate-usage')
    expect(within(rateCard).queryByText('credits')).toBeNull()
    // "Credits used" would contradict the ¥ figure, so the rate card switches
    // to the plain "Spend" label.
    expect(within(rateCard).getByText('Spend')).toBeTruthy()
    expect(within(rateCard).queryByText('Credits used')).toBeNull()

    // The pre-deducted/refunded hover is a ledger fact and stays in credits.
    await user.hover(spendCard)
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip.textContent).toContain('14,200')
    expect(tooltip.textContent).toContain('1,720')
  })

  it('honours a non-default quota_per_unit and symbol', async () => {
    installScenario({
      status: () =>
        statusWithBilling({
          display_unit: 'cny',
          quota_per_unit: 200,
          currency_symbol: '$',
        }),
      summary: () => jsonResponse({ ...emptySummary(), cost_total: 10000 }),
    })
    renderWithProviders(<DashboardPage />)

    // 10,000 credits at 200 credits/unit = 5,000 cents = 50.00.
    await waitFor(() => {
      expect(screen.getByTestId('card-total-spent-value').textContent).toBe(
        '$50.00',
      )
    })
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

    const rangeGroup = screen.getByRole('radiogroup', { name: 'Time range' })
    await user.click(within(rangeGroup).getByRole('radio', { name: '7d' }))

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
      me: { ...ME_ADMIN, permissions: ['logs:read'] },
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
    // Capability now comes from /api/me; the users list is never probed.
    expect(requestsFor(requests, '/api/users')).toHaveLength(0)
  })

  it('hides the username filter from an admin who lacks logs:read', async () => {
    installScenario({
      me: { ...ME_ADMIN, permissions: [] },
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

  it('renders the four quick actions below the shared page header', async () => {
    installScenario({})
    renderWithProviders(<DashboardPage />)

    const createToken = await screen.findByRole('link', { name: /Create token/ })
    expect(createToken.getAttribute('href')).toBe('/tokens')
    expect(screen.getByRole('link', { name: /Top up/ }).getAttribute('href')).toBe(
      '/wallet',
    )
    expect(screen.getByRole('link', { name: /View logs/ }).getAttribute('href')).toBe(
      '/dashboard/usage-logs',
    )
    expect(screen.getByRole('link', { name: /Playground/ }).getAttribute('href')).toBe(
      '/playground',
    )
    // The old inline heading is gone: the shared header owns the page title.
    expect(screen.getByTestId('page-header')).toBeTruthy()
    expect(screen.getByRole('heading', { name: 'Overview' })).toBeTruthy()
  })

  it('zero-fills the trend across the whole selected window with UTC buckets', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-10-18T12:34:00Z'))

    installScenario({
      timeseries: () =>
        jsonResponse([
          { bucket: '2026-10-18T01:00:00Z', cost: 5, requests: 2 },
          { bucket: '2026-10-18T05:00:00Z', cost: 7, requests: 3 },
        ]),
    })
    renderWithProviders(<DashboardPage />)

    await screen.findByTestId('analytics-chart')
    expect(screen.getByTestId('analytics-chart').getAttribute('data-kind')).toBe('area')

    const table = screen.getByRole('table')
    const rowFor = (ts: string): HTMLElement => {
      const cell = within(table).getByText(ts)
      const row = cell.closest('tr')
      if (row === null) throw new Error(`no row for ${ts}`)
      return row
    }

    // The spine covers the full 24h window, not just first→last active bucket.
    expect(rowFor('2026-10-17T13:00:00.000Z')).toBeTruthy()
    expect(rowFor('2026-10-18T12:00:00.000Z')).toBeTruthy()
    expect(within(table).getAllByRole('row')).toHaveLength(25)

    // Inactive hours are materialised as zero instead of being skipped.
    for (const ts of [
      '2026-10-18T02:00:00.000Z',
      '2026-10-18T03:00:00.000Z',
      '2026-10-18T04:00:00.000Z',
    ]) {
      expect(within(rowFor(ts)).getByText('0')).toBeTruthy()
    }
    expect(within(rowFor('2026-10-18T01:00:00.000Z')).getByText('5')).toBeTruthy()
    expect(within(rowFor('2026-10-18T05:00:00.000Z')).getByText('7')).toBeTruthy()
  })
})

describe('formatCurrencyValue', () => {
  it('converts credits to the display amount at the configured quota per unit', () => {
    // credits = cents * quota_per_unit / 100, so 12,480 credits at 100 = 124.80.
    expect(formatCurrencyValue(12480, 100, '¥', 'en')).toBe('¥124.80')
    expect(formatCurrencyValue(10000, 200, '$', 'en')).toBe('$50.00')
    expect(formatCurrencyValue(0, 100, '¥', 'en')).toBe('¥0.00')
  })

  it('localises the decimal separator', () => {
    expect(formatCurrencyValue(12480, 100, '¥', 'zh-CN')).toBe('¥124.80')
    expect(formatCurrencyValue(12480, 100, '¥', 'fr')).toBe('¥124,80')
  })

  it('returns -- for values it cannot convert', () => {
    expect(formatCurrencyValue(Number.NaN, 100, '¥', 'en')).toBe('--')
    expect(formatCurrencyValue(-1, 100, '¥', 'en')).toBe('--')
    expect(formatCurrencyValue(100, 0, '¥', 'en')).toBe('--')
    expect(formatCurrencyValue(100, Number.NaN, '¥', 'en')).toBe('--')
  })
})
