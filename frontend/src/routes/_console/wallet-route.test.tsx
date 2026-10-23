import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  window.scrollTo = () => {}
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const PUBLIC_STATUS = {
  system_initialized: true,
  auth: {
    signup_enabled: true,
    password_login_enabled: true,
    email_verification_required: false,
    turnstile: { enabled: false, site_key: '' },
  },
  password_login_encryption_enabled: false,
  languages: ['en'],
  default_language: 'en',
  credit_display_symbol: 'C',
}

function renderAt(path: string) {
  const queryClient = createQueryClient()
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
    scrollRestoration: false,
  })
  const result = render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider modules={['common']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return { ...result, router }
}

function installSession() {
  server.use(
    http.get(`${ORIGIN}/api/notifications`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 }),
    ),
    http.get(`${ORIGIN}/api/notifications/unread-count`, () =>
      HttpResponse.json({ unread: 0 }),
    ),
    http.get(`${ORIGIN}/api/announcements/active`, () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'alice',
        role: 'user',
        status: 'enabled',
        permissions: [],
      }),
    ),
    http.get(`${ORIGIN}/api/wallet/transactions`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    // 默认 overview Tab 的子查询（本测试只验证 Tab 选择，形状保持最小）。
    http.get(`${ORIGIN}/api/topup/config`, () =>
      HttpResponse.json({
        amount_options: [1000],
        min_topup: 1000,
        max_topup: 500000,
        quota_per_unit: 100,
        credits_ratio: 1,
        enabled_providers: [],
        providers: {},
      }),
    ),
    http.get(`${ORIGIN}/api/wallet/summary`, () =>
      HttpResponse.json({
        balance: 0,
        held_credits: 0,
        total_consumed: 0,
        total_requests: 0,
        threshold: 1000,
        health: 'depleted',
        group: 'default',
      }),
    ),
    http.get(`${ORIGIN}/api/usage-stats/summary`, () =>
      HttpResponse.json({
        cost_total: 0,
        request_count: 0,
        success_rate: 0,
        prompt_tokens_total: 0,
        completion_tokens_total: 0,
        pre_deducted_total: 0,
        refunded_total: 0,
      }),
    ),
    http.get(`${ORIGIN}/api/usage-stats/timeseries`, () =>
      HttpResponse.json([]),
    ),
    http.get(`${ORIGIN}/api/subscription/plans`, () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get(`${ORIGIN}/api/subscriptions`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 }),
    ),
    http.get(`${ORIGIN}/api/payment-methods`, () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get(`${ORIGIN}/api/billing-preference`, () =>
      HttpResponse.json({ preference: 'subscription_first' }),
    ),
  )
}

describe('/wallet route search (R-M3-01)', () => {
  it('defaults to the overview tab when no tab is given', async () => {
    installSession()
    const { router } = renderAt('/wallet')
    expect(
      await screen.findByRole('tab', { name: 'Overview', selected: true }),
    ).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).not.toContain('tab=')
    })
  })

  it('opens the top-up tab from ?tab=topup', async () => {
    installSession()
    const { router } = renderAt('/wallet?tab=topup')
    expect(
      await screen.findByRole('tab', { name: 'Top-up', selected: true }),
    ).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).toContain('tab=topup')
    })
    expect(router.state.location.search).toMatchObject({ tab: 'topup' })
  })

  it('opens the subscription tab from ?tab=subscription', async () => {
    installSession()
    const { router } = renderAt('/wallet?tab=subscription')
    expect(
      await screen.findByRole('tab', { name: 'Subscription', selected: true }),
    ).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).toContain('tab=subscription')
    })
  })

  it('opens the transactions tab from ?tab=transactions', async () => {
    installSession()
    const { router } = renderAt('/wallet?tab=transactions')
    expect(
      await screen.findByRole('tab', { name: 'Transactions', selected: true }),
    ).toBeTruthy()
    expect(await screen.findByText('No transactions yet')).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).toContain('tab=transactions')
    })
  })

  it('canonicalises a redundant ?tab=overview to a tab-less URL', async () => {
    installSession()
    const { router } = renderAt('/wallet?tab=overview')
    expect(
      await screen.findByRole('tab', { name: 'Overview', selected: true }),
    ).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).not.toContain('tab=')
    })
  })

  it('falls back to the overview and strips an unknown tab from the URL', async () => {
    installSession()
    const { router } = renderAt('/wallet?tab=bogus')
    expect(
      await screen.findByRole('tab', { name: 'Overview', selected: true }),
    ).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).not.toContain('tab=')
    })
    expect(router.state.location.search).not.toHaveProperty('tab')
  })

  it('preserves p/page_size while canonicalising an invalid tab', async () => {
    installSession()
    const { router } = renderAt('/wallet?tab=bogus&p=2&page_size=10')
    expect(
      await screen.findByRole('tab', { name: 'Overview', selected: true }),
    ).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.searchStr).not.toContain('tab=')
    })
    expect(router.state.location.search).toMatchObject({
      p: 2,
      page_size: 10,
    })
  })
})
