import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { SubscriptionPlansPage } from './subscription-plans-page'

type SubscriptionPlan = components['schemas']['SubscriptionPlan']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function planRow(overrides: Partial<SubscriptionPlan> = {}): SubscriptionPlan {
  return {
    id: 1,
    plan_key: 'pro-monthly',
    title: 'Pro Monthly',
    description: 'monthly plan',
    period_days: 30,
    price_amount: 5000,
    currency: 'CNY',
    grant_credits: 0,
    reset_on_renew: true,
    allow_wallet_overflow: true,
    model_scope: null,
    concurrency_limit: null,
    grace_days: 0,
    status: 'enabled',
    sort_order: 1,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

function installMe(permissions: string[] = ['settings:manage']) {
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

function installPlans(
  respond: (params: URLSearchParams) => Response | Promise<Response> = () =>
    HttpResponse.json({
      items: [planRow()],
      total: 1,
      page: 1,
      page_size: 20,
    }),
) {
  server.use(
    http.get(`${ORIGIN}/api/subscription-plans`, ({ request }) =>
      respond(new URL(request.url).searchParams),
    ),
  )
}

describe('SubscriptionPlansPage', () => {
  it('renders the plan table with CNY price, unlimited quota and scope', async () => {
    installMe()
    installPlans()

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])

    expect(await screen.findByText('pro-monthly')).toBeTruthy()
    expect(screen.getByText('Pro Monthly')).toBeTruthy()
    expect(screen.getByText('30 days')).toBeTruthy()
    expect(screen.getByText('¥50.00')).toBeTruthy()
    expect(screen.getByText('Unlimited')).toBeTruthy()
    expect(screen.getByText('All models')).toBeTruthy()
    expect(screen.getAllByText('Enabled').length).toBeGreaterThan(0)
  })

  it('sends keyword/status filters and resets to the first page', async () => {
    const recorded: URLSearchParams[] = []
    installMe()
    installPlans((params) => {
      recorded.push(new URLSearchParams(params))
      return HttpResponse.json({
        items: [planRow({ status: 'disabled' })],
        total: 45,
        page: Number(params.get('p') ?? '1'),
        page_size: 20,
      })
    })

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    expect(await screen.findByText('pro-monthly')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() => expect(recorded.at(-1)?.get('p')).toBe('2'))

    await user.selectOptions(screen.getByLabelText('Status'), 'disabled')
    await user.type(screen.getByLabelText('Keyword (plan key or title)'), 'pro')
    await user.click(screen.getByRole('button', { name: 'Search' }))

    await waitFor(() => {
      const last = recorded.at(-1)
      expect(last?.get('keyword')).toBe('pro')
      expect(last?.get('status')).toBe('disabled')
      expect(last?.get('p')).toBe('1')
    })
  })

  it('shows the empty state and a filtered-empty variant', async () => {
    installMe()
    installPlans(() =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    expect(await screen.findByText('No subscription plans yet.')).toBeTruthy()
    await user.type(screen.getByLabelText('Keyword (plan key or title)'), 'x')
    await user.click(screen.getByRole('button', { name: 'Search' }))
    expect(await screen.findByText('No plans match these filters.')).toBeTruthy()
  })

  it('shows a Retry-After countdown on a 429 list response', async () => {
    installMe()
    installPlans(() =>
      HttpResponse.json(
        {
          type: 'https://newtg.dev/problems/rate_limited',
          title: 'Too many requests',
        },
        { status: 429, headers: { 'Retry-After': '5' } },
      ),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    expect(
      await screen.findByText('Too many requests. Try again in 5 s.'),
    ).toBeTruthy()
  })

  it('renders the 403 state without settings:manage and does not query plans', async () => {
    let planRequests = 0
    installMe([])
    server.use(
      http.get(`${ORIGIN}/api/subscription-plans`, () => {
        planRequests += 1
        return HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 })
      }),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    expect(
      await screen.findByText('Your account lacks the settings:manage permission.'),
    ).toBeTruthy()
    // 能力层在查询前就短路：无权限时不得把请求发到服务端。
    expect(planRequests).toBe(0)
  })

  it('confirms disable and calls the idempotent endpoint', async () => {
    let disabled = 0
    installMe()
    installPlans()
    server.use(
      http.post(`${ORIGIN}/api/subscription-plans/1/disable`, () => {
        disabled += 1
        return HttpResponse.json(planRow({ status: 'disabled' }))
      }),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Disable' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText('Disable plan?')).toBeTruthy()
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))

    await waitFor(() => expect(disabled).toBe(1))
    await waitFor(() =>
      expect(
        useToastStore.getState().toasts.some((toast) => toast.message === 'Plan disabled'),
      ).toBe(true),
    )
  })

  it('resets quota with the advance checkbox and reports the affected count', async () => {
    let body: unknown
    installMe()
    installPlans()
    server.use(
      http.post(`${ORIGIN}/api/subscription-plans/1/reset-quota`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ affected: 3 })
      }),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Reset quota' }))
    const dialog = screen.getByRole('dialog')
    await user.click(
      within(dialog).getByRole('checkbox', {
        name: /Also advance the next reset by one period/,
      }),
    )
    await user.click(within(dialog).getByRole('button', { name: 'Reset quota' }))

    await waitFor(() => expect(body).toEqual({ advance_next_reset: true }))
    await waitFor(() =>
      expect(
        useToastStore.getState().toasts.some((toast) => toast.message === '3 subscriptions reset.'),
      ).toBe(true),
    )
  })

  it('shows instances (total minus orders) when deleting a referenced plan (409)', async () => {
    installMe()
    installPlans()
    server.use(
      http.delete(`${ORIGIN}/api/subscription-plans/1`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/plan_referenced',
            title: 'plan_referenced',
            // reference_count = instances + unclosed orders, so 3 total with
            // 1 order means 2 instances (the backend detail says the same).
            reference_count: 3,
            order_reference_count: 1,
          },
          { status: 409 },
        ),
      ),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    const dialog = screen.getByRole('alertdialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    expect(
      await within(dialog).findByText(
        /still referenced by 2 subscription instances and 1 unclosed orders/,
      ),
    ).toBeTruthy()
    // 引用未解除时对话框必须保持打开，管理员才能读到计数与处理提示。
    expect(screen.getByRole('alertdialog')).toBeTruthy()
  })

  it('falls back to generic copy when the 409 lacks the order count', async () => {
    installMe()
    installPlans()
    server.use(
      http.delete(`${ORIGIN}/api/subscription-plans/1`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/plan_referenced',
            title: 'plan_referenced',
            reference_count: 2,
          },
          { status: 409 },
        ),
      ),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    const dialog = screen.getByRole('alertdialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    expect(
      await within(dialog).findByText(
        'This plan is still referenced. Disable it or wait for its orders to finish before deleting.',
      ),
    ).toBeTruthy()
  })

  it('keeps a failed enable/disable inside the confirm dialog with a Close action', async () => {
    installMe()
    installPlans()
    server.use(
      http.post(`${ORIGIN}/api/subscription-plans/1/disable`, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/internal', title: 'Internal error' },
          { status: 500 },
        ),
      ),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Disable' }))
    const dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))

    expect(
      await within(dialog).findByText('The action failed. Try again.'),
    ).toBeTruthy()
    // 失败后仍停留在确认框内，且次要按钮改为「关闭」而不是「取消」。
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: 'Close' })).toBeTruthy()
  })

  it('falls back to a page when the current page is out of range', async () => {
    const pages: string[] = []
    installMe()
    installPlans((params) => {
      const p = params.get('p') ?? '1'
      pages.push(p)
      if (p === '1') {
        return HttpResponse.json({
          items: [planRow()],
          total: 45,
          page: 1,
          page_size: 20,
        })
      }
      // The second page shrank (e.g. concurrent deletion), so page 2 no
      // longer exists and the page must clamp back to page 1.
      return HttpResponse.json({
        items: [],
        total: 12,
        page: Number(p),
        page_size: 20,
      })
    })

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Next' }))

    await waitFor(() => expect(pages).toContain('2'))
    await waitFor(() => expect(pages.at(-1)).toBe('1'))
  })

  it('shows offline-specific copy when the list read fails offline', async () => {
    installMe()
    server.use(
      http.get(`${ORIGIN}/api/subscription-plans`, () => HttpResponse.error()),
    )

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])

    expect(
      await screen.findByText(
        'Could not load data while offline. Reconnect and retry.',
      ),
    ).toBeTruthy()
  })

  it('opens the edit drawer with a locked plan key', async () => {
    installMe()
    installPlans()

    await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByText('pro-monthly')
    await user.click(screen.getByRole('button', { name: 'Edit' }))

    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText('Edit subscription plan')).toBeTruthy()
    const key = within(dialog).getByLabelText('Plan Key') as HTMLInputElement
    expect(key.value).toBe('pro-monthly')
    expect(key.disabled).toBe(true)
  })
})

function setOnline(online: boolean): void {
  Object.defineProperty(globalThis.navigator, 'onLine', {
    configurable: true,
    get: () => online,
  })
  window.dispatchEvent(new Event(online ? 'online' : 'offline'))
}

describe('SubscriptionPlansPage offline state', () => {
  it('disables write actions while offline', async () => {
    installMe()
    installPlans()
    setOnline(false)

    try {
      await renderWithProviders(<SubscriptionPlansPage />, ['common', 'admin'])
      await screen.findByText('pro-monthly')

      expect(screen.getByRole('button', { name: 'New plan' })).toHaveProperty(
        'disabled',
        true,
      )
      expect(screen.getByRole('button', { name: 'Edit' })).toHaveProperty(
        'disabled',
        true,
      )
      expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty(
        'disabled',
        true,
      )
      expect(
        screen.getByText(/You are offline\. Write actions are disabled/),
      ).toBeTruthy()
    } finally {
      setOnline(true)
    }
  })
})
