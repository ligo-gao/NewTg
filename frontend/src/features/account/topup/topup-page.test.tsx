import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { TopupPage } from './topup-page'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const CONFIG = {
  amount_options: [1000, 5000, 10000],
  min_topup: 1000,
  max_topup: 500000,
  quota_per_unit: 100,
  enabled_providers: ['stripe', 'alipay'],
  providers: { stripe: { publishable_key: 'pk_test_123' }, alipay: {} },
}

function summary(balance: number) {
  return {
    balance,
    held_credits: 0,
    total_consumed: 0,
    total_requests: 0,
    threshold: 1000,
    health: balance === 0 ? 'depleted' : balance < 1000 ? 'low' : 'healthy',
    group: 'default',
  }
}

describe('TopupPage', () => {
  it('renders amount chips, limits and providers from GET /api/topup/config', async () => {
    server.use(
      http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(CONFIG)),
      http.get(`${ORIGIN}/api/wallet/summary`, () => HttpResponse.json(summary(0))),
    )
    await renderWithProviders(<TopupPage />, ['common', 'account'])
    const user = userEvent.setup()

    expect(await screen.findByRole('radio', { name: '¥10' })).toBeTruthy()
    expect(screen.getByRole('radio', { name: '¥50' })).toBeTruthy()
    expect(screen.getByRole('radio', { name: '¥100' })).toBeTruthy()
    expect(screen.getByRole('radio', { name: /^Stripe/ })).toBeTruthy()
    expect(screen.getByRole('radio', { name: /^Alipay/ })).toBeTruthy()

    // Default selection: the first preset at or above the minimum.
    expect(screen.getByTestId('summary-pay').textContent).toBe('¥10.00')
    expect(screen.getByTestId('summary-credits').textContent).toBe('1,000')

    await user.click(screen.getByRole('radio', { name: 'Custom Amount' }))
    const input = screen.getByLabelText('Custom Amount')

    await user.type(input, '5')
    expect(await screen.findByText('Minimum top-up is ¥10.00')).toBeTruthy()
    expect(screen.getByRole('button', { name: /^Pay/ }).hasAttribute('disabled')).toBe(
      true,
    )

    await user.clear(input)
    await user.type(input, '6000')
    expect(await screen.findByText('Maximum top-up is ¥5,000.00')).toBeTruthy()

    await user.clear(input)
    await user.type(input, '100')
    expect(screen.getByTestId('summary-pay').textContent).toBe('¥100.00')
    expect(screen.getByTestId('summary-credits').textContent).toBe('10,000')
    expect(
      screen.getByRole('button', { name: /^Pay ¥100\.00/ }),
    ).toBeTruthy()
  })

  it('creates the order with the selected amount when the confirm dialog is submitted', async () => {
    let body: unknown
    let idempotencyKey: string | null = null
    server.use(
      http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(CONFIG)),
      http.get(`${ORIGIN}/api/wallet/summary`, () => HttpResponse.json(summary(0))),
      http.post(`${ORIGIN}/api/orders`, async ({ request }) => {
        body = await request.json()
        idempotencyKey = request.headers.get('idempotency-key')
        return HttpResponse.json(
          {
            order: {
              id: 7,
              provider: 'alipay',
              amount: 5000,
              currency: 'CNY',
              status: 'pending',
              credited_credits: 0,
              created_at: '2026-10-19T10:00:00Z',
            },
            qr_code: 'https://qr.alipay.com/example',
          },
          { status: 201 },
        )
      }),
      http.get(`${ORIGIN}/api/orders/7`, () =>
        HttpResponse.json({
          id: 7,
          user_id: 1,
          provider: 'alipay',
          amount: 5000,
          currency: 'CNY',
          status: 'pending',
          credited_credits: 0,
          manual_completed: false,
          created_at: '2026-10-19T10:00:00Z',
          updated_at: '2026-10-19T10:00:00Z',
          expires_at: '2026-10-19T10:30:00Z',
        }),
      ),
    )
    await renderWithProviders(<TopupPage />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('radio', { name: '¥50' }))
    await user.click(screen.getByRole('radio', { name: 'Alipay' }))
    await user.click(screen.getByRole('button', { name: /^Pay ¥50\.00/ }))
    expect(await screen.findByRole('dialog')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))

    await waitFor(() => {
      expect(body).toEqual({ amount: 5000, currency: 'CNY', provider: 'alipay' })
    })
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('renders a payload-less idempotent replay from the fetched order status', async () => {
    let detailCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(CONFIG)),
      http.get(`${ORIGIN}/api/wallet/summary`, () => HttpResponse.json(summary(0))),
      http.post(`${ORIGIN}/api/orders`, () =>
        // Replay: the contract returns the first order without a QR payload.
        HttpResponse.json(
          {
            order: {
              id: 7,
              provider: 'alipay',
              amount: 5000,
              currency: 'CNY',
              status: 'pending',
              credited_credits: 0,
              created_at: '2026-10-19T10:00:00Z',
            },
          },
          { status: 200 },
        ),
      ),
      http.get(`${ORIGIN}/api/orders/7`, () => {
        detailCalls += 1
        return HttpResponse.json({
          id: 7,
          user_id: 1,
          provider: 'alipay',
          amount: 5000,
          currency: 'CNY',
          status: 'pending',
          credited_credits: 0,
          manual_completed: false,
          created_at: '2026-10-19T10:00:00Z',
          updated_at: '2026-10-19T10:00:00Z',
          expires_at: '2026-10-19T10:30:00Z',
        })
      }),
    )
    await renderWithProviders(<TopupPage />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('radio', { name: 'Alipay' }))
    await user.click(screen.getByRole('button', { name: /^Pay ¥10\.00/ }))
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))

    expect(await screen.findByText('Payment already created')).toBeTruthy()
    expect(
      screen.getByText(/Open Order History to check its status/),
    ).toBeTruthy()
    await waitFor(() => {
      expect(detailCalls).toBeGreaterThanOrEqual(1)
    })
  })

  it('seeds the balance from /api/wallet/summary and refetches after success', async () => {
    let summaryCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(CONFIG)),
      http.get(`${ORIGIN}/api/wallet/summary`, () => {
        summaryCalls += 1
        return HttpResponse.json(summary(summaryCalls === 1 ? 123450 : 128450))
      }),
      http.post(`${ORIGIN}/api/orders`, () =>
        HttpResponse.json(
          {
            order: {
              id: 7,
              provider: 'alipay',
              amount: 5000,
              currency: 'CNY',
              status: 'succeeded',
              credited_credits: 5000,
              created_at: '2026-10-19T10:00:00Z',
            },
            qr_code: 'https://qr.alipay.com/example',
          },
          { status: 201 },
        ),
      ),
    )
    await renderWithProviders(<TopupPage />, ['common', 'account'])
    const user = userEvent.setup()

    await waitFor(() => {
      expect(screen.getByTestId('balance-value').textContent).toBe('123,450')
    })

    await user.click(await screen.findByRole('radio', { name: 'Alipay' }))
    await user.click(screen.getByRole('button', { name: /^Pay ¥10\.00/ }))
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))

    expect(await screen.findByText('Payment successful')).toBeTruthy()
    await waitFor(() => {
      expect(summaryCalls).toBe(2)
      expect(screen.getByTestId('balance-value').textContent).toBe('128,450')
    })
  })
})
