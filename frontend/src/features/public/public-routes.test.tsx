import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, waitFor, within } from '@testing-library/react'
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

const PRICING_DETAIL = {
  model_id: 'gpt-4o',
  display_name: 'GPT-4o',
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
  modalities: ['text', 'image'],
  knowledge_cutoff: null,
  released_at: null,
  group_prices: [],
  parameters: [],
  rate_limits: [],
  auth_style: 'bearer',
}

/** `/models` 目录卡片：一条 chat 模型，形状取自生成类型 `PublicModelCard`。 */
const MODEL_CARD = {
  model_id: 'gpt-4o',
  display_name: 'GPT-4o',
  vendor: 'openai',
  capabilities: ['text', 'vision'],
  context_length: 128000,
  max_output: 32000,
  mode: 'per_token',
  prompt_price: '500',
  completion_price: '1500',
  cache_read_price: null,
  cache_write_price: null,
  is_free: false,
  group_price: null,
  pricing_schedule: null,
  endpoints: ['chat.completions'],
  tags: [],
  description: '',
}

const MODEL_LIST = {
  items: [MODEL_CARD],
  total: '1',
  page: 1,
  page_size: 100,
  facets: {
    vendors: [],
    capabilities: [],
    context: [],
    groups: [],
    endpoints: [],
    pricing_types: [],
  },
  meta: {},
}

/** 一次管理员测速：有 P50/P95/TPS，证明指标确实被并进登记册。 */
const MODEL_METRICS = {
  range: '7d',
  window: { start: '2026-10-18T00:00:00Z', end: '2026-10-25T00:00:00Z' },
  items: [
    {
      model_id: 'gpt-4o',
      summary: { sample_size: 120 },
      latest_test: {
        ttft_ms_p50: 300,
        ttft_ms_p95: 600,
        latency_ms_p50: 500,
        latency_ms_p95: 900,
        tps: 42,
        success_rate: 0.99,
        sample_size: 120,
        tested_at: '2026-10-24T00:00:00Z',
      },
    },
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
    // `/models` 目录逐页拉全 + 批量指标。两条都必须在 `/api/pricing/:modelId`
    // 之前登记：MSW 按注册顺序匹配，否则 `metrics` 会被当成 modelId。
    http.get(`${ORIGIN}/api/pricing/metrics`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json(MODEL_METRICS)
    }),
    http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json(MODEL_LIST)
    }),
    http.get(`${ORIGIN}/api/pricing/:modelId`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json(PRICING_DETAIL)
    }),
    // 显式登记 /readyz：否则「公开外壳不探测 readyz」的断言是空的——
    // 请求只会落到未处理分支，不会出现在 requestedPaths 里。
    http.get(`${ORIGIN}/readyz`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json({ status: 'ok', checks: {} })
    }),
    // Public shell probes the session (401 = guest, silent) and stats band.
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    http.get(`${ORIGIN}/api/home/stats`, () =>
      HttpResponse.json({ upstreams: 0, models: 0, endpoints: 3, routing_dims: 6 }),
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

  it('renders /pricing/$modelId with the pricing marketplace breadcrumb level', async () => {
    installPublicHandlers()
    const { findByText, findByRole, router } = renderAt('/pricing/gpt-4o')

    expect(await findByRole('heading', { name: 'GPT-4o' })).toBeTruthy()
    expect(await findByText('500')).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/pricing/gpt-4o')
    })

    const nav = await findByRole('navigation', { name: 'Breadcrumb' })
    expect(nav.textContent).toContain('Home')
    expect(nav.textContent).toContain('Model Marketplace')
    expect(nav.textContent).toContain('GPT-4o')
    expect(requestedPaths.some((path) => path === '/readyz')).toBe(false)
  })

  it('renders the model library route with catalog and metrics', async () => {
    installPublicHandlers()
    const { findByRole, router } = renderAt('/models')

    // 匿名直达 `/models`，标题来自 public.models.title。
    expect(
      await findByRole('heading', { level: 1, name: 'Model library' }),
    ).toBeTruthy()

    // 登记册（宽屏下是表格）里能看到目录模型；页头「最快」副标题也可能
    // 重复展示名，因此断言只针对登记册本身。
    const table = await findByRole('table')
    expect(within(table).getByText('GPT-4o')).toBeTruthy()
    // 指标确实并进了读数：管理员测速的 TTFT P50 而不是空读数。
    expect(within(table).getByText(/TTFT P50 300 ms/)).toBeTruthy()

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/models')
    })
    expect(requestedPaths).toContain('/api/pricing')
    expect(requestedPaths).toContain('/api/pricing/metrics')
    expect(requestedPaths.some((path) => path === '/readyz')).toBe(false)
  })

  it('hides /models and redirects home when nav.models is disabled', async () => {
    installPublicHandlers()
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({
          ...PUBLIC_STATUS,
          nav: { models: { enabled: false, require_auth: false } },
        }),
      ),
    )
    const { findByRole, router } = renderAt('/models')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/')
    })
    // 导航里不再有入口，且页面从未渲染（没有去拉目录指标）。
    const nav = await findByRole('navigation', { name: 'Public pages' })
    expect(within(nav).queryByRole('link', { name: 'Models' })).toBeNull()
    expect(requestedPaths).not.toContain('/api/pricing/metrics')
  })

  it('renders a public announcement inside the shared header/footer shell', async () => {
    installPublicHandlers()
    server.use(
      http.get(`${ORIGIN}/api/announcements/public/:id`, () =>
        HttpResponse.json({
          id: 'a-1',
          title: 'Planned maintenance',
          category: 'maintenance',
          content_html: '<p>The gateway restarts at 02:00 UTC.</p>',
          published_at: '2026-10-01T10:00:00Z',
          expire_at: null,
        }),
      ),
    )

    const { findByRole, getByRole, router } = renderAt('/announcements/a-1')

    expect(
      await findByRole('heading', { level: 1, name: 'Planned maintenance' }),
    ).toBeTruthy()
    // PublicLayout owns the footer; the page itself does not render one.
    expect(getByRole('contentinfo')).toBeTruthy()
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/announcements/a-1')
    })
  })
})
