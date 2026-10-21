import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
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
    // 默认 topup Tab 的子查询（本测试只验证 Tab 选择，形状保持最小）。
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
  it('defaults to the top-up tab when no tab is given', async () => {
    installSession()
    renderAt('/wallet')
    expect(
      await screen.findByRole('tab', { name: 'Top-up', selected: true }),
    ).toBeTruthy()
  })

  it('opens the transactions tab from ?tab=transactions', async () => {
    installSession()
    renderAt('/wallet?tab=transactions')
    expect(
      await screen.findByRole('tab', { name: 'Transactions', selected: true }),
    ).toBeTruthy()
    expect(await screen.findByText('No transactions yet')).toBeTruthy()
  })

  it('falls back to top-up for an unknown tab value', async () => {
    installSession()
    renderAt('/wallet?tab=bogus')
    expect(
      await screen.findByRole('tab', { name: 'Top-up', selected: true }),
    ).toBeTruthy()
  })
})
