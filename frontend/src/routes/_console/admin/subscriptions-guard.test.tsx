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

const server = setupServer()

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  window.scrollTo = () => {}
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installSession(permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/notifications`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 }),
    ),
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'alice',
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
    // ConsoleHeader 的定价能力探测。
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    http.get(`${ORIGIN}/api/announcements/active`, () =>
      HttpResponse.json({ items: [] }),
    ),
  )
}

function installPlans(onRequest?: () => void) {
  server.use(
    http.get(`${ORIGIN}/api/subscription-plans`, () => {
      onRequest?.()
      return HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 })
    }),
  )
}

function renderAt(path: string) {
  const queryClient = createQueryClient()
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
    scrollRestoration: false,
  })
  render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider modules={['common', 'admin']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return router
}

describe('/admin/subscriptions permission guard', () => {
  it('redirects an admin without settings:manage to /403 and keeps the target', async () => {
    installSession(['users:read'])
    const router = renderAt('/admin/subscriptions')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
    expect(router.state.location.search).toMatchObject({
      redirect: '/admin/subscriptions',
    })
  })

  it('does not redirect an admin holding settings:manage', async () => {
    let planRequests = 0
    installSession(['settings:manage'])
    installPlans(() => {
      planRequests += 1
    })
    const router = renderAt('/admin/subscriptions')

    // The memory history already starts at /admin/subscriptions, so asserting
    // the pathname alone would pass before an erroneous redirect could fire.
    // Wait for evidence the guarded page actually rendered its data instead.
    await waitFor(() => {
      expect(planRequests).toBeGreaterThan(0)
    })
    expect(await screen.findByText('Subscription Plans')).toBeTruthy()
    expect(router.state.location.pathname).toBe('/admin/subscriptions')
  })
})
