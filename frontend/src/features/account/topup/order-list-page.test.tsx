import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { OrderListPage } from './order-list-page'
import type { OrderStatus } from './types'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const STATUSES: OrderStatus[] = [
  'pending',
  'succeeded',
  'failed',
  'expired',
  'cancelled',
]

function order(id: number, status: OrderStatus) {
  return {
    id,
    user_id: 1,
    provider: id % 2 === 0 ? 'stripe' : 'alipay',
    amount: 5000,
    currency: 'CNY',
    status,
    credited_credits: status === 'succeeded' ? 5000 : 0,
    manual_completed: false,
    paid_at: status === 'succeeded' ? '2026-10-19T10:05:00Z' : null,
    created_at: '2026-10-19T10:00:00Z',
    updated_at: '2026-10-19T10:00:00Z',
    expires_at: '2026-10-19T10:30:00Z',
  }
}

describe('OrderListPage', () => {
  it('renders all five order states with their badges', async () => {
    const items = STATUSES.map((status, index) => order(index + 1, status))
    server.use(
      http.get(`${ORIGIN}/api/orders`, () =>
        HttpResponse.json({ items, total: items.length, page: 1, page_size: 20 }),
      ),
    )

    await renderWithProviders(<OrderListPage />, ['common', 'account'])

    const rows = await screen.findAllByText(/^#\d$/)
    expect(rows).toHaveLength(5)

    const variants = screen
      .getAllByTestId('status-badge')
      .map((badge) => badge.getAttribute('data-variant'))
    expect(variants).toEqual([
      'warning',
      'success',
      'danger',
      'neutral',
      'neutral-outline',
    ])
  })

  it('opens the detail sheet from a row', async () => {
    const pending = order(1, 'pending')
    server.use(
      http.get(`${ORIGIN}/api/orders`, () =>
        HttpResponse.json({ items: [pending], total: 1, page: 1, page_size: 20 }),
      ),
      http.get(`${ORIGIN}/api/orders/1`, () => HttpResponse.json(pending)),
    )

    await renderWithProviders(<OrderListPage />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByText('#1'))

    const sheet = await screen.findByRole('dialog')
    expect(within(sheet).getByText('Order Details')).toBeTruthy()
    await waitFor(() => {
      expect(within(sheet).getByRole('button', { name: 'I have paid' })).toBeTruthy()
    })
  })
})
