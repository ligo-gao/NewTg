import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, waitFor } from '@testing-library/react'
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
    http.get(`${ORIGIN}/api/notifications/unread-count`, () =>
      HttpResponse.json({ unread: 0 }),
    ),
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
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
    // ConsoleHeader's pricing capability probe.
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
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

describe('/admin/upstream-reconciliation permission guard', () => {
  it('redirects an admin without settings:manage to /403', async () => {
    installSession(['channels:read'])
    const router = renderAt('/admin/upstream-reconciliation')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
    expect(router.state.location.search).toMatchObject({
      // `validateSearch` materializes the default tab before `beforeLoad`, so
      // the origin is preserved with its normalized `?tab=` search string.
      redirect: expect.stringContaining('/admin/upstream-reconciliation'),
    })
  })

  it('does not redirect an admin holding settings:manage', async () => {
    installSession(['settings:manage'])
    const router = renderAt('/admin/upstream-reconciliation')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe(
        '/admin/upstream-reconciliation',
      )
    })
  })
})
