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

function installSession(role: 'admin' | 'user', permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'alice', role, status: 'enabled', permissions }),
    ),
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
      <I18nProvider modules={['common']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return router
}

describe('/admin/models permission guard', () => {
  it('redirects an admin without channels:read to /403', async () => {
    installSession('admin', ['channels:manage'])
    const router = renderAt('/admin/models?tab=vendors')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
    expect(router.state.location.search).toMatchObject({
      redirect: expect.stringContaining('/admin/models?tab=vendors'),
    })
  })

  it('does not redirect an admin holding channels:read', async () => {
    installSession('admin', ['channels:read'])
    server.use(
      http.get(`${ORIGIN}/api/models`, () =>
        HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
      ),
      http.get(`${ORIGIN}/api/models/missing`, () =>
        HttpResponse.json({ items: [], skipped_channels: 0, total: 0 }),
      ),
      http.get(`${ORIGIN}/api/vendors/search`, () => HttpResponse.json({ items: [] })),
      // ConsoleHeader 的定价能力探测。
      http.get(`${ORIGIN}/api/users`, () =>
        HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
      ),
    )
    const router = renderAt('/admin/models')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/admin/models')
    })
  })
})
