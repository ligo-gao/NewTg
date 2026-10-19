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

const server = setupServer()
let pricingRequests = 0

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  window.scrollTo = () => {}
})
afterEach(() => {
  server.resetHandlers()
  pricingRequests = 0
})
afterAll(() => server.close())

function status(nav: Record<string, unknown>) {
  return {
    system_initialized: true,
    auth: {
      signup_enabled: true,
      password_login_enabled: true,
      email_verification_required: false,
      turnstile: { enabled: false, site_key: '' },
    },
    languages: ['en'],
    default_language: 'en',
    nav,
  }
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
      <I18nProvider modules={['public', 'auth']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return router
}

const LIST = {
  items: [],
  total: '0',
  page: 1,
  page_size: 24,
  facets: { vendors: [], capabilities: [], context: [], groups: [], endpoints: [], pricing_types: [] },
  meta: {
    credit_symbol: 'C',
    quota_per_unit: 100,
    usd_exchange_rate: 7.1,
    version: 'v1',
    generated_at: '2026-10-19T00:00:00Z',
  },
}

function handlers(nav: Record<string, unknown>, guest = true) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(status(nav))),
    http.get(`${ORIGIN}/api/me`, () =>
      guest
        ? HttpResponse.json({ type: 'unauthorized' }, { status: 401 })
        : HttpResponse.json({
            username: 'u',
            role: 'user',
            status: 'enabled',
            permissions: [],
          }),
    ),
    http.get(`${ORIGIN}/api/pricing`, () => {
      pricingRequests += 1
      return HttpResponse.json(LIST)
    }),
    http.get(`${ORIGIN}/api/home/stats`, () =>
      HttpResponse.json({ upstreams: 0, models: 0, endpoints: 3, routing_dims: 6 }),
    ),
  )
}

describe('pricing route guard (F-09-14)', () => {
  it('renders the marketplace for the safe default entry', async () => {
    handlers({ pricing: { enabled: true, require_auth: false } })
    const router = renderAt('/pricing')
    await waitFor(() => expect(router.state.location.pathname).toBe('/pricing'))
    await waitFor(() => expect(pricingRequests).toBeGreaterThan(0))
  })

  it('silently redirects to the home page when nav.pricing.enabled=false', async () => {
    handlers({ pricing: { enabled: false, require_auth: false } })
    const router = renderAt('/pricing')
    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
  })

  it('sends a guest to sign-in with the full original URL when require_auth=true', async () => {
    handlers({ pricing: { enabled: true, require_auth: true } })
    const router = renderAt('/pricing?sort=price_asc')
    await waitFor(() => expect(router.state.location.pathname).toBe('/sign-in'))
    // 路由的 validateSearch 会物化默认值，所以 redirect 里除了原始 search
    // 还带着 p/page_size 等默认项；关键断言是原搜索参数被完整保留。
    const redirect = router.state.location.search.redirect
    expect(typeof redirect).toBe('string')
    expect(redirect).toContain('/pricing?')
    expect(redirect).toContain('sort=price_asc')
  })

  it('keeps a model_id containing "/" inside one encoded path segment', async () => {
    const paths: string[] = []
    const detail = {
      model_id: 'openai/gpt-5',
      display_name: 'GPT-5',
      vendor: 'openai',
      capabilities: [],
      context_length: 128000,
      max_output: 32000,
      mode: 'per_token',
      prompt_price: '500',
      completion_price: '1500',
      cache_read_price: null,
      cache_write_price: null,
      is_free: false,
      group_price: null,
      endpoints: [],
      tags: [],
      description: '',
      modalities: [],
      knowledge_cutoff: null,
      released_at: null,
      group_prices: [],
      parameters: [],
      rate_limits: [],
      auth_style: 'bearer',
    }
    handlers({ pricing: { enabled: true, require_auth: false } })
    server.use(
      http.get(`${ORIGIN}/api/pricing/:modelId`, ({ request }) => {
        paths.push(new URL(request.url).pathname)
        return HttpResponse.json(detail)
      }),
    )

    const router = renderAt('/pricing/openai%2Fgpt-5')
    await waitFor(() => expect(paths.length).toBeGreaterThan(0))
    expect(paths[0]).toBe('/api/pricing/openai%2Fgpt-5')
    expect(router.state.location.pathname).toBe('/pricing/openai%2Fgpt-5')
  })
})
