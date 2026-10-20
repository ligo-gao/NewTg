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
    // ConsoleHeader 的定价能力探测。
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

function installPerformance() {
  server.use(
    http.get(`${ORIGIN}/api/performance/stats`, () =>
      HttpResponse.json({
        instance_id: 'gateway-1',
        cache_stats: {
          enabled: true,
          file_count: 0,
          total_bytes: 0,
          last_cleared_at: null,
        },
        memory_stats: {
          alloc_bytes: 0,
          sys_bytes: 0,
          num_gc: 0,
          num_goroutine: 0,
        },
        disk_cache_info: {
          path: '/var/cache/newtg',
          file_count: 0,
          total_bytes: 0,
          last_modified: null,
        },
        disk_space_info: { total_bytes: 0, free_bytes: 0, used_bytes: 0 },
        config: {
          enabled: true,
          path: '/var/cache/newtg',
          max_mb: 1024,
          threshold_mb: 256,
        },
      }),
    ),
    http.get(`${ORIGIN}/api/performance/logs`, () =>
      HttpResponse.json({
        enabled: false,
        log_dir: '',
        file_count: 0,
        total_size: 0,
        oldest_time: null,
        newest_time: null,
        files: [],
      }),
    ),
    http.get(`${ORIGIN}/api/performance/recent-operations`, () =>
      HttpResponse.json({ items: [] }),
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

describe('/admin/system/performance permission guard', () => {
  it('redirects an admin without settings:manage to /403', async () => {
    installSession(['users:read'])
    const router = renderAt('/admin/system/performance')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
    expect(router.state.location.search).toMatchObject({
      redirect: '/admin/system/performance',
    })
  })

  it('does not redirect an admin holding settings:manage', async () => {
    installSession(['settings:manage'])
    installPerformance()
    const router = renderAt('/admin/system/performance')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/admin/system/performance')
    })
  })
})
