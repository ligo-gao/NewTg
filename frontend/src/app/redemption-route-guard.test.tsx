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
      <I18nProvider modules={['common', 'admin', 'public']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return { ...result, router }
}

function installSession(role: 'admin' | 'user', permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'alice', role, status: 'enabled', permissions }),
    ),
    // ConsoleLayout 的定价能力探测；空列表即「能力未知」，不影响本页。
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    http.get(`${ORIGIN}/api/redemption-codes`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

describe('/redemption-codes guard (web-00 §4, §9.3)', () => {
  it('redirects an admin without redemption:manage to /403', async () => {
    installSession('admin', [])
    const { router } = renderAt('/redemption-codes')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
    expect(router.state.location.search).toMatchObject({
      redirect: '/redemption-codes?p=1',
    })
  })

  it('redirects a non-admin session to /403', async () => {
    installSession('user', ['redemption:manage'])
    const { router } = renderAt('/redemption-codes')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
  })

  it('renders the page for an admin holding redemption:manage', async () => {
    installSession('admin', ['redemption:manage'])
    const { router } = renderAt('/redemption-codes')

    expect(
      await screen.findByRole('heading', { name: 'Redemption Codes' }),
    ).toBeTruthy()
    expect(router.state.location.pathname).toBe('/redemption-codes')
    expect(await screen.findByText('No redemption codes yet.')).toBeTruthy()
  })
})
