import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render } from '@testing-library/react'
import { waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'

const ORIGIN = 'http://localhost:3000'

const PUBLIC_STATUS = {
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
  // jsdom implements no layout; the router's scroll manager calls `scrollTo`.
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
    // jsdom has no layout, and the default restoration calls `scrollTo`.
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

function guestHandlers() {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json(
        { type: 'unauthorized', title: 'Unauthorized' },
        { status: 401 },
      ),
    ),
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
  )
}

describe('route guard chain (web-00 §9.3)', () => {
  it('redirects an unauthenticated deep link to sign-in with return_to', async () => {
    guestHandlers()
    const { router } = renderAt('/profile')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/sign-in')
    })
    expect(router.state.location.search).toMatchObject({ redirect: '/profile' })
  })

  it('lands a signed-in non-admin on /403 for an admin route', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ username: 'alice', role: 'user', status: 'enabled' }),
      ),
    )

    const { router } = renderAt('/admin/users')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
  })
})
