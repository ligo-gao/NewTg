import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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
  customer_type: 'general',
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

/**
 * 弹窗读取调用者的生效权限来决定是否渲染「立即取消」（R-47：users:update）。
 * 缺省给 ['users:read']，保持只读用例的语义；写用例显式给 users:update。
 */
function installMe(permissions: string[] = ['users:read']) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
  )
}

describe('UserSubscriptionsDialog', () => {
  it('lists instances with quota progress and a masked order id, read-only', async () => {
    installMe(['users:read'])
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
      within(dialog).getByText(/Immediate cancellation keeps the instance usable/),
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

    // 只有 users:read：不渲染取消按钮，只有关闭。
    const buttons = within(dialog).getAllByRole('button')
    expect(buttons.map((button) => button.textContent)).toEqual(['Close'])
  })

  it('renders a 403 explanation instead of an empty list', async () => {
    installMe(['users:read'])
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
    installMe(['users:read'])
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
    installMe(['users:read'])
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
    installMe(['users:read'])
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

  it('immediately cancels after an explicit confirmation that explains the grace period', async () => {
    installMe(['users:read', 'users:update'])
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.get(`${ORIGIN}/api/users/1/subscriptions`, () =>
        HttpResponse.json({
          items: [subscription({ grace_days: 5 }), subscription({ id: 11, status: 'expired' })],
          total: 2,
          page: 1,
          page_size: 100,
        }),
      ),
      http.post(
        `${ORIGIN}/api/users/1/subscriptions/10/cancel`,
        async ({ request }) => {
          calls.push({ path: new URL(request.url).pathname, body: await request.json() })
          return HttpResponse.json(subscription({ grace_days: 5 }))
        },
      ),
    )

    await renderWithProviders(
      <UserSubscriptionsDialog user={USER} onClose={() => undefined} />,
      ['common', 'admin'],
    )

    const dialog = await screen.findByRole('dialog')
    // 终态行不渲染取消按钮：只有 active 行有一个「Cancel immediately」。
    const cancelButtons = await within(dialog).findAllByRole('button', {
      name: 'Cancel immediately',
    })
    expect(cancelButtons).toHaveLength(1)

    await userEvent.click(cancelButtons[0]!)
    const confirm = await screen.findByRole('alertdialog')
    // R-48 文案：必须说明宽限期仍可用，否则会被误解为立刻停用。
    expect(
      within(confirm).getByText(/stays usable during its grace period of 5 days/),
    ).toBeTruthy()
    await userEvent.click(within(confirm).getByRole('button', { name: 'Cancel now' }))

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]?.path).toBe('/api/users/1/subscriptions/10/cancel')
    expect(calls[0]?.body).toEqual({ immediate: true })
  })
})
