import { renderWithProviders } from '@/test-support/providers'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { WalletOverview } from './wallet-overview'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
})
afterAll(() => server.close())

const SUMMARY = {
  balance: 194660,
  held_credits: 3620,
  total_consumed: 894210,
  total_requests: 18492,
  threshold: 100000,
  health: 'healthy',
  group: 'default',
}

const TOPUP_CONFIG = {
  amount_options: [5000, 10000, 20000, 50000],
  min_topup: 1000,
  max_topup: 10000000,
  quota_per_unit: 100,
  credits_ratio: 1,
  enabled_providers: ['stripe'],
  providers: { stripe: { min_topup: 1000 } },
}

const USAGE_SUMMARY = {
  cost_total: 119040,
  request_count: 4218,
  success_rate: 0.992,
  prompt_tokens_total: 0,
  completion_tokens_total: 0,
  pre_deducted_total: 0,
  refunded_total: 0,
}

const TIMESERIES = [
  { bucket: '2026-10-22T00:00:00Z', cost: 60000, requests: 2000 },
  { bucket: '2026-10-23T00:00:00Z', cost: 59040, requests: 2218 },
]

const TRANSACTIONS = [
  {
    id: 1,
    type: 'consume',
    amount: -1240,
    balance_after: 194660,
    status: 'settled',
    reference_type: 'usage_log',
    reference_id: 'req_a3f8',
    created_at: '2026-10-23T14:32:00Z',
  },
  {
    id: 2,
    type: 'topup',
    amount: 100000,
    balance_after: 195900,
    status: 'confirmed',
    reference_type: 'order',
    reference_id: '20261023-01',
    created_at: '2026-10-23T11:05:00Z',
  },
]

const SUBSCRIPTIONS = {
  items: [
    {
      id: 1,
      plan_title_snapshot: 'Pro Monthly',
      status: 'active',
      start_at: '2026-10-24T00:00:00Z',
      end_at: '2026-11-23T00:00:00Z',
      amount_total: 1000000,
      amount_used: 380000,
      amount_remain: 620000,
      auto_renew: false,
      auto_renew_failures: 0,
      cancel_at_period_end: false,
      period_days: 30,
      grace_days: 0,
      created_at: '2026-10-24T00:00:00Z',
      updated_at: '2026-10-24T00:00:00Z',
    },
  ],
  total: 1,
  page: 1,
  page_size: 100,
}

interface FailOnce {
  summary?: boolean
  usageSummary?: boolean
  subscriptions?: boolean
  timeseries?: boolean
  plans?: boolean
}

interface WalletOverrides {
  summary?: unknown
  transactions?: unknown
  /** Holds the recent-ledger response until the test resolves it. */
  transactionsGate?: Promise<unknown>
  subscriptions?: unknown
  config?: unknown
  timeseries?: unknown
  plans?: unknown
  trendRange?: { value: string | null }
  failOnce?: FailOnce
}

function installWallet(overrides: WalletOverrides = {}) {
  const failed = new Set<keyof FailOnce>()
  function shouldFail(key: keyof FailOnce): boolean {
    if (!overrides.failOnce?.[key] || failed.has(key)) return false
    failed.add(key)
    return true
  }

  server.use(
    http.get(`${ORIGIN}/api/wallet/summary`, () => {
      if (shouldFail('summary')) {
        return HttpResponse.json({ title: 'boom' }, { status: 500 })
      }
      return HttpResponse.json(overrides.summary ?? SUMMARY)
    }),
    http.get(`${ORIGIN}/api/wallet/transactions`, async () => {
      if (overrides.transactionsGate !== undefined) {
        await overrides.transactionsGate
      }
      return HttpResponse.json({
        items: overrides.transactions ?? TRANSACTIONS,
        total: Array.isArray(overrides.transactions)
          ? overrides.transactions.length
          : TRANSACTIONS.length,
        page: 1,
        page_size: 6,
      })
    }),
    http.get(`${ORIGIN}/api/usage-stats/summary`, () => {
      if (shouldFail('usageSummary')) {
        return HttpResponse.json({ title: 'boom' }, { status: 500 })
      }
      return HttpResponse.json(USAGE_SUMMARY)
    }),
    http.get(`${ORIGIN}/api/usage-stats/timeseries`, ({ request }) => {
      if (overrides.trendRange !== undefined) {
        overrides.trendRange.value = new URL(request.url).searchParams.get(
          'range',
        )
      }
      if (shouldFail('timeseries')) {
        return HttpResponse.json({ title: 'boom' }, { status: 500 })
      }
      return HttpResponse.json(overrides.timeseries ?? TIMESERIES)
    }),
    http.get(`${ORIGIN}/api/topup/config`, () =>
      HttpResponse.json(overrides.config ?? TOPUP_CONFIG),
    ),
    http.get(`${ORIGIN}/api/subscriptions`, () => {
      if (shouldFail('subscriptions')) {
        return HttpResponse.json({ title: 'boom' }, { status: 500 })
      }
      return HttpResponse.json(overrides.subscriptions ?? SUBSCRIPTIONS)
    }),
    http.get(`${ORIGIN}/api/subscription/plans`, () => {
      if (shouldFail('plans')) {
        return HttpResponse.json({ title: 'boom' }, { status: 500 })
      }
      return HttpResponse.json(overrides.plans ?? { items: [{ id: 1 }] })
    }),
  )
}

async function renderOverview(onSelectTab = vi.fn(), onTopup = vi.fn()) {
  const result = await renderWithProviders(
    <WalletOverview onSelectTab={onSelectTab} onTopup={onTopup} />,
    ['common', 'account'],
  )
  return { ...result, onSelectTab, onTopup }
}

function quickCard(container: HTMLElement): HTMLElement {
  return (
    within(container).getByRole('heading', { name: 'Quick top-up' })
      .closest('section') ?? container
  )
}

describe('WalletOverview', () => {
  it('shows the balance, spend stats, trend and latest ledger', async () => {
    installWallet()

    const { getByTestId } = await renderOverview()

    await waitFor(() => {
      expect(getByTestId('balance-value').textContent).toBe('194,660')
    })
    expect(screen.getByText('Total spend')).toBeTruthy()
    expect(screen.getByText('894,210')).toBeTruthy()
    expect(screen.getByText('Total requests')).toBeTruthy()
    expect(screen.getByText('18,492')).toBeTruthy()
    expect(screen.getByText('Spending trend')).toBeTruthy()
    expect(await screen.findByTestId('wallet-spend-chart')).toBeTruthy()

    expect(screen.getByText('Recent transactions')).toBeTruthy()
    expect(await screen.findByText('req_a3f8')).toBeTruthy()
    expect(screen.getByText('20261023-01')).toBeTruthy()
    expect(screen.getAllByText('Consumption').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Top-up').length).toBeGreaterThan(0)

    expect(screen.getByText('Quick top-up')).toBeTruthy()
    expect(screen.getByText('My subscription')).toBeTruthy()
    expect(screen.getByText('Pro Monthly')).toBeTruthy()
  })

  it('opens the transactions tab from the ledger header', async () => {
    installWallet()
    const { onSelectTab } = await renderOverview()
    const user = userEvent.setup()

    const button = await screen.findByRole('button', {
      name: 'View all transactions',
    })
    await user.click(button)

    expect(onSelectTab).toHaveBeenCalledWith('transactions')
  })

  it('shows the empty ledger and the no-subscription state', async () => {
    installWallet({
      transactions: [],
      subscriptions: { items: [], total: 0, page: 1, page_size: 100 },
    })

    await renderOverview()

    expect(
      await screen.findByText('No transactions in the last 30 days.'),
    ).toBeTruthy()
    expect(await screen.findByText('No active subscription.')).toBeTruthy()
  })

  it('shows the empty balance state for a new account with no ledger rows', async () => {
    installWallet({
      summary: {
        ...SUMMARY,
        balance: 0,
        held_credits: 0,
        total_consumed: 0,
        total_requests: 0,
        // The server cannot tell "never topped up" from "used up"; it always
        // reports depleted at zero.
        health: 'depleted',
      },
      transactions: [],
    })

    await renderOverview()

    await screen.findByText('No transactions in the last 30 days.')
    expect(await screen.findByText('No balance yet')).toBeTruthy()
    expect(screen.getByText('Top up to start making calls')).toBeTruthy()
    // A brand-new account must not be painted with the danger badge, nor see
    // the held/threshold hint.
    expect(screen.queryByText('Depleted')).toBeNull()
    expect(screen.queryByText(/Held 0 · alert below/)).toBeNull()
  })

  it('does not flash the empty state while the ledger read is still pending', async () => {
    let releaseLedger!: () => void
    const ledgerGate = new Promise<void>((resolve) => {
      releaseLedger = resolve
    })

    installWallet({
      summary: {
        ...SUMMARY,
        balance: 0,
        held_credits: 0,
        total_consumed: 0,
        total_requests: 0,
        health: 'depleted',
      },
      transactions: [],
      transactionsGate: ledgerGate,
    })

    await renderOverview()

    // The summary has settled at zero, but the ledger is still loading: the
    // card must keep the server's health rather than guessing "empty".
    expect(await screen.findByText('Depleted')).toBeTruthy()
    expect(screen.queryByText('No balance yet')).toBeNull()

    await act(async () => {
      releaseLedger()
    })

    expect(await screen.findByText('No balance yet')).toBeTruthy()
    expect(screen.queryByText('Depleted')).toBeNull()
  })

  it('keeps the depleted badge when credits are still held', async () => {
    installWallet({
      summary: {
        ...SUMMARY,
        balance: 0,
        held_credits: 500,
        health: 'depleted',
      },
      transactions: [],
    })

    await renderOverview()

    await screen.findByText('No transactions in the last 30 days.')
    expect(screen.getByText('Depleted')).toBeTruthy()
    expect(screen.queryByText('No balance yet')).toBeNull()
    expect(screen.getByText('Held 500 · alert below 100,000')).toBeTruthy()
  })

  it('keeps the depleted badge when a zero balance has ledger history', async () => {
    installWallet({
      summary: { ...SUMMARY, balance: 0, held_credits: 0, health: 'depleted' },
    })

    await renderOverview()

    expect(await screen.findByText('req_a3f8')).toBeTruthy()
    expect(screen.getByText('Depleted')).toBeTruthy()
    expect(screen.queryByText('No balance yet')).toBeNull()
    expect(screen.getByText('Held 0 · alert below 100,000')).toBeTruthy()
  })

  it('marks a dormant account depleted on lifetime activity outside the recent window', async () => {
    // The recent-ledger endpoint is limited to the last 30 days. Someone who
    // spent their balance down months ago has no recent rows but is not new.
    installWallet({
      summary: {
        ...SUMMARY,
        balance: 0,
        held_credits: 0,
        total_consumed: 894210,
        total_requests: 18492,
        health: 'depleted',
      },
      transactions: [],
    })

    await renderOverview()

    await screen.findByText('No transactions in the last 30 days.')
    expect(await screen.findByText('Depleted')).toBeTruthy()
    expect(screen.queryByText('No balance yet')).toBeNull()
    expect(screen.getByText('Held 0 · alert below 100,000')).toBeTruthy()
  })

  it('keeps the server health for a funded account', async () => {
    installWallet()

    await renderOverview()

    expect(await screen.findByText('req_a3f8')).toBeTruthy()
    expect(screen.getByText('Healthy')).toBeTruthy()
    expect(screen.queryByText('No balance yet')).toBeNull()
    expect(screen.getByText('Held 3,620 · alert below 100,000')).toBeTruthy()
  })

  it('re-queries the trend with the selected range', async () => {
    const trendRange = { value: null as string | null }
    installWallet({ trendRange })

    await renderOverview()
    await waitFor(() => expect(trendRange.value).toBe('7d'))

    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Last 30 days' }))

    await waitFor(() => expect(trendRange.value).toBe('30d'))
  })

  it('shows the quick top-up preview in credits', async () => {
    installWallet()
    const { container } = await renderOverview()

    const card = quickCard(container)
    await waitFor(() => {
      expect(within(card).getByText(/10,000 credits/)).toBeTruthy()
    })
  })

  it('converts a quick chip with the configured credits ratio', async () => {
    installWallet({ config: { ...TOPUP_CONFIG, credits_ratio: 0.8 } })
    const { container } = await renderOverview()

    const card = quickCard(container)
    await waitFor(() => {
      expect(within(card).getByText(/8,000 credits/)).toBeTruthy()
    })
  })

  it('converts the hero yuan view with the configured quota per unit', async () => {
    installWallet({ config: { ...TOPUP_CONFIG, quota_per_unit: 200 } })
    await renderOverview()

    const user = userEvent.setup()
    await user.click(await screen.findByRole('radio', { name: 'Yuan' }))

    expect(screen.getByTestId('balance-value').textContent).toBe('¥973.30')
  })

  it('passes the picked quick amount to the top-up intent', async () => {
    installWallet()
    const { container, onTopup } = await renderOverview()
    const user = userEvent.setup()

    const card = quickCard(container)
    await within(card).findByText(/10,000 credits/)
    await user.click(within(card).getByRole('button', { name: '¥200' }))
    await user.click(
      within(card).getByRole('button', {
        name: 'Top up the selected amount',
      }),
    )

    expect(onTopup).toHaveBeenCalledWith({ cents: 20000, custom: false })
  })

  it('opens the top-up tab in custom mode from the Custom chip', async () => {
    installWallet()
    const { container, onTopup } = await renderOverview()
    const user = userEvent.setup()

    const card = quickCard(container)
    await user.click(await within(card).findByRole('button', { name: 'Custom' }))

    expect(onTopup).toHaveBeenCalledWith({ cents: null, custom: true })
  })

  it('passes the highlighted default from the hero top-up action', async () => {
    installWallet()
    const { container, onTopup } = await renderOverview()
    const user = userEvent.setup()

    await within(quickCard(container)).findByText(/10,000 credits/)
    await user.click(screen.getByRole('button', { name: 'Top up balance' }))

    expect(onTopup).toHaveBeenCalledWith({ cents: 10000, custom: false })
  })

  it('shows a summary error banner and retries the balance', async () => {
    installWallet({ failOnce: { summary: true } })
    await renderOverview()

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      expect.stringContaining('Could not load your balance.'),
    )

    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))

    await waitFor(() => {
      expect(screen.getByTestId('balance-value').textContent).toBe('194,660')
    })
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('marks the usage stats unavailable and retries them', async () => {
    installWallet({ failOnce: { usageSummary: true } })
    const { container } = await renderOverview()

    const consumed = screen.getByText('Total spend').closest('div')
      ?.parentElement as HTMLElement
    await waitFor(() => {
      expect(within(consumed).getByText('Usage unavailable')).toBeTruthy()
    })

    // Every usage footer owns the same retry; one click refetches the summary.
    const retries = screen.getAllByRole('button', { name: 'Retry' })
    expect(retries.length).toBeGreaterThanOrEqual(2)
    await userEvent.setup().click(retries[0] as HTMLElement)

    expect(await screen.findByText('119,040 in this range')).toBeTruthy()
    expect(screen.queryByText('Usage unavailable')).toBeNull()
    expect(container).toBeTruthy()
  })

  it('shows the trend error with a retry that refetches both usage queries', async () => {
    installWallet({ failOnce: { timeseries: true } })
    await renderOverview()

    expect(
      await screen.findByText('Could not load the spending trend.'),
    ).toBeTruthy()

    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))

    expect(await screen.findByTestId('wallet-spend-chart')).toBeTruthy()
    expect(screen.getByText('119,040')).toBeTruthy()
  })

  it('shows the trend empty copy when there is no spend', async () => {
    installWallet({ timeseries: [] })
    await renderOverview()

    expect(await screen.findByText('No spending in this range.')).toBeTruthy()
  })

  it('shows a retry when the subscription read fails', async () => {
    installWallet({ failOnce: { subscriptions: true } })
    await renderOverview()

    expect(
      await screen.findByText('Could not load your subscription.'),
    ).toBeTruthy()
    expect(screen.queryByText('No active subscription.')).toBeNull()

    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))

    expect(await screen.findByText('Pro Monthly')).toBeTruthy()
  })

  it('shows the remaining subscription credits', async () => {
    installWallet()
    await renderOverview()

    expect(
      await screen.findByText('620,000 of 1,000,000 credits left'),
    ).toBeTruthy()
  })

  it('treats a zero-total subscription as unlimited', async () => {
    installWallet({
      subscriptions: {
        items: [
          {
            ...SUBSCRIPTIONS.items[0],
            amount_total: 0,
            amount_used: 0,
            amount_remain: 0,
          },
        ],
        total: 1,
        page: 1,
        page_size: 100,
      },
    })
    await renderOverview()

    expect(await screen.findByText('Unlimited credits')).toBeTruthy()
    expect(screen.queryByText(/of 0 credits left/)).toBeNull()
  })

  it('hides the subscription manage button when no plan is offered', async () => {
    installWallet({
      plans: { items: [] },
      subscriptions: { items: [], total: 0, page: 1, page_size: 100 },
    })
    await renderOverview()

    expect(await screen.findByText('No active subscription.')).toBeTruthy()
    await waitFor(() => {
      expect(
        screen.queryByRole('button', { name: 'Manage subscription' }),
      ).toBeNull()
    })
  })

  it('marks the ledger column headers as column scopes', async () => {
    installWallet()
    await renderOverview()

    // The chart also exposes a semantic table; pick the ledger by one of its
    // reference cells so the scope check keeps testing the ledger.
    const tables = await screen.findAllByRole('table')
    const table = tables.find(
      (candidate) => within(candidate).queryByText('req_a3f8') !== null,
    )
    expect(table).toBeTruthy()
    for (const header of within(table as HTMLElement).getAllByRole(
      'columnheader',
    )) {
      expect(header.getAttribute('scope')).toBe('col')
    }
  })

  it('renders the wide wallet chart with a localised image label', async () => {
    installWallet()
    const { getByTestId } = await renderOverview()

    const wrapper = await waitFor(() => getByTestId('wallet-spend-chart'))
    const svg = within(wrapper).getByRole('img')
    expect(svg.getAttribute('aria-label')).toBe(
      'Spending trend · Last 7 days',
    )
    expect(svg.getAttribute('viewBox')).toBe('0 0 760 220')
    // 5 grid lines, one area polygon, one line, and a marker per daily bucket.
    expect(svg.querySelectorAll('line')).toHaveLength(5)
    expect(svg.querySelectorAll('polygon')).toHaveLength(1)
    expect(svg.querySelectorAll('polyline')).toHaveLength(1)
    expect(svg.querySelectorAll('circle')).toHaveLength(7)
    expect(svg.querySelectorAll('text')).toHaveLength(12)
    expect(within(wrapper).getByRole('table')).toBeTruthy()
    expect(within(wrapper).getAllByRole('img')).toHaveLength(1)
  })

  it('shows absolute ledger times and keeps the ISO instant in the title', async () => {
    installWallet()
    await renderOverview()

    const time = await screen.findByTitle('2026-10-23T14:32:00.000Z')
    expect(time.tagName).toBe('TIME')
    expect(time.getAttribute('datetime')).toBe('2026-10-23T14:32:00Z')
    const expected = new Intl.DateTimeFormat('en', {
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    }).format(new Date('2026-10-23T14:32:00Z'))
    expect(time.textContent).toBe(expected)
  })

  it('shows the success rate to one decimal', async () => {
    installWallet()
    await renderOverview()

    expect(await screen.findByText('99.2% success rate')).toBeTruthy()
  })

  it('offers a compact retry in the trend card when usage stats fail', async () => {
    installWallet({ failOnce: { usageSummary: true } })
    await renderOverview()

    const section = (
      await screen.findByText('Spending trend')
    ).closest('section') as HTMLElement
    expect(within(section).getAllByText('—')).toHaveLength(3)

    await userEvent
      .setup()
      .click(await within(section).findByRole('button', { name: 'Retry' }))

    expect(await within(section).findByText('119,040')).toBeTruthy()
    expect(within(section).queryByText('Usage unavailable')).toBeNull()
  })

  it('collapses the overview grids below the wide breakpoint', async () => {
    installWallet()
    const { getByTestId } = await renderOverview()

    expect(getByTestId('wallet-hero').className).toContain(
      'min-[721px]:grid-cols-2',
    )
    expect(getByTestId('wallet-hero').className).toContain(
      'min-[1181px]:grid-cols-[1.5fr_repeat(3,minmax(0,1fr))]',
    )
    expect(getByTestId('wallet-grid').className).toContain(
      'min-[1181px]:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]',
    )
  })
})
