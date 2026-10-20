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

  it('shows the empty state when the server returns no plans', async () => {
    useBaseHandlers({ plans: [], subscriptions: [] })
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

    expect(await screen.findByText('No plans available')).toBeTruthy()
    expect(screen.getByText('You have no subscriptions yet')).toBeTruthy()
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
        idempotencyKey = request.headers.get('idempotency-key')
        return HttpResponse.json({ id: 5, cancel_at_period_end: true })
      }),
    )
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])
    const user = userEvent.setup()

    const toggle = await screen.findByRole('switch', {
      name: 'Cancel at period end',
    })
    expect(toggle.getAttribute('aria-checked')).toBe('false')
    await user.click(toggle)

    await waitFor(() => {
      expect(screen.getByRole('switch', { name: 'Cancel at period end' }).getAttribute('aria-checked')).toBe('true')
    })
    expect(cancelCalls).toBe(1)
    expect(method).toBe('POST')
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('renders unlimited quota without a progress bar', async () => {
    useBaseHandlers({
      plans: [],
      subscriptions: [ACTIVE_SUB, UNLIMITED_SUB],
    })
    await renderWithProviders(<SubscriptionSection />, ['common', 'account'])

    expect(await screen.findByText('Unlimited')).toBeTruthy()
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
            name: 'Cancel at period end',
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
})
