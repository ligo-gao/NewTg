import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

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
  languages: ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi'],
  default_language: 'en',
}

const METRICS = {
  summary: {
    avg_latency_ms: 820,
    success_rate: 0.997,
    tps: 42.5,
    failed_24h: 12,
    sample_size: 1543,
  },
  per_group: [
    { group: 'default', avg_ttft_ms: 210, avg_latency_ms: 800, success_rate: 0.998 },
  ],
}

const server = setupServer()

let requestedPaths: string[] = []

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  // jsdom implements no layout; the router's scroll manager calls `scrollTo`.
  window.scrollTo = () => {}
})
beforeEach(() => {
  requestedPaths = []
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installPublicHandlers() {
  server.use(
    http.get(`${ORIGIN}/api/status`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json(PUBLIC_STATUS)
    }),
    http.get(`${ORIGIN}/api/pricing/:modelId/metrics`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json(METRICS)
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
  const result = render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider modules={['public']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return { ...result, router }
}

describe('public route integration (anonymous reachability)', () => {
  it('renders /about without a session and never probes /readyz from the public shell', async () => {
    installPublicHandlers()
    const { findByRole, router } = renderAt('/about')

    // 匿名可直达公开页，未被初始化/会话闸门重定向。
    expect(await findByRole('heading', { level: 1 })).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/about')
    })
    expect(requestedPaths).toContain('/api/status')
    // 裁决一（modules-09 C 系列）：公开外壳不得请求 /readyz。
    expect(requestedPaths.some((path) => path === '/readyz')).toBe(false)
  })

  it('renders /pricing/$modelId with the disabled middle breadcrumb level', async () => {
    installPublicHandlers()
    const { findByText, findByRole, getByText, router } = renderAt('/pricing/gpt-4o')

    expect(await findByText('820 ms')).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/pricing/gpt-4o')
    })

    const nav = await findByRole('navigation', { name: 'Breadcrumb' })
    expect(nav.textContent).toContain('Home')
    expect(nav.textContent).toContain('Pricing')

    // /pricing 尚未开放（M7）：中间层不可点，但读屏器能读到原因。
    const middle = getByText('Pricing')
    expect(middle.getAttribute('aria-disabled')).toBe('true')
    const describedBy = middle.getAttribute('aria-describedby')
    expect(document.getElementById(describedBy ?? '')?.textContent).toBe(
      'This page is not open yet',
    )
    expect(requestedPaths.some((path) => path === '/readyz')).toBe(false)
  })
})
