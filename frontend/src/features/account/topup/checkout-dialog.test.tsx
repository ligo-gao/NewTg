import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { CheckoutDialog } from './checkout-dialog'
import type { OrderView, TopupOrderView } from './types'

vi.mock('@stripe/stripe-js', () => ({
  loadStripe: vi.fn(() => Promise.resolve(null)),
}))
vi.mock('@stripe/react-stripe-js', () => ({
  Elements: ({ children }: { children: React.ReactNode }) => children,
  CardElement: () => <div data-testid="card-element" />,
  useStripe: () => null,
  useElements: () => null,
}))

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const PENDING_ORDER: TopupOrderView = {
  id: 7,
  provider: 'alipay',
  amount: 5000,
  currency: 'CNY',
  status: 'pending',
  credited_credits: 0,
  created_at: '2026-10-19T10:00:00Z',
}

const SUCCEEDED_ORDER: OrderView = {
  id: 7,
  user_id: 1,
  provider: 'alipay',
  amount: 5000,
  currency: 'CNY',
  status: 'succeeded',
  credited_credits: 5000,
  manual_completed: false,
  refunded_amount: 0,
  refunded_at: null,
  refund_reason: null,
  refund_requested_at: null,
  paid_at: '2026-10-19T10:05:00Z',
  created_at: '2026-10-19T10:00:00Z',
  updated_at: '2026-10-19T10:05:00Z',
  expires_at: '2026-10-19T10:30:00Z',
}

describe('CheckoutDialog', () => {
  it('shows the Alipay QR code with an accessible alternative', async () => {
    await renderWithProviders(
      <CheckoutDialog
        open
        order={PENDING_ORDER}
        qrCode="https://qr.alipay.com/example"
        onOpenChange={() => undefined}
        onSucceeded={() => undefined}
        pollIntervalMs={60_000}
      />,
      ['common', 'account'],
    )

    expect(await screen.findByText('Scan with Alipay')).toBeTruthy()
    const qr = screen.getByRole('img', {
      name: /Alipay payment QR code, order 7, amount ¥50\.00/,
    })
    expect(qr).toBeTruthy()
    expect(screen.getByText('#7')).toBeTruthy()
    expect(screen.getByText('Time remaining')).toBeTruthy()
  })

  it('polls the order and switches to the success view on succeeded', async () => {
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/orders/7`, () => {
        calls += 1
        return HttpResponse.json(calls < 2 ? PENDING_ORDER : SUCCEEDED_ORDER)
      }),
    )
    const onSucceeded = vi.fn()

    await renderWithProviders(
      <CheckoutDialog
        open
        order={PENDING_ORDER}
        qrCode="https://qr.alipay.com/example"
        onOpenChange={() => undefined}
        onSucceeded={onSucceeded}
        pollIntervalMs={20}
        pollMaxMs={60}
      />,
      ['common', 'account'],
    )

    expect(await screen.findByText('Payment successful')).toBeTruthy()
    await waitFor(() => {
      expect(onSucceeded).toHaveBeenCalledWith({
        amount: 5000,
        credited_credits: 5000,
      })
    })
    // Balance/credits in the success view come from the server response.
    expect(screen.getByText('5,000')).toBeTruthy()
  })

  it('explains a payload-less pending replay instead of a dead checkout', async () => {
    server.use(
      http.get(`${ORIGIN}/api/orders/7`, () => HttpResponse.json(PENDING_ORDER)),
    )

    await renderWithProviders(
      <CheckoutDialog
        open
        order={PENDING_ORDER}
        replayOrder={{ ...SUCCEEDED_ORDER, status: 'pending', credited_credits: 0 }}
        onOpenChange={() => undefined}
        onSucceeded={() => undefined}
        pollIntervalMs={20}
      />,
      ['common', 'account'],
    )

    expect(await screen.findByText('Payment already created')).toBeTruthy()
    expect(
      screen.getByText(/Open Order History to check its status/),
    ).toBeTruthy()
  })

  it.each([
    ['failed', 'The payment failed. Try again or view the order in Order History.'],
    ['expired', 'This order expired. Start a new payment.'],
    ['cancelled', 'This order was cancelled. Start a new payment.'],
  ] as const)(
    'renders the %s terminal state and stops polling',
    async (status, body) => {
      let calls = 0
      server.use(
        http.get(`${ORIGIN}/api/orders/7`, () => {
          calls += 1
          return HttpResponse.json({ ...SUCCEEDED_ORDER, status })
        }),
      )

      await renderWithProviders(
        <CheckoutDialog
          open
          order={PENDING_ORDER}
          replayOrder={{ ...SUCCEEDED_ORDER, status }}
          onOpenChange={() => undefined}
          onSucceeded={() => undefined}
          pollIntervalMs={10}
          pollMaxMs={20}
        />,
        ['common', 'account'],
      )

      expect(await screen.findByText(body)).toBeTruthy()
      // Terminal means terminal: no 10s polling until the 5-minute ceiling.
      await new Promise((resolve) => setTimeout(resolve, 50))
      expect(calls).toBe(0)
    },
  )
})
