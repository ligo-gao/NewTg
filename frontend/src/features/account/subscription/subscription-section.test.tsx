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
  // jsdom in this setup does not guarantee `navigator.onLine === true`; the
  // section treats any non-`true` value as offline and disables writes.
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
  server.listen({ onUnhandledRequest: 'error' })
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const PLAN = {
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

const FREE_PLAN = {
  ...PLAN,
  id: 2,
  plan_key: 'free',
  title: 'Starter',
  description: null,
  price_amount: 0,
  grant_credits: 0,
  model_scope: ['gpt-4o'],
  sort_order: 2,
}

const ACTIVE_SUB = {
  id: 5,
  plan_title_snapshot: 'Basic',
  status: 'active',
  start_at: '2026-10-01T00:00:00Z',
  end_at: '2026-10-31T00:00:00Z',
  next_reset_at: '2026-10-31T00:00:00Z',
  amount_total: 1000,
  amount_used: 250,
  amount_remain: 750,
  auto_renew: false,
  cancel_at_period_end: false,
  period_days: 30,
  grace_days: 0,
  model_scope: null,
  concurrency_limit: null,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
}

const UNLIMITED_SUB = {
  ...ACTIVE_SUB,
  id: 6,
  plan_title_snapshot: 'Meterless',
  amount_total: 0,
  amount_used: 0,
  amount_remain: undefined,
  next_reset_at: null,
  status: 'active',
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

function subscriptionList(items: unknown[]) {
  return { items, total: items.length, page: 1, page_size: 100 }
}

function useBaseHandlers(options: {
  plans?: unknown[]
  subscriptions?: unknown[]
  preference?: string
}) {
  server.use(
    http.get(`${ORIGIN}/api/subscription/plans`, () =>
      HttpResponse.json({ items: options.plans ?? [PLAN, FREE_PLAN] }),
    ),
    http.get(`${ORIGIN}/api/subscriptions`, () =>
      HttpResponse.json(subscriptionList(options.subscriptions ?? [])),
    ),
    http.get(`${ORIGIN}/api/billing-preference`, () =>
      HttpResponse.json({ preference: options.preference ?? 'subscription_first' }),
    ),
    http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(TOPUP_CONFIG)),
  )
}

describe('SubscriptionSection', () => {
  it('renders enabled plans with price, period, quota and model scope', async () => {
    useBaseHandlers({})
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

    expect(await screen.findByText('Basic')).toBeTruthy()
    const basic = screen.getByText('Basic').closest('article')
    expect(basic).not.toBeNull()
    const basicCard = within(basic as HTMLElement)
    expect(basicCard.getByText('¥50.00')).toBeTruthy()
    expect(basicCard.getByText('1,000')).toBeTruthy()
    expect(basicCard.getByText('All models')).toBeTruthy()
    expect(basicCard.getByText(/30 days/)).toBeTruthy()

    const free = screen.getByText('Starter').closest('article')
    const freeCard = within(free as HTMLElement)
    // 0 credits means unlimited, never "0".
    expect(freeCard.getByText('Free')).toBeTruthy()
    expect(freeCard.getByText('Unlimited')).toBeTruthy()
    expect(freeCard.getByText('gpt-4o')).toBeTruthy()
  })

  it('renders no subscription area at all when no plan is enabled', async () => {
    useBaseHandlers({ plans: [], subscriptions: [] })
    // 裁决 A3a-M2 (billing-08 §16.3 / modules-04:623): no enabled plan means
    // the whole area is omitted, not replaced by an empty state. This also
    // hides My Subscriptions and the billing preference.
    const { container } = await renderWithProviders(<SubscriptionSection />, [
      'common',
      'account',
    ])

    await waitFor(() => {
      expect(container.firstChild).toBeNull()
    })
    expect(screen.queryByText('Subscription Plans')).toBeNull()
    expect(screen.queryByText('My Subscriptions')).toBeNull()
    expect(screen.queryByText('Billing Preference')).toBeNull()
    // The empty-state copy is retained in the dictionaries but never rendered.
    expect(screen.queryByText('No plans available')).toBeNull()
  })

  it('offers a retry when the plan list fails', async () => {
    let calls = 0
    useBaseHandlers({})
    server.use(
      http.get(`${ORIGIN}/api/subscription/plans`, () => {
        calls += 1
        return calls === 1
          ? HttpResponse.json({ title: 'boom' }, { status: 500 })
          : HttpResponse.json({ items: [PLAN] })
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

    expect(await screen.findByText('Could not load subscription plans.')).toBeTruthy()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('Basic')).toBeTruthy()
  })

  it('purchases with Alipay and shows the QR checkout', async () => {
    let body: unknown
    let idempotencyKey: string | null = null
    useBaseHandlers({})
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, async ({ request }) => {
        body = await request.json()
        idempotencyKey = request.headers.get('idempotency-key')
        return HttpResponse.json(
          {
            order: {
              id: 11,
              provider: 'alipay',
              amount: 5000,
              currency: 'CNY',
              status: 'pending',
              credited_credits: 0,
              created_at: '2026-10-20T10:00:00Z',
            },
            qr_code: 'https://qr.alipay.com/subscription',
          },
          { status: 201 },
        )
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    expect(await screen.findByRole('dialog')).toBeTruthy()
    expect(screen.getByText('Confirm Subscription')).toBeTruthy()
    await user.click(screen.getByRole('radio', { name: 'Alipay' }))
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    expect(
      await screen.findByRole('img', {
        name: /Alipay payment QR code, order 11/,
      }),
    ).toBeTruthy()
    expect(screen.getByText('Subscription Checkout')).toBeTruthy()
    expect(body).toEqual({ plan_id: 1, provider: 'alipay' })
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('purchases with Stripe and enters the existing card checkout', async () => {
    let body: unknown
    useBaseHandlers({})
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          {
            order: {
              id: 12,
              provider: 'stripe',
              amount: 5000,
              currency: 'CNY',
              status: 'pending',
              credited_credits: 0,
              created_at: '2026-10-20T10:00:00Z',
            },
            client_secret: 'pi_123_secret_456',
          },
          { status: 201 },
        )
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    await user.click(await screen.findByRole('radio', { name: 'Stripe' }))
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    expect(await screen.findByTestId('card-element')).toBeTruthy()
    expect(screen.getByText('Subscription Checkout')).toBeTruthy()
    expect(body).toEqual({ plan_id: 1, provider: 'stripe' })
  })

  it('activates a zero-price plan immediately without a checkout', async () => {
    let body: unknown
    useBaseHandlers({ plans: [FREE_PLAN] })
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          {
            order: {
              id: 13,
              provider: 'stripe',
              amount: 0,
              currency: 'CNY',
              status: 'succeeded',
              credited_credits: 0,
              created_at: '2026-10-20T10:00:00Z',
            },
          },
          { status: 201 },
        )
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Starter' }))
    expect(
      screen.getByText('This plan is free and will be activated immediately.'),
    ).toBeTruthy()
    // A free plan has no payment-method choice.
    expect(screen.queryByRole('radio', { name: 'Stripe' })).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    expect(await screen.findByText('Subscription activated')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Done' })).toBeTruthy()
    // The contract still requires a provider; the first enabled one is sent and
    // the server never calls the channel for a zero amount.
    expect(body).toEqual({ plan_id: 2, provider: 'stripe' })
  })

  it('renders the rate-limit message inline on 429', async () => {
    useBaseHandlers({})
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, () =>
        HttpResponse.json(
          { title: 'too many' },
          { status: 429, headers: { 'Retry-After': '30' } },
        ),
      ),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    // Wait for the provider list so the Confirm button is enabled.
    await screen.findByRole('radio', { name: 'Stripe' })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe('Too many purchase attempts. Try again in 30s.')
    expect(screen.getByRole('dialog')).toBeTruthy()
    // The button is usable again; the same intent key is retained internally.
    expect(
      (screen.getByRole('button', { name: 'Confirm' }) as HTMLButtonElement).disabled,
    ).toBe(false)
  })

  it('cancels an active subscription at the end of the period', async () => {
    let cancelCalls = 0
    let idempotencyKey: string | null = null
    let method: string | null = null
    let path: string | null = null
    useBaseHandlers({ subscriptions: [ACTIVE_SUB] })
    server.use(
      http.get(`${ORIGIN}/api/subscriptions`, () =>
        HttpResponse.json(
          subscriptionList([
            { ...ACTIVE_SUB, cancel_at_period_end: cancelCalls > 0 },
          ]),
        ),
      ),
      http.post(`${ORIGIN}/api/subscriptions/5/cancel`, ({ request }) => {
        cancelCalls += 1
        method = request.method
        path = new URL(request.url).pathname
        idempotencyKey = request.headers.get('idempotency-key')
        return HttpResponse.json({ ...ACTIVE_SUB, cancel_at_period_end: true })
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    const toggle = await screen.findByRole('switch', {
      name: 'Cancel at period end: Basic',
    })
    expect(toggle.getAttribute('aria-checked')).toBe('false')
    await user.click(toggle)

    await waitFor(() => {
      expect(
        screen
          .getByRole('switch', { name: 'Cancel at period end: Basic' })
          .getAttribute('aria-checked'),
      ).toBe('true')
    })
    expect(cancelCalls).toBe(1)
    expect(method).toBe('POST')
    // The generated client owns the path template; prove the id was expanded.
    expect(path).toBe('/api/subscriptions/5/cancel')
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('surfaces a 409 cancel response inline and leaves the switch unchanged', async () => {
    useBaseHandlers({ subscriptions: [ACTIVE_SUB] })
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/cancel`, () =>
        HttpResponse.json({ title: 'ended' }, { status: 409 }),
      ),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    const toggle = await screen.findByRole('switch', {
      name: 'Cancel at period end: Basic',
    })
    await user.click(toggle)

    // 409 = the instance already expired/cancelled: the write did not change
    // the server field, so the switch must stay off and an inline error shows.
    expect(
      await screen.findByText('Could not update the subscription.'),
    ).toBeTruthy()
    expect(
      screen
        .getByRole('switch', { name: 'Cancel at period end: Basic' })
        .getAttribute('aria-checked'),
    ).toBe('false')
  })

  it('renders unlimited quota without a progress bar', async () => {
    useBaseHandlers({
      subscriptions: [ACTIVE_SUB, UNLIMITED_SUB],
    })
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

    const unlimitedRow = (await screen.findByText('Meterless')).closest('article')
    expect(unlimitedRow).not.toBeNull()
    expect(within(unlimitedRow as HTMLElement).getByText('Unlimited')).toBeTruthy()
    // Only the finite instance has a denominator; the unlimited one must not
    // render a bar at all.
    const bars = screen.getAllByRole('progressbar')
    expect(bars).toHaveLength(1)
    expect(bars[0]?.getAttribute('aria-valuenow')).toBe('250')
    expect(bars[0]?.getAttribute('aria-valuemax')).toBe('1000')
  })

  it('saves the billing preference immediately and rolls back a failed save', async () => {
    let serverPreference = 'subscription_first'
    let failNextPut = false
    useBaseHandlers({})
    server.use(
      http.get(`${ORIGIN}/api/billing-preference`, () =>
        HttpResponse.json({ preference: serverPreference }),
      ),
      http.put(`${ORIGIN}/api/billing-preference`, async ({ request }) => {
        if (failNextPut) return HttpResponse.json({ title: 'boom' }, { status: 500 })
        const body = (await request.json()) as { preference: string }
        serverPreference = body.preference
        return HttpResponse.json({ preference: serverPreference })
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    const wallet = await screen.findByRole('radio', { name: /Wallet first/ })
    expect(wallet.getAttribute('aria-checked')).toBe('false')
    await user.click(wallet)
    await waitFor(() => {
      expect(
        screen.getByRole('radio', { name: /Wallet first/ }).getAttribute('aria-checked'),
      ).toBe('true')
    })
    expect(serverPreference).toBe('wallet_first')

    failNextPut = true
    await user.click(screen.getByRole('radio', { name: /Subscription only/ }))
    await waitFor(() => {
      expect(
        screen.getByRole('radio', { name: /Wallet first/ }).getAttribute('aria-checked'),
      ).toBe('true')
    })
    expect(
      screen.getByRole('radio', { name: /Subscription only/ }).getAttribute('aria-checked'),
    ).toBe('false')
  })

  it('disables writes while offline', async () => {
    useBaseHandlers({ subscriptions: [ACTIVE_SUB] })
    // jsdom exposes `onLine` as an inherited getter; define an own property so
    // the hook's initial read and its event re-sync both see the offline value.
    Object.defineProperty(window.navigator, 'onLine', {
      configurable: true,
      get: () => false,
    })
    try {
      await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

      expect(
        await screen.findByText(
          'You are offline. Changes are disabled until the connection returns.',
        ),
      ).toBeTruthy()
      expect(
        (
          (await screen.findByRole('button', {
            name: 'Subscribe Basic',
          })) as HTMLButtonElement
        ).disabled,
      ).toBe(true)
      expect(
        (
          (await screen.findByRole('switch', {
            name: 'Cancel at period end: Basic',
          })) as HTMLButtonElement
        ).disabled,
      ).toBe(true)
      expect(
        (
          screen.getAllByRole('radio')[0] as HTMLButtonElement
        ).disabled,
      ).toBe(true)
    } finally {
      Object.defineProperty(window.navigator, 'onLine', {
        configurable: true,
        get: () => true,
      })
    }
  })

  it.each([
    [409, 'This plan is no longer available, or the request was already used.'],
    [422, 'This plan cannot be purchased right now.'],
    [503, 'Payment is temporarily unavailable. Please try again later.'],
  ] as const)(
    'renders a %i purchase failure inline next to the action',
    async (status, copy) => {
      useBaseHandlers({})
      server.use(
        http.post(`${ORIGIN}/api/subscription/purchase`, () =>
          HttpResponse.json({ title: 'boom' }, { status }),
        ),
      )
      await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
      const user = userEvent.setup()

      await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
      await screen.findByRole('radio', { name: 'Stripe' })
      await user.click(screen.getByRole('button', { name: 'Confirm' }))

      expect((await screen.findByRole('alert')).textContent).toBe(copy)
      // The intent survives a failure: the dialog stays open for a retry.
      expect(screen.getByRole('dialog')).toBeTruthy()
    },
  )

  it('renders an offline purchase failure inline', async () => {
    useBaseHandlers({})
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, () =>
        HttpResponse.error(),
      ),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    await screen.findByRole('radio', { name: 'Stripe' })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))

    expect((await screen.findByRole('alert')).textContent).toBe(
      'You are offline. Reconnect to complete the purchase.',
    )
  })

  it('reuses the same Idempotency-Key when retrying a failed purchase', async () => {
    const keys: string[] = []
    let attempts = 0
    useBaseHandlers({})
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, ({ request }) => {
        attempts += 1
        keys.push(request.headers.get('idempotency-key') ?? '')
        if (attempts === 1) {
          return HttpResponse.json({ title: 'boom' }, { status: 500 })
        }
        return HttpResponse.json(
          {
            order: {
              id: 21,
              provider: 'stripe',
              amount: 5000,
              currency: 'CNY',
              status: 'pending',
              credited_credits: 0,
              created_at: '2026-10-20T10:00:00Z',
            },
            client_secret: 'pi_retry_secret',
          },
          { status: 201 },
        )
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    await screen.findByRole('radio', { name: 'Stripe' })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))
    expect((await screen.findByRole('alert')).textContent).toBe(
      'Could not create the subscription order. Please try again.',
    )

    await user.click(screen.getByRole('button', { name: 'Confirm' }))
    expect(await screen.findByTestId('card-element')).toBeTruthy()
    expect(keys).toHaveLength(2)
    // A retry of the *same* intent must replay the first order, not create a
    // second one: the key is minted on open and only replaced on close/retry.
    expect(keys[1]).toBe(keys[0])
  })

  it('mints a different Idempotency-Key when the same plan is reopened', async () => {
    const keys: string[] = []
    useBaseHandlers({})
    server.use(
      http.post(`${ORIGIN}/api/subscription/purchase`, ({ request }) => {
        keys.push(request.headers.get('idempotency-key') ?? '')
        return HttpResponse.json({ title: 'boom' }, { status: 500 })
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    // First intent: fail, then cancel out of the dialog.
    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    await screen.findByRole('radio', { name: 'Stripe' })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))
    await screen.findByRole('alert')
    const firstDialog = screen.getByRole('dialog')
    await user.click(within(firstDialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    // Second intent on the same plan: state and key must be fresh.
    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    await screen.findByRole('radio', { name: 'Stripe' })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(keys).toHaveLength(2))
    expect(keys[1]).not.toBe(keys[0])
  })

  it('shows next reset, quota progress width and an accessible value text', async () => {
    useBaseHandlers({ subscriptions: [ACTIVE_SUB] })
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

    expect(await screen.findByText(/Next reset/)).toBeTruthy()
    const bar = await screen.findByRole('progressbar')
    // 250 used / 1000 total.
    expect(bar.getAttribute('aria-valuenow')).toBe('250')
    expect(bar.getAttribute('aria-valuemax')).toBe('1000')
    expect(bar.getAttribute('aria-valuetext')).toBe('250 of 1,000 used')
    const fill = bar.firstElementChild as HTMLElement | null
    expect(fill).not.toBeNull()
    expect(fill?.style.width).toBe('25%')
  })

  it('disables every cancel switch and shows a notice while one is in flight', async () => {
    let release: () => void = () => undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    useBaseHandlers({
      subscriptions: [ACTIVE_SUB, { ...ACTIVE_SUB, id: 7, plan_title_snapshot: 'Pro' }],
    })
    server.use(
      http.post(`${ORIGIN}/api/subscriptions/5/cancel`, async () => {
        await gate
        return HttpResponse.json({ ...ACTIVE_SUB, cancel_at_period_end: true })
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    const basic = await screen.findByRole('switch', {
      name: 'Cancel at period end: Basic',
    })
    const pro = screen.getByRole('switch', { name: 'Cancel at period end: Pro' })
    await user.click(basic)

    expect(await screen.findByText('Cancelling...')).toBeTruthy()
    expect(basic.getAttribute('aria-busy')).toBe('true')
    // The section serializes cancellations: the other row is disabled too.
    await waitFor(() => {
      expect((pro as HTMLButtonElement).disabled).toBe(true)
    })

    release()
    await waitFor(() => expect(screen.queryByText('Cancelling...')).toBeNull())
  })

  it('disables Confirm in an open purchase dialog when the connection drops', async () => {
    useBaseHandlers({})
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Subscribe Basic' }))
    await screen.findByRole('radio', { name: 'Stripe' })
    expect(
      (screen.getByRole('button', { name: 'Confirm' }) as HTMLButtonElement).disabled,
    ).toBe(false)

    Object.defineProperty(window.navigator, 'onLine', {
      configurable: true,
      get: () => false,
    })
    try {
      window.dispatchEvent(new Event('offline'))
      await waitFor(() => {
        expect(
          (screen.getByRole('button', { name: 'Confirm' }) as HTMLButtonElement)
            .disabled,
        ).toBe(true)
      })
    } finally {
      Object.defineProperty(window.navigator, 'onLine', {
        configurable: true,
        get: () => true,
      })
    }
  })
})
