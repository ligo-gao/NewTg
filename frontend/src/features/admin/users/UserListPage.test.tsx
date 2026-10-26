import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor, screen } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { UserListPage } from './UserListPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function adminUser(id: number, username: string) {
  return {
    id,
    username,
    display_name: null,
    email: null,
    role: 'user' as const,
    status: 'enabled' as const,
    balance: 1000,
    group: 'default',
    customer_type: 'general',
    admin_permissions: null,
    totp_enabled: false,
    must_change_password: false,
    token_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
  }
}

function installHandlers(record: (params: URLSearchParams) => void) {
  server.use(
    http.get(`${ORIGIN}/api/groups`, () =>
      HttpResponse.json({
        items: [
          {
            id: 1,
            name: 'default',
            description: null,
            ratio: 100,
            user_selectable: true,
            is_system: true,
            status: 1,
            users_count: 0,
            pricing_overrides_count: 0,
            channels_count: 0,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
        ],
        total: 1,
        page: 1,
        page_size: 100,
      }),
    ),
    http.get(`${ORIGIN}/api/users`, ({ request }) => {
      const params = new URL(request.url).searchParams
      record(params)
      const page = Number(params.get('p') ?? '1')
      return HttpResponse.json({
        items: [adminUser(page, page === 1 ? 'alice' : 'bob-page-two')],
        total: 45,
        page,
        page_size: 20,
      })
    }),
  )
}

describe('UserListPage', () => {
  it('requests the next offset page through the pagination control', async () => {
    let lastParams: URLSearchParams | undefined
    installHandlers((params) => {
      lastParams = params
    })

    const { getByRole, findByText } = await renderWithProviders(<UserListPage />, [
      'common',
      'admin',
    ])
    const user = userEvent.setup()

    expect(await findByText('@alice')).toBeTruthy()

    await user.click(getByRole('button', { name: 'Next' }))

    expect(await findByText('@bob-page-two')).toBeTruthy()
    await waitFor(() => {
      expect(lastParams?.get('p')).toBe('2')
    })
  })

  it('applies filters and resets to the first page', async () => {
    const recorded: URLSearchParams[] = []
    installHandlers((params) => {
      recorded.push(new URLSearchParams(params))
    })

    const { getByLabelText, getByRole, findByText } = await renderWithProviders(
      <UserListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('@alice')).toBeTruthy()

    await user.selectOptions(getByLabelText('Status'), 'disabled')
    await user.type(getByLabelText('Keyword'), 'zhang')
    await user.click(getByRole('button', { name: 'Search' }))

    await waitFor(() => {
      const applied = recorded.at(-1)
      expect(applied?.get('status')).toBe('disabled')
      expect(applied?.get('keyword')).toBe('zhang')
      expect(applied?.get('p')).toBe('1')
    })
  })

  it('opens the read-only subscriptions dialog from the row action (billing-08 §16.4)', async () => {
    installHandlers(() => undefined)
    server.use(
      http.get(`${ORIGIN}/api/users/1/subscriptions`, () =>
        HttpResponse.json({
          items: [
            {
              id: 10,
              user_id: 1,
              plan_id: 1,
              plan_title_snapshot: 'Pro Monthly',
              status: 'active',
              start_at: '2026-10-01T00:00:00Z',
              end_at: '2026-10-31T00:00:00Z',
              next_reset_at: null,
              amount_total: 1000,
              amount_used: 10,
              amount_remain: 990,
              auto_renew: false,
              cancel_at_period_end: false,
              period_days: 30,
              grace_days: 0,
              model_scope: null,
              concurrency_limit: null,
              allow_wallet_overflow: true,
              order_id: 42,
              currency: 'CNY',
              price_paid_amount: 5000,
              created_at: '2026-10-01T00:00:00Z',
              updated_at: '2026-10-01T00:00:00Z',
            },
          ],
          total: 1,
          page: 1,
          page_size: 100,
        }),
      ),
    )

    const { getByRole, findByText } = await renderWithProviders(<UserListPage />, [
      'common',
      'admin',
    ])
    const user = userEvent.setup()

    expect(await findByText('@alice')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Manage subscriptions' }))

    expect(await screen.findByText('Subscriptions of @alice')).toBeTruthy()
    expect(await screen.findByText('Pro Monthly')).toBeTruthy()
    expect(screen.getByText('#42')).toBeTruthy()
  })
})
