import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SubscriptionSection } from './subscription-section'

vi.mock('@stripe/stripe-js', () => ({
  loadStripe: vi.fn(() => Promise.resolve({})),
}))
vi.mock('@stripe/react-stripe-js', () => ({
  Elements: ({ children }: { children: React.ReactNode }) => children,
  CardElement: () => <div data-testid="card-element" />,
  useStripe: () => null,
  useElements: () => null,
}))

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => {
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
  server.listen({ onUnhandledRequest: 'error' })
})
afterEach(() => {
  server.resetHandlers()
  Object.defineProperty(window.navigator, 'onLine', {
    configurable: true,
    value: true,
  })
})
afterAll(() => server.close())

const BASIC = {
  id: 1,
  plan_key: 'basic',
  title: 'Basic',
  description: 'Starter plan',
  period_days: 30,
  price_amount: 5000,
  currency: 'CNY',
  grant_credits: 1000,
  reset_on_renew: true,
  model_scope: null,
  concurrency_limit: null,
  grace_days: 0,
  sort_order: 1,
}
/** Same cycle, cheaper: the change path is provably zero-delta. */
const LITE = {
  ...BASIC,
  id: 3,
  plan_key: 'lite',
  title: 'Lite',
  description: null,
  price_amount: 2000,
  grant_credits: 500,
  sort_order: 3,
}
/** Same cycle, more expensive: paid path with a prorated delta. */
const PRO = {
  ...BASIC,
  id: 2,
  plan_key: 'pro',
  title: 'Pro',
  description: null,
  price_amount: 9000,
  grant_credits: 2000,
  sort_order: 2,
}

const ACTIVE_SUB = {
  id: 5,
  plan_title_snapshot: 'Basic',
  status: 'active',
  start_at: '2026-10-01T00:00:00Z',
  end_at: '2099-10-31T00:00:00Z',
  next_reset_at: '2099-10-31T00:00:00Z',
  amount_total: 1000,
  amount_used: 0,
  amount_remain: 1000,
  auto_renew: false,
  auto_renew_failures: 0,
  cancel_at_period_end: false,
  period_days: 30,
  grace_days: 0,
  model_scope: null,
  concurrency_limit: null,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
}

const TOPUP_CONFIG = {
  amount_options: [1000],
  min_topup: 1000,
  max_topup: 500000,
  quota_per_unit: 100,
  credits_ratio: 1,
  enabled_providers: ['stripe', 'alipay'],
  providers: {
    stripe: { publishable_key: 'pk_test_123', min_topup: 1000 },
    alipay: { min_topup: 1000 },
  },
}

const WALLET = {
  balance: 6000,
  held_credits: 0,
  total_consumed: 0,
  total_requests: 0,
  threshold: 1000,
}

function order(overrides: Record<string, unknown> = {}) {
  return {
    id: 41,
    provider: 'alipay',
    amount: 5000,
    currency: 'CNY',
    status: 'pending',
    credited_credits: 0,
    created_at: '2026-10-20T10:00:00Z',
    ...overrides,
  }
}

function subscriptionList(items: unknown[]) {
  return { items, total: items.length, page: 1, page_size: 100 }
}

function useBaseHandlers(options: {
  plans?: unknown[]
  subscriptions?: unknown[]
  wallet?: unknown
  walletStatus?: number
} = {}) {
  server.use(
    http.get(`${ORIGIN}/api/subscription/plans`, () =>
      HttpResponse.json({ items: options.plans ?? [BASIC, LITE, PRO] }),
    ),
    http.get(`${ORIGIN}/api/subscriptions`, () =>
      HttpResponse.json(subscriptionList(options.subscriptions ?? [ACTIVE_SUB])),
    ),
    http.get(`${ORIGIN}/api/billing-preference`, () =>
      HttpResponse.json({ preference: 'subscription_first' }),
    ),
    http.get(`${ORIGIN}/api/payment-methods`, () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(TOPUP_CONFIG)),
  )
  if (options.walletStatus !== undefined) {
    server.use(
      http.get(`${ORIGIN}/api/wallet/summary`, () =>
        HttpResponse.json({ title: 'boom' }, { status: options.walletStatus }),
      ),
    )
  } else {
    server.use(
      http.get(`${ORIGIN}/api/wallet/summary`, () =>
        HttpResponse.json(options.wallet ?? WALLET),
      ),
    )
  }
}

async function renderSection() {
  await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
}

async function openRenew() {
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Renew: Basic' }))
  return user
}

async function openChangeTo(target: RegExp) {
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Change plan: Basic' }),
  )
  await user.click(await screen.findByRole('radio', { name: target }))
  await user.click(screen.getByRole('button', { name: 'Continue' }))
  return user
}

describe('renewal', () => {
  it('renews through Alipay and opens the shared checkout', async () => {
    let body: unknown
    let path: string | null = null
    let key: string | null = null
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/renew`, async ({ request }) => {
        body = await request.json()
        path = new URL(request.url).pathname
        key = request.headers.get('idempotency-key')
        return HttpResponse.json(
          { order: order(), qr_code: 'https://qr.alipay.com/renew' },
          { status: 201 },
        )
      }),
    )
    await renderSection()
    const user = await openRenew()

    // The dialog previews the matched catalog price and the cycle.
    expect(screen.getByText('Renew subscription')).toBeTruthy()
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText('¥50.00')).toBeTruthy()
    expect(
      within(dialog).getByText('Extend Basic by another 30 days.'),
    ).toBeTruthy()

    await user.click(screen.getByRole('radio', { name: 'Alipay' }))
    await user.click(screen.getByRole('button', { name: 'Renew' }))

    expect(
      await screen.findByRole('img', { name: /Alipay payment QR code, order 41/ }),
    ).toBeTruthy()
    expect(body).toEqual({ provider: 'alipay' })
    expect(path).toBe('/api/subscriptions/5/renew')
    expect(key).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('activates a zero-price renewal immediately without a checkout', async () => {
    const FREE = { ...BASIC, price_amount: 0, grant_credits: 0 }
    useBaseHandlers({ plans: [FREE] })
    let body: unknown
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/renew`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          { order: order({ amount: 0, status: 'succeeded' }) },
          { status: 201 },
        )
      }),
    )
    await renderSection()
    const user = await openRenew()

    expect(
      screen.getByText('This plan is free and will be renewed immediately.'),
    ).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Renew' }))

    expect(await screen.findByText('Subscription renewed')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Done' })).toBeTruthy()
    // The contract always requires a provider; zero amount never reaches it.
    expect(body).toEqual({ provider: 'stripe' })
  })

  it.each([
    [409, 'A renewal for this period is already in progress.'],
    [429, 'Too many attempts. Try again in 30s.'],
    [500, 'Could not renew the subscription. Please try again.'],
  ] as const)('renders a %i renewal failure inline', async (status, copy) => {
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/renew`, () =>
        HttpResponse.json(
          { title: 'boom' },
          { status, headers: status === 429 ? { 'Retry-After': '30' } : {} },
        ),
      ),
    )
    await renderSection()
    const user = await openRenew()
    await user.click(screen.getByRole('button', { name: 'Renew' }))

    expect((await screen.findByRole('alert')).textContent).toBe(copy)
    // The dialog stays open so the same intent can be retried.
    expect(screen.getByRole('dialog')).toBeTruthy()
  })

  it('replays the first renewal order when retrying the same intent', async () => {
    const keys: string[] = []
    let attempts = 0
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/renew`, ({ request }) => {
        attempts += 1
        keys.push(request.headers.get('idempotency-key') ?? '')
        if (attempts === 1) {
          return HttpResponse.json({ title: 'boom' }, { status: 500 })
        }
        return HttpResponse.json({ order: order(), qr_code: 'qr' }, { status: 200 })
      }),
    )
    await renderSection()
    const user = await openRenew()
    await user.click(screen.getByRole('button', { name: 'Renew' }))
    await screen.findByRole('alert')

    await user.click(screen.getByRole('button', { name: 'Renew' }))
    await screen.findByRole('img', { name: /payment QR code/ })
    expect(keys).toHaveLength(2)
    expect(keys[1]).toBe(keys[0])
  })

  it('treats a payload-less succeeded replay as renewed', async () => {
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/renew`, () =>
        HttpResponse.json(
          { order: order({ status: 'succeeded' }) },
          { status: 200 },
        ),
      ),
    )
    await renderSection()
    const user = await openRenew()
    await user.click(screen.getByRole('button', { name: 'Renew' }))

    expect(await screen.findByText('Subscription renewed')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Done' })).toBeTruthy()
  })
})

describe('plan change', () => {
  it('requires the confirmation step before writing', async () => {
    let calls = 0
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/change`, () => {
        calls += 1
        return HttpResponse.json({ title: 'unexpected' }, { status: 500 })
      }),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Change plan: Basic' }),
    )
    await user.click(await screen.findByRole('radio', { name: /Lite/ }))

    // Selecting a target alone must not write; only the confirm step may.
    expect(calls).toBe(0)
    await user.click(screen.getByRole('button', { name: 'Continue' }))
    expect(await screen.findByText('Confirm plan change')).toBeTruthy()
    expect(calls).toBe(0)
  })

  it('applies a zero-delta downgrade immediately and shows the result', async () => {
    let body: unknown
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/change`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          {
            order: order({ id: 42, provider: 'stripe', amount: 0, status: 'succeeded' }),
            from_plan_id: 1,
            to_plan_id: 3,
            event_kind: 'downgraded',
            price_delta: 0,
            credits_delta: -500,
            new_end_at: '2099-11-20T00:00:00Z',
          },
          { status: 201 },
        )
      }),
    )
    await renderSection()
    const user = await openChangeTo(/Lite/)

    // The zero-delta path states "no extra charge" and offers no provider UI.
    expect(
      screen.getByText(/No extra charge: the change takes effect immediately/),
    ).toBeTruthy()
    expect(screen.queryByRole('radio', { name: 'Stripe' })).toBeNull()

    await user.click(screen.getByRole('button', { name: 'Confirm change' }))
    expect(await screen.findByText('Plan downgraded')).toBeTruthy()
    expect(screen.getByText('Free')).toBeTruthy()
    expect(screen.getByText('−500')).toBeTruthy()
    expect(body).toEqual({ plan_id: 3, provider: 'stripe' })
  })

  it('opens the payment checkout for an upgrade with a positive delta', async () => {
    let body: unknown
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/change`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          {
            order: order({ id: 43, provider: 'stripe', amount: 3000 }),
            client_secret: 'pi_change_secret',
            from_plan_id: 1,
            to_plan_id: 2,
            event_kind: 'upgraded',
            price_delta: 3000,
            credits_delta: 1000,
            new_end_at: '2099-11-20T00:00:00Z',
          },
          { status: 201 },
        )
      }),
    )
    await renderSection()
    const user = await openChangeTo(/Pro/)

    expect(
      screen.getByText(/prorated from the unused value of your current plan/),
    ).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Confirm change' }))

    expect(await screen.findByTestId('card-element')).toBeTruthy()
    expect(body).toEqual({ plan_id: 2, provider: 'stripe' })
  })

  it('distinguishes a cycle mismatch from other 422s', async () => {
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/change`, () =>
        HttpResponse.json(
          { title: 'subscription_cycle_mismatch' },
          { status: 422 },
        ),
      ),
    )
    await renderSection()
    const user = await openChangeTo(/Pro/)
    await user.click(screen.getByRole('button', { name: 'Confirm change' }))

    expect((await screen.findByRole('alert')).textContent).toBe(
      'The target plan must use the same billing cycle.',
    )
  })

  it.each([
    [409, 'A plan change is already pending for this subscription.'],
    [429, 'Too many attempts. Try again in 30s.'],
    [500, 'Could not change the plan. Please try again.'],
  ] as const)('renders a %i change failure inline', async (status, copy) => {
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/change`, () =>
        HttpResponse.json(
          { title: 'boom' },
          { status, headers: status === 429 ? { 'Retry-After': '30' } : {} },
        ),
      ),
    )
    await renderSection()
    const user = await openChangeTo(/Pro/)
    await user.click(screen.getByRole('button', { name: 'Confirm change' }))

    expect((await screen.findByRole('alert')).textContent).toBe(copy)
    // Still on the confirm step so the user can retry.
    expect(screen.getByText('Confirm plan change')).toBeTruthy()
  })
})

describe('refund request', () => {
  it('submits a reason for an unused active subscription', async () => {
    let body: unknown
    useBaseHandlers()
    server.use(
      http.post(
        `${ORIGIN}/api/subscriptions/5/refund-request`,
        async ({ request }) => {
          body = await request.json()
          return HttpResponse.json({
            order_id: 1,
            requested_at: '2026-10-21T10:00:00Z',
            reason: 'Changed my mind',
          })
        },
      ),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Request refund: Basic' }),
    )
    await user.type(
      screen.getByRole('textbox', { name: 'Reason' }),
      'Changed my mind',
    )
    await user.click(screen.getByRole('button', { name: 'Submit request' }))

    expect(await screen.findByText('Refund requested')).toBeTruthy()
    expect(screen.getByText(/Reason: Changed my mind/)).toBeTruthy()
    expect(body).toEqual({ reason: 'Changed my mind' })
  })

  it('disables the refund action once quota has been used', async () => {
    const used = { ...ACTIVE_SUB, amount_used: 250, amount_remain: 750 }
    useBaseHandlers({ subscriptions: [used] })
    await renderSection()

    const button = (await screen.findByRole('button', {
      name: 'Request refund: Basic',
    })) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    expect(
      screen.getByText('Refunds are only available before any quota is used.'),
    ).toBeTruthy()
  })

  it('disables the refund action after the period ended', async () => {
    const ended = { ...ACTIVE_SUB, end_at: '2020-01-01T00:00:00Z' }
    useBaseHandlers({ subscriptions: [ended] })
    await renderSection()

    const button = (await screen.findByRole('button', {
      name: 'Request refund: Basic',
    })) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    expect(
      screen.getByText(
        'This subscription has ended and can no longer be refunded.',
      ),
    ).toBeTruthy()
  })

  it('hides the refund action for cancelled subscriptions', async () => {
    const cancelled = { ...ACTIVE_SUB, status: 'cancelled' }
    useBaseHandlers({ subscriptions: [cancelled] })
    await renderSection()

    await screen.findByRole('button', { name: 'History: Basic' })
    expect(
      screen.queryByRole('button', { name: 'Request refund: Basic' }),
    ).toBeNull()
  })

  it.each([
    ['subscription_in_use', 422, 'Only active subscriptions with no quota used can be refunded.'],
    [
      'refund_requires_manual_review',
      422,
      'Renewal and plan-change orders must be refunded manually. Please contact support.',
    ],
    ['conflict', 409, 'This order is not refundable in its current state.'],
    ['boom', 500, 'Could not submit the refund request. Please try again.'],
  ] as const)('maps %s (%i) to an inline reason', async (title, status, copy) => {
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/refund-request`, () =>
        HttpResponse.json({ title }, { status }),
      ),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Request refund: Basic' }),
    )
    await user.type(screen.getByRole('textbox', { name: 'Reason' }), 'please')
    await user.click(screen.getByRole('button', { name: 'Submit request' }))

    expect((await screen.findByRole('alert')).textContent).toBe(copy)
  })
})

describe('subscription events', () => {
  const EVENTS = [
    {
      seq: 1,
      kind: 'created',
      detail: { plan_id: 1, order_id: 9, amount_total: 1000, price_paid_amount: 5000 },
      created_at: '2026-10-01T00:00:00Z',
    },
    {
      seq: 2,
      kind: 'upgraded',
      detail: { from_plan_id: 1, to_plan_id: 2, price_delta: 3000, credits_delta: 1000 },
      created_at: '2026-10-02T00:00:00Z',
    },
    {
      seq: 3,
      kind: 'cancelled',
      detail: { immediate: false },
      created_at: '2026-10-03T00:00:00Z',
    },
  ]

  it('renders the read-only timeline with localized kinds and details', async () => {
    useBaseHandlers()
    server.use(
      http.get(`${ORIGIN}/api/subscriptions/5/events`, () =>
        HttpResponse.json({ events: EVENTS }),
      ),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'History: Basic' }))

    expect(await screen.findByText('Subscription history')).toBeTruthy()
    const list = screen.getByRole('list', { name: 'Subscription events' })
    const items = within(list).getAllByRole('listitem')
    expect(items).toHaveLength(3)
    expect(within(list).getByText('Created')).toBeTruthy()
    expect(within(list).getByText('Upgraded')).toBeTruthy()
    expect(within(list).getByText('Cancelled')).toBeTruthy()
    expect(within(list).getByText('¥50.00')).toBeTruthy()
    expect(within(list).getByText('+1,000')).toBeTruthy()
    expect(within(list).getByText('At period end')).toBeTruthy()
    // Read-only: no input surface inside the history dialog.
    expect(screen.queryByRole('textbox')).toBeNull()
  })

  it('shows the empty state when there are no events', async () => {
    useBaseHandlers()
    server.use(
      http.get(`${ORIGIN}/api/subscriptions/5/events`, () =>
        HttpResponse.json({ events: [] }),
      ),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'History: Basic' }))

    expect(await screen.findByText('No events yet.')).toBeTruthy()
  })

  it('retries a failed history read', async () => {
    let calls = 0
    useBaseHandlers()
    server.use(
      http.get(`${ORIGIN}/api/subscriptions/5/events`, () => {
        calls += 1
        return calls === 1
          ? HttpResponse.json({ title: 'boom' }, { status: 500 })
          : HttpResponse.json({ events: EVENTS })
      }),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'History: Basic' }))

    expect(
      await screen.findByText('Could not load the subscription history.'),
    ).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('Created')).toBeTruthy()
  })

  it('pages with the largest seq as after_seq', async () => {
    const first = Array.from({ length: 500 }, (_, index) => ({
      seq: index + 1,
      kind: 'reset',
      detail: { amount_total: 1000 },
      created_at: '2026-10-01T00:00:00Z',
    }))
    const cursors: number[] = []
    useBaseHandlers()
    server.use(
      http.get(`${ORIGIN}/api/subscriptions/5/events`, ({ request }) => {
        const after = Number(
          new URL(request.url).searchParams.get('after_seq') ?? '0',
        )
        cursors.push(after)
        return HttpResponse.json({
          events: after === 0 ? first : [{ ...first[0], seq: 501 }],
        })
      }),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'History: Basic' }))

    await user.click(await screen.findByRole('button', { name: 'Load more' }))
    await waitFor(() => expect(cursors).toEqual([0, 500]))
  })
})

describe('wallet funding for purchases', () => {
  it('offers the wallet when its balance is readable', async () => {
    let body: unknown
    useBaseHandlers({ wallet: { ...WALLET, balance: 6000 } })
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          { order: order({ provider: 'wallet', status: 'succeeded' }) },
          { status: 201 },
        )
      }),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Subscribe Basic' }),
    )

    const wallet = await screen.findByRole('radio', { name: /Wallet balance/ })
    expect(screen.getByText('Balance: 6,000')).toBeTruthy()
    await user.click(wallet)
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    expect(await screen.findByText('Subscription activated')).toBeTruthy()
    expect(body).toEqual({ plan_id: 1, provider: 'wallet' })
  })

  it('surfaces a 402 insufficient-balance response inline', async () => {
    useBaseHandlers()
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, () =>
        HttpResponse.json({ title: 'Insufficient wallet balance' }, { status: 402 }),
      ),
    )
    await renderSection()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Subscribe Basic' }),
    )
    await user.click(await screen.findByRole('radio', { name: /Wallet balance/ }))
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    expect((await screen.findByRole('alert')).textContent).toBe(
      'Your wallet balance is not enough for this plan.',
    )
  })

  it('hides the wallet option when the balance cannot be read', async () => {
    useBaseHandlers({ walletStatus: 500 })
    await renderSection()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Subscribe Basic' }),
    )
    await screen.findByRole('radio', { name: 'Stripe' })

    expect(screen.queryByRole('radio', { name: /Wallet balance/ })).toBeNull()
  })
})

describe('offline and accessibility', () => {
  it('disables write entries while keeping history readable', async () => {
    useBaseHandlers()
    Object.defineProperty(window.navigator, 'onLine', {
      configurable: true,
      get: () => false,
    })
    try {
      await renderSection()
      expect(
        await screen.findByText(
          'You are offline. Changes are disabled until the connection returns.',
        ),
      ).toBeTruthy()
      expect(
        (
          (await screen.findByRole('button', {
            name: 'Renew: Basic',
          })) as HTMLButtonElement
        ).disabled,
      ).toBe(true)
      expect(
        (screen.getByRole('button', {
          name: 'Change plan: Basic',
        }) as HTMLButtonElement).disabled,
      ).toBe(true)
      expect(
        (screen.getByRole('button', {
          name: 'Request refund: Basic',
        }) as HTMLButtonElement).disabled,
      ).toBe(true)
      // History is a read: it stays reachable offline.
      expect(
        (screen.getByRole('button', { name: 'History: Basic' }) as HTMLButtonElement)
          .disabled,
      ).toBe(false)
    } finally {
      Object.defineProperty(window.navigator, 'onLine', {
        configurable: true,
        get: () => true,
      })
      window.dispatchEvent(new Event('online'))
    }
  })

  it('labels the renew dialog and its inputs for assistive tech', async () => {
    useBaseHandlers()
    await renderSection()
    const user = await openRenew()

    const dialog = screen.getByRole('dialog')
    expect(dialog.getAttribute('aria-labelledby')).toBeTruthy()
    const radios = within(dialog).getAllByRole('radio')
    expect(radios).toHaveLength(2)
    expect(within(dialog).getByText('Renew subscription')).toBeTruthy()
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })
})
