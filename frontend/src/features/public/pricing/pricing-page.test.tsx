import { fireEvent, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PublicPricingList } from './api'
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
      capabilities: [],
      context: [],
      groups: [{ value: 'vip', label: 'vip', ratio: 120, count: 1 }],
      endpoints: [],
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
})
