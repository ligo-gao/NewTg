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

function publicStatus(playground: boolean) {
  return {
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
    nav: { modules: { chat: { playground } } },
  }
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

const RATES = {
  rpm_avg: 0,
  rpm_peak: 0,
  tpm_avg: 0,
  tpm_peak: 0,
  window_minutes: 0,
}

const MODEL_PERF = {
  window: { range: '24h', start: '', end: '', granularity: '1m' },
  buckets: [],
  summary: {},
  incidents: [],
  models: [],
  pipeline: { enabled: false, updated_at: null, stale: true },
  comparison: null,
}

const server = setupServer()

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  window.scrollTo = () => {}
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installSession(playground: boolean) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'alice',
        role: 'user',
        status: 'enabled',
        permissions: [],
      }),
    ),
    http.get(`${ORIGIN}/api/usage-stats/summary`, () => HttpResponse.json(SUMMARY)),
    http.get(`${ORIGIN}/api/usage-stats/by-model`, () => HttpResponse.json([])),
    http.get(`${ORIGIN}/api/usage-stats/timeseries`, () => HttpResponse.json([])),
    http.get(`${ORIGIN}/api/usage-stats/rates`, () => HttpResponse.json(RATES)),
    http.get(`${ORIGIN}/api/model-perf`, () => HttpResponse.json(MODEL_PERF)),
    http.get(`${ORIGIN}/api/self`, () =>
      HttpResponse.json({
        language: 'en',
        sidebar_modules: null,
        record_ip_log: false,
        default_model: null,
        updated_at: '2026-10-01T00:00:00Z',
      }),
    ),
    http.get(`${ORIGIN}/api/wallet/summary`, () =>
      HttpResponse.json({
        balance: 0,
        held_credits: 0,
        total_consumed: 0,
        total_requests: 0,
        threshold: 1000,
        health: 'healthy',
        group: 'default',
      }),
    ),
    // Playground's model dropdown comes from usable_groups; without models the
    // page still renders its empty state (this test only asserts the nav/guard).
    http.get(`${ORIGIN}/api/status`, () =>
      HttpResponse.json({
        ...publicStatus(playground),
        usable_groups: [],
      }),
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
  return { ...render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider modules={['common']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  ), router }
}

describe('playground route guard (TC-F-02-19-05)', () => {
  it('redirects a direct link to /dashboard when the site disables Playground', async () => {
    installSession(false)
    const { router } = renderAt('/playground')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/dashboard')
    })
    await waitFor(() => {
      expect(screen.queryByRole('group', { name: 'Chat' })).toBeNull()
    })
  })

  it('shows the Chat group and the Playground entry when enabled', async () => {
    installSession(true)
    const { router } = renderAt('/playground')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/playground')
    })
    const group = await screen.findByRole('group', { name: 'Chat' })
    expect(group.textContent).toContain('Playground')
  })
})
