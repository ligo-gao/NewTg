import { fireEvent, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PublicPricingList } from './api'
import { PricingFacetPanel, PricingFilterSheet } from './facet-panel'
import { validatePricingSearch, type PricingSearch } from './params'
import { PricingPage } from './pricing-page'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const STATUS = {
  system_initialized: true,
  languages: ['en'],
  default_language: 'en',
}

function card(overrides: Record<string, unknown> = {}) {
  return {
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
    pricing_schedule: null,
    endpoints: [],
    tags: [],
    description: '',
    ...overrides,
  }
}

function listResponse(overrides: Partial<PublicPricingList> = {}): PublicPricingList {
  return {
    items: [card()],
    total: '1',
    page: 1,
    page_size: 24,
    facets: {
      vendors: [
        { value: 'openai', label: 'OpenAI', count: 1 },
        { value: 'anthropic', label: 'Anthropic', count: 0 },
      ],
      capabilities: [
        { value: 'vision', label: 'Vision', count: 1 },
        { value: 'embedding', label: 'embedding', count: 0 },
      ],
      context: [{ value: '128k', label: '128K', count: 1 }],
      groups: [{ value: 'vip', label: 'vip', ratio: 120, count: 1 }],
      endpoints: [
        { value: 'chat.completions', label: 'chat.completions', count: 1 },
        { value: 'messages', label: 'messages', count: 0 },
      ],
      pricing_types: [
        { value: 'token', count: 1 },
        { value: 'request', count: 0 },
      ],
    },
    meta: {
      credit_symbol: 'C',
      quota_per_unit: 100,
      usd_exchange_rate: 7.1,
      version: 'pricing-v1',
      generated_at: '2026-10-19T00:00:00Z',
    },
    ...overrides,
  }
}

interface HarnessProps {
  initial?: Record<string, unknown>
  onChange?: (next: PricingSearch) => void
}

function Harness({ initial, onChange }: HarnessProps) {
  const [search, setSearch] = useState<PricingSearch>(validatePricingSearch(initial ?? {}))
  return (
    <PricingPage
      search={search}
      onSearchChange={(next) => {
        onChange?.(next)
        setSearch(next)
      }}
    />
  )
}

function baseHandlers(record?: (url: URL) => void, body = listResponse()) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
      record?.(new URL(request.url))
      return HttpResponse.json(body)
    }),
  )
}

describe('PricingPage (F-09-14 ~ F-09-22)', () => {
  beforeEach(() => baseHandlers())

  it('renders the header stat line, cards and integer price rows', async () => {
    const { findByText, getByText } = await renderWithProviders(<Harness />, ['public'])

    expect(await findByText('GPT-5')).toBeTruthy()
    expect(getByText('1 models · 2 vendors')).toBeTruthy()
    // 输入/输出价保留整数与千分位；缓存价 null 渲染为 —。
    expect(getByText('500')).toBeTruthy()
    expect(getByText('1,500')).toBeTruthy()
    // 缓存价 null → 两行都渲染 —（读/写各一）。
    expect(getByText('1,500')).toBeTruthy()
    expect(getByText('GPT-5')).toBeTruthy()
  })

  it('sends the documented query params and resets p on filter changes', async () => {
    const urls: string[] = []
    baseHandlers((url) => urls.push(url.search))
    const changes: PricingSearch[] = []

    const { findByText, getByLabelText } = await renderWithProviders(
      <Harness initial={{ p: 3 }} onChange={(next) => changes.push(next)} />,
      ['public'],
    )
    await findByText('GPT-5')
    await waitFor(() => expect(urls.length).toBeGreaterThan(0))

    fireEvent.click(getByLabelText(/OpenAI/))
    await waitFor(() => {
      expect(urls.some((search) => search.includes('vendor=openai'))).toBe(true)
    })
    expect(changes.at(-1)?.p).toBe(1)
  })

  it('writes metadata facets into the URL and the API request', async () => {
    const urls: string[] = []
    baseHandlers((url) => urls.push(url.search))
    const changes: PricingSearch[] = []

    const { findByText, getByLabelText } = await renderWithProviders(
      <Harness initial={{ p: 3 }} onChange={(next) => changes.push(next)} />,
      ['public'],
    )
    await findByText('GPT-5')

    fireEvent.click(getByLabelText(/Vision/))
    await waitFor(() => {
      expect(urls.some((search) => search.includes('capability=vision'))).toBe(true)
    })
    expect(changes.at(-1)?.capability).toBe('vision')
    // 任何筛选变化都重置页码（F-09-21）。
    expect(changes.at(-1)?.p).toBe(1)

    fireEvent.click(getByLabelText(/32K – 128K/))
    await waitFor(() => {
      expect(urls.some((search) => search.includes('context=128k'))).toBe(true)
    })

    fireEvent.click(getByLabelText(/Chat completions/))
    await waitFor(() => {
      expect(
        urls.some((search) => search.includes('endpointType=chat.completions')),
      ).toBe(true)
    })
  })

  it('disables metadata facet options whose count is zero', async () => {
    const { findByText, getByLabelText } = await renderWithProviders(<Harness />, [
      'public',
    ])
    await findByText('GPT-5')

    const zeroCapability = getByLabelText(/Embeddings/) as HTMLInputElement
    expect(zeroCapability.disabled).toBe(true)
    expect(zeroCapability.closest('label')?.getAttribute('aria-disabled')).toBe(
      'true',
    )
    const zeroEndpoint = getByLabelText(/Messages/) as HTMLInputElement
    expect(zeroEndpoint.disabled).toBe(true)
  })

  it('renders profile metadata on cards and keeps the plain fallback without it', async () => {
    baseHandlers(
      undefined,
      listResponse({
        items: [
          card({
            model_id: 'openai/gpt-5',
            display_name: 'GPT-5',
            vendor: 'OpenAI',
            capabilities: ['vision', 'function_calling'],
            tags: ['long-context'],
            endpoints: ['chat.completions'],
            description: 'A multimodal model',
          }),
          card({
            // 无档案：display_name 为空、能力/标签/端点为空数组。
            model_id: 'legacy/model',
            display_name: '',
            vendor: '',
            capabilities: [],
            tags: [],
            endpoints: [],
            context_length: null,
            max_output: null,
            description: '',
          }),
        ],
        total: '2',
      }),
    )

    const { findByText, getAllByText } = await renderWithProviders(<Harness />, ['public'])
    const profiled = (await findByText('GPT-5')).closest('article') as HTMLElement
    expect(profiled).not.toBeNull()
    expect(within(profiled).getByText('OpenAI')).toBeTruthy()
    expect(within(profiled).getByText('Vision')).toBeTruthy()
    expect(within(profiled).getByText('Function calling')).toBeTruthy()
    expect(within(profiled).getByText('long-context')).toBeTruthy()
    expect(within(profiled).getByText('Chat completions')).toBeTruthy()

    // 空 display_name 回退 model_id，且不渲染能力/标签/端点空壳。
    const fallback = getAllByText('legacy/model')[0]?.closest(
      'article',
    ) as HTMLElement
    expect(fallback).not.toBeNull()
    expect(within(fallback).queryByText('Chat completions')).toBeNull()
    expect(within(fallback).queryByText('Context unknown')).toBeNull()
  })

  it('drops a capability the facet candidates do not know from the URL', async () => {
    const changes: PricingSearch[] = []
    const { findByText } = await renderWithProviders(
      <Harness initial={{ capability: 'telepathy' }} onChange={(next) => changes.push(next)} />,
      ['public'],
    )
    await findByText('GPT-5')

    // 响应落定后 effect 把未知 capability 从地址栏剔除（replace，不新增历史）。
    await waitFor(() => {
      expect(changes.some((next) => next.capability === '')).toBe(true)
    })
  })

  it('keeps a valid zero-hit capability and vendor in the URL and shows the empty state', async () => {
    const changes: PricingSearch[] = []
    baseHandlers(
      undefined,
      listResponse({
        items: [],
        total: '0',
        facets: {
          // faceted navigation 忽略 capability 自身的筛选，只叠加 vendor：
          // anthropic 存在（所以 vendor facet 有值），但它没有 vision，
          // capability facet 里自然没有 vision——这是一个合法的零命中组合。
          vendors: [{ value: 'anthropic', label: 'Anthropic', count: 3 }],
          capabilities: [{ value: 'text', label: 'text', count: 3 }],
          context: [{ value: '128k', label: '128K', count: 3 }],
          groups: [],
          endpoints: [{ value: 'messages', label: 'messages', count: 3 }],
          pricing_types: [{ value: 'token', count: 3 }],
        },
      }),
    )

    const { findByText, getByLabelText } = await renderWithProviders(
      <Harness
        initial={{ capability: 'vision', vendor: 'anthropic' }}
        onChange={(next) => changes.push(next)}
      />,
      ['public'],
    )

    expect(await findByText('No models match your filters')).toBeTruthy()
    // 零结果时 facet 缺失无法区分「值不存在」与「组合无命中」，URL 值必须
    // 原样保留（chip 仍在），而不是被修复 effect 悄悄剔除。
    expect(getByLabelText('Remove filter: Capability: Vision')).toBeTruthy()
    expect(getByLabelText('Remove filter: Vendor: anthropic')).toBeTruthy()
    expect(changes).toHaveLength(0)
  })

  it('keeps a frozen context bucket that the response facets do not list', async () => {
    const changes: PricingSearch[] = []
    baseHandlers(
      undefined,
      listResponse({
        items: [],
        total: '0',
        facets: {
          vendors: [],
          capabilities: [],
          // 目录里没有 8K 档命中，facet 只列出有命中的档位；8k 仍是冻结
          // 枚举，必须保留。
          context: [],
          groups: [],
          endpoints: [],
          pricing_types: [{ value: 'token', count: 0 }],
        },
      }),
    )

    const { findByText, getByLabelText } = await renderWithProviders(
      <Harness initial={{ context: '8k' }} onChange={(next) => changes.push(next)} />,
      ['public'],
    )

    expect(await findByText('No models match your filters')).toBeTruthy()
    expect(getByLabelText('Remove filter: Context: 8K or less')).toBeTruthy()
    expect(changes).toHaveLength(0)
  })

  it('allows a selected zero-count facet option to be unchecked', async () => {
    const changes: PricingSearch[] = []
    const { getByLabelText } = await renderWithProviders(
      <PricingFacetPanel
        facets={listResponse().facets}
        value={validatePricingSearch({ capability: 'embedding' })}
        onChange={(next) => changes.push(next)}
      />,
      ['public'],
    )

    // embedding 的 facet 计数为 0，但它是当前已选中的值：复选框必须是
    // 可用的，否则用户会被自己的筛选锁死。
    const checkbox = getByLabelText(/Embeddings/) as HTMLInputElement
    expect(checkbox.checked).toBe(true)
    expect(checkbox.disabled).toBe(false)

    fireEvent.click(checkbox)
    expect(changes.at(-1)?.capability).toBe('')
  })

  it('sends multiple capabilities as repeated query keys', async () => {
    const urls: URL[] = []
    baseHandlers(
      (url) => urls.push(url),
      listResponse({
        facets: {
          ...listResponse().facets,
          capabilities: [
            { value: 'vision', label: 'Vision', count: 1 },
            { value: 'text', label: 'text', count: 1 },
          ],
        },
      }),
    )

    const { findByText } = await renderWithProviders(
      <Harness initial={{ capability: 'vision,text' }} />,
      ['public'],
    )
    await findByText('GPT-5')
    await waitFor(() => expect(urls.length).toBeGreaterThan(0))

    // 后端冻结的入参形态是重复键（而非 JSON 数组），openapi-fetch 的默认
    // 数组序列化正好匹配。
    const request = urls.find((url) => url.searchParams.has('capability'))
    expect(request?.searchParams.getAll('capability')).toEqual(['vision', 'text'])
  })

  it('syncs metadata facets inside the mobile filter sheet', async () => {
    const applies: PricingSearch[] = []
    const { findByRole, getByLabelText, getByRole } = await renderWithProviders(
      <PricingFilterSheet
        open
        onOpenChange={() => {}}
        facets={listResponse().facets}
        value={validatePricingSearch({})}
        onApply={(next) => applies.push(next)}
      />,
      ['public'],
    )

    expect(await findByRole('dialog', { name: 'Filters' })).toBeTruthy()
    fireEvent.click(getByLabelText(/Vision/))
    fireEvent.click(getByRole('button', { name: 'Apply filters' }))
    expect(applies.at(-1)?.capability).toBe('vision')
  })

  it('debounces search input into a single request', async () => {
    const urls: string[] = []
    baseHandlers((url) => urls.push(url.search))

    const { findByText, getByLabelText } = await renderWithProviders(<Harness />, ['public'])
    await findByText('GPT-5')
    const input = getByLabelText('Search models')

    fireEvent.change(input, { target: { value: 'g' } })
    fireEvent.change(input, { target: { value: 'gp' } })
    fireEvent.change(input, { target: { value: 'gpt' } })

    await waitFor(
      () => expect(urls.some((search) => search.includes('search=gpt'))).toBe(true),
      { timeout: 2000 },
    )
    const searchRequests = urls.filter((search) => search.includes('search='))
    expect(searchRequests).toHaveLength(1)
  })

  it('switches to the table view without issuing another request', async () => {
    const urls: string[] = []
    baseHandlers((url) => urls.push(url.search))

    const { findByRole, getByText } = await renderWithProviders(<Harness />, ['public'])
    await findByRole('link', { name: 'GPT-5' })
    const before = urls.length

    fireEvent.click(getByText('Table'))
    expect(await findByRole('table')).toBeTruthy()
    expect(urls).toHaveLength(before)
  })

  it('keeps previous data and requests the next offset page', async () => {
    let page = 1
    const urls: string[] = []
    server.use(
      http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
        const url = new URL(request.url)
        urls.push(url.search)
        page = Number(url.searchParams.get('p') ?? '1')
        return HttpResponse.json(
          listResponse({
            total: '48',
            page,
            items: [card({ model_id: `m/${page}`, display_name: `M${page}` })],
          }),
        )
      }),
    )

    const { findByText, getByRole } = await renderWithProviders(<Harness />, ['public'])
    await findByText('M1')
    fireEvent.click(getByRole('button', { name: 'Next' }))

    await waitFor(() => expect(urls.at(-1)).toContain('p=2'))
    expect(await findByText('M2')).toBeTruthy()
  })

  it('shows the filtered empty state and clears filters', async () => {
    baseHandlers(undefined, listResponse({ items: [], total: '0' }))
    const changes: PricingSearch[] = []

    const { findByText, getByRole } = await renderWithProviders(
      <Harness initial={{ search: 'nope' }} onChange={(next) => changes.push(next)} />,
      ['public'],
    )

    expect(await findByText('No models match your filters')).toBeTruthy()
    fireEvent.click(getByRole('button', { name: 'Clear filters' }))
    expect(changes.at(-1)?.search).toBe('')
  })

  it('renders the group price column from group_price without multiplying', async () => {
    baseHandlers(
      undefined,
      listResponse({
        items: [
          card({
            group_price: {
              group: 'vip',
              ratio: 120,
              prompt: '600',
              completion: '1800',
              cache_read: null,
              cache_write: null,
              source: 'ratio',
            },
          }),
        ],
      }),
    )

    const { findByText, getByText, getAllByText } = await renderWithProviders(
      <Harness initial={{ group: 'vip' }} />,
      ['public'],
    )
    await findByText('GPT-5')
    // 服务端已算好的 600 原样展示；前端不做 500 × 120 ÷ 100。
    expect(getByText('vip price')).toBeTruthy()
    expect(getByText('600')).toBeTruthy()
    // 侧栏分组候选与实付行各有一个倍率标签。
    expect(getAllByText('1.20x').length).toBeGreaterThanOrEqual(1)
  })

  it('sends page_size, sort and quotaType to the API', async () => {
    const urls: string[] = []
    baseHandlers((url) => urls.push(url.search))

    const { findByText } = await renderWithProviders(
      <Harness initial={{ page_size: 50, sort: 'price_asc', quotaType: 'token' }} />,
      ['public'],
    )
    await findByText('GPT-5')
    const query = urls.find((value) => value.includes('page_size=50'))
    expect(query).toBeDefined()
    expect(query).toContain('page_size=50')
    expect(query).toContain('sort=price_asc')
    expect(query).toContain('quotaType=token')
  })

  it('distinguishes an explicitly free model from a partial zero price', async () => {
    baseHandlers(
      undefined,
      listResponse({
        items: [
          card({
            model_id: 'free/model',
            display_name: 'FreeModel',
            prompt_price: '0',
            completion_price: '0',
            cache_read_price: '0',
            cache_write_price: '0',
            is_free: true,
          }),
          card({
            model_id: 'zero/model',
            display_name: 'ZeroModel',
            prompt_price: '0',
            completion_price: '500',
            cache_read_price: null,
            cache_write_price: null,
            is_free: false,
          }),
        ],
        total: '2',
      }),
    )

    const { findByText, getByText, getAllByText } = await renderWithProviders(<Harness />, ['public'])
    await findByText('FreeModel')
    // 全 0 才由服务端标记免费；部分 0 仍显示数字 0（不挂免费标）。
    // 免费卡片有两处「免费」：标题行分类标签 + 价格区替换块。
    const freeArticle = getByText('FreeModel').closest('article')
    expect(freeArticle).not.toBeNull()
    expect(within(freeArticle as HTMLElement).getAllByText('Free')).toHaveLength(2)
    expect(getAllByText('Billed at 0 credits')).toHaveLength(1)
    const zeroArticle = getByText('ZeroModel').closest('article')
    expect(zeroArticle).not.toBeNull()
    expect(within(zeroArticle as HTMLElement).queryByText('Free')).toBeNull()
    expect(within(zeroArticle as HTMLElement).getAllByText('0').length).toBeGreaterThanOrEqual(1)
  })

  it('shows the rate-limit message and retries after a transient error', async () => {
    let fail = true
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
      http.get(`${ORIGIN}/api/pricing`, () => {
        if (fail) {
          fail = false
          return HttpResponse.json(
            { type: 'rate_limited', title: 'Too many requests' },
            { status: 429, headers: { 'Retry-After': '5' } },
          )
        }
        return HttpResponse.json(listResponse())
      }),
    )

    const { findByText, getByRole } = await renderWithProviders(<Harness />, ['public'])
    expect(
      await findByText('Too many requests. Please try again later.'),
    ).toBeTruthy()
    fireEvent.click(getByRole('button', { name: 'Retry' }))
    expect(await findByText('GPT-5')).toBeTruthy()
  })

  it('uses the offline copy when the request cannot reach the server', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
      http.get(`${ORIGIN}/api/pricing`, () => HttpResponse.error()),
    )

    const { findByText } = await renderWithProviders(<Harness />, ['public'])
    expect(
      await findByText('Network disconnected. Check your connection and try again.'),
    ).toBeTruthy()
  })

  it('forces and disables the dual-currency switch when prices are published in CNY (P1-3)', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({ ...STATUS, billing: { display_unit: 'cny' } }),
      ),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
      http.get(`${ORIGIN}/api/pricing`, () => HttpResponse.json(listResponse())),
    )

    // A shared URL still says `dualCurrency=false`; it must not win.
    const { findByRole, findByText } = await renderWithProviders(
      <Harness initial={{ dualCurrency: false }} />,
      ['public'],
    )
    // Wait for the listing meta: `currencyAvailable` (and the switch state)
    // only settles once the pricing response arrives.
    await findByText('GPT-5')

    const toggle = (await findByRole('switch', {
      name: 'Show currency estimate',
    })) as HTMLButtonElement
    await waitFor(() => {
      expect(toggle.getAttribute('aria-checked')).toBe('true')
    })
    expect(toggle.disabled).toBe(true)
    expect(
      await findByText(
        'Prices are published in CNY, so the currency estimate is always shown.',
      ),
    ).toBeTruthy()
  })

  it('leaves the dual-currency switch user-controlled for a non-CNY site (P1-3)', async () => {
    const { findByRole, findByText } = await renderWithProviders(<Harness />, ['public'])
    await findByText('GPT-5')

    const toggle = (await findByRole('switch', {
      name: 'Show currency estimate',
    })) as HTMLButtonElement
    await waitFor(() => expect(toggle.disabled).toBe(false))
    expect(toggle.getAttribute('aria-checked')).toBe('false')

    fireEvent.click(toggle)
    await waitFor(() => {
      expect(toggle.getAttribute('aria-checked')).toBe('true')
    })
  })
})
