import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  it,
} from 'vitest'

import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'

const ORIGIN = 'http://localhost:3000'

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
}

const SUMMARY = {
  cost_total: 0,
  request_count: 0,
  success_rate: 0,
  prompt_tokens_total: 0,
  completion_tokens_total: 0,
  pre_deducted_total: 0,
  refunded_total: 0,
}

const server = setupServer()

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  window.scrollTo = () => {}
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

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
    http.get(`${ORIGIN}/api/usage-logs`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    http.get(`${ORIGIN}/api/audit-logs`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    http.get(`${ORIGIN}/api/usage-stats/summary`, () =>
      HttpResponse.json(SUMMARY),
    ),
    http.get(`${ORIGIN}/api/usage-stats/by-model`, () =>
      HttpResponse.json([]),
    ),
    http.get(`${ORIGIN}/api/usage-stats/timeseries`, () =>
      HttpResponse.json([]),
    ),
  )
}

describe('usage/audit route IA (web-00 §4.3)', () => {
  it('redirects /usage-logs to /dashboard/usage-logs and keeps the search', async () => {
    installSession()
    const { router } = renderAt('/usage-logs?range=7d&status=error')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/dashboard/usage-logs')
    })
    expect(router.state.location.search).toMatchObject({
      range: '7d',
      status: 'error',
    })
    expect(
      await screen.findByRole('heading', { name: 'Usage Logs' }),
    ).toBeTruthy()
  })

  it('redirects /audit-logs to /dashboard/audit-logs and keeps the search', async () => {
    installSession()
    const { router } = renderAt('/audit-logs?category=login&p=2')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/dashboard/audit-logs')
    })
    expect(router.state.location.search).toMatchObject({
      category: 'login',
      p: 2,
    })
  })

  it('falls back to the logs tab for an unknown ?tab value', async () => {
    installSession()
    renderAt('/dashboard/usage-logs?tab=performance')

    expect(
      await screen.findByRole('tab', { name: 'Logs', selected: true }),
    ).toBeTruthy()
    expect(
      screen.queryByRole('tab', { name: 'Stats', selected: true }),
    ).toBeNull()
  })

  it('opens the stats tab from ?tab=stats', async () => {
    installSession()
    renderAt('/dashboard/usage-logs?tab=stats')

    expect(
      await screen.findByRole('tab', { name: 'Stats', selected: true }),
    ).toBeTruthy()
    expect(await screen.findByText('Total spent')).toBeTruthy()
  })
})
