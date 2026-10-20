import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { UserSubscriptionsDialog } from './user-subscriptions-dialog'

type AdminUser = components['schemas']['AdminUser']
type AdminSubscription = components['schemas']['AdminSubscription']

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const USER: AdminUser = {
  id: 1,
  username: 'alice',
  display_name: null,
  email: null,
  role: 'user',
  status: 'enabled',
  balance: 0,
  group: 'default',
  admin_permissions: null,
  totp_enabled: false,
  must_change_password: false,
  token_count: 0,
  created_at: '2026-10-01T10:00:00Z',
  updated_at: '2026-10-01T10:00:00Z',
}

function subscription(overrides: Partial<AdminSubscription> = {}): AdminSubscription {
  return {
    id: 10,
    user_id: 1,
    plan_id: 1,
    plan_title_snapshot: 'Pro Monthly',
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
    allow_wallet_overflow: true,
    order_id: 42,
    currency: 'CNY',
    price_paid_amount: 5000,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

function installSubscriptions(items: AdminSubscription[]) {
  server.use(
    http.get(`${ORIGIN}/api/users/1/subscriptions`, () =>
      HttpResponse.json({
        items,
        total: items.length,
        page: 1,
        page_size: 100,
      }),
    ),
  )
}

describe('UserSubscriptionsDialog', () => {
  it('lists instances with quota progress and a masked order id, read-only', async () => {
    installSubscriptions([
      subscription(),
      subscription({
        id: 11,
        status: 'expired',
        plan_title_snapshot: 'Starter',
        amount_total: 0,
        amount_used: 900,
        amount_remain: undefined,
        order_id: null,
        auto_renew: true,
        cancel_at_period_end: true,
      }),
    ])

    await renderWithProviders(
      <UserSubscriptionsDialog user={USER} onClose={() => undefined} />,
      ['common', 'admin'],
    )

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Subscriptions of @alice')).toBeTruthy()
    expect(
      within(dialog).getByText(/Read-only\. Immediate cancellation is not available/),
    ).toBeTruthy()

    expect(await within(dialog).findByText('Pro Monthly')).toBeTruthy()
    expect(within(dialog).getByText('250 / 1,000')).toBeTruthy()
    expect(within(dialog).getByText('#42')).toBeTruthy()
    expect(within(dialog).getByText('Active')).toBeTruthy()

    // 起止区间、自动续费与周期末取消状态都是管理侧需要的事实。
    expect(within(dialog).getByText('Starts')).toBeTruthy()
    expect(within(dialog).getByText('Ends')).toBeTruthy()
    expect(within(dialog).getByText('Auto-renew')).toBeTruthy()
    expect(within(dialog).getByText('Cancel at period end')).toBeTruthy()
    expect(within(dialog).getAllByText('Off')).toHaveLength(1)
    expect(within(dialog).getAllByText('No')).toHaveLength(1)
    expect(within(dialog).getByText('On')).toBeTruthy()
    expect(within(dialog).getByText('Scheduled')).toBeTruthy()

    expect(within(dialog).getByText('Starter')).toBeTruthy()
    // amount_total = 0 是无限额度：不渲染分母，也不渲染进度条。
    expect(within(dialog).getAllByText('Unlimited')).toHaveLength(1)
    expect(within(dialog).getByText('Expired')).toBeTruthy()

    // 本批只读：没有任何取消/写操作按钮，只有关闭。
    const buttons = within(dialog).getAllByRole('button')
    expect(buttons.map((button) => button.textContent)).toEqual(['Close'])
  })

  it('renders a 403 explanation instead of an empty list', async () => {
    server.use(
      http.get(`${ORIGIN}/api/users/1/subscriptions`, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/forbidden', title: 'Forbidden' },
          { status: 403 },
        ),
      ),
    )

    await renderWithProviders(
      <UserSubscriptionsDialog user={USER} onClose={() => undefined} />,
      ['common', 'admin'],
    )

    expect(
      await screen.findByText('Your account lacks the users:read permission.'),
    ).toBeTruthy()
    expect(screen.queryByText('This user has no subscription instances.')).toBeNull()
  })

  it('shows an empty state when the user has no instances', async () => {
    installSubscriptions([])

    await renderWithProviders(
      <UserSubscriptionsDialog user={USER} onClose={() => undefined} />,
      ['common', 'admin'],
    )

    expect(
      await screen.findByText('This user has no subscription instances.'),
    ).toBeTruthy()
  })

  it('warns when more instances exist than the 100-row cap shows', async () => {
    server.use(
      http.get(`${ORIGIN}/api/users/1/subscriptions`, () =>
        HttpResponse.json({
          items: [subscription()],
          total: 150,
          page: 1,
          page_size: 100,
        }),
      ),
    )

    await renderWithProviders(
      <UserSubscriptionsDialog user={USER} onClose={() => undefined} />,
      ['common', 'admin'],
    )

    expect(
      await screen.findByText(
        'Showing the 1 most recent subscriptions. Older instances are not displayed.',
      ),
    ).toBeTruthy()
  })

  it('shows offline-specific copy when the read fails offline', async () => {
    server.use(
      http.get(`${ORIGIN}/api/users/1/subscriptions`, () => HttpResponse.error()),
    )

    await renderWithProviders(
      <UserSubscriptionsDialog user={USER} onClose={() => undefined} />,
      ['common', 'admin'],
    )

    expect(
      await screen.findByText(
        'Could not load data while offline. Reconnect and retry.',
      ),
    ).toBeTruthy()
  })
})
