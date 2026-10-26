import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'

import {
  CATALOG_PAGE_SIZE,
  MAX_CATALOG_PAGES,
  fetchLibraryMetrics,
  fetchModelCatalog,
  indexMetrics,
  libraryMetricsQueryOptions,
  modelCatalogQueryOptions,
  type MetricsItem,
} from './api'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function card(modelId: string): PublicModelCard {
  return {
    model_id: modelId,
    display_name: modelId,
    vendor: 'openai',
    capabilities: [],
    context_length: null,
    max_output: null,
    mode: 'per_token',
    prompt_price: null,
    completion_price: null,
    cache_read_price: null,
    cache_write_price: null,
    is_free: false,
    group_price: null,
    pricing_schedule: null,
    endpoints: [],
    tags: [],
    description: '',
  }
}

function cards(prefix: string, count: number): PublicModelCard[] {
  return Array.from({ length: count }, (_, index) => card(`${prefix}-${index}`))
}

function metric(modelId: string, sampleSize = 0): MetricsItem {
  return {
    model_id: modelId,
    summary: { sample_size: sampleSize },
    latest_test: null,
  }
}

describe('fetchModelCatalog', () => {
  it('fetches one page and reports total', async () => {
    const requested: URL[] = []
    server.use(
      http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
        const url = new URL(request.url)
        requested.push(url)
        return HttpResponse.json({ items: cards('model', 2), total: '2' })
      }),
    )

    const catalog = await fetchModelCatalog()

    expect(catalog.items).toHaveLength(2)
    expect(catalog.total).toBe(2)
    expect(catalog.truncated).toBe(false)
    expect(requested).toHaveLength(1)
    expect(requested[0]?.searchParams.get('p')).toBe('1')
    expect(requested[0]?.searchParams.get('page_size')).toBe('100')
    expect(requested[0]?.searchParams.get('sort')).toBe('name')
  })

  it('keeps the meta from the first page and ignores later pages', async () => {
    server.use(
      http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
        const p = Number(new URL(request.url).searchParams.get('p'))
        return HttpResponse.json({
          items: cards(`page${p}`, 100),
          total: '150',
          meta: {
            credit_symbol: '¥',
            quota_per_unit: p === 1 ? 500000 : 1,
            usd_exchange_rate: 7.2,
            version: 'v1',
            generated_at: '2026-10-26T00:00:00Z',
          },
        })
      }),
    )

    const catalog = await fetchModelCatalog()

    expect(catalog.meta?.quota_per_unit).toBe(500000)
  })

  it('follows pages until total, dedupes by model_id', async () => {
    // 250 unique models spread over three pages; the last page repeats an id
    // from page 1, so the raw item count (251) exceeds the deduped total.
    const page3 = [...cards('page3', 50), card('page-0')]
    server.use(
      http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
        const p = Number(new URL(request.url).searchParams.get('p'))
        const items =
          p === 1
            ? cards('page', 100)
            : p === 2
              ? cards('page2', 100)
              : p === 3
                ? page3
                : []
        return HttpResponse.json({ items, total: '250' })
      }),
    )

    const catalog = await fetchModelCatalog()

    expect(catalog.items).toHaveLength(250)
    expect(new Set(catalog.items.map((item) => item.model_id)).size).toBe(250)
    expect(catalog.items.filter((item) => item.model_id === 'page-0')).toHaveLength(1)
    expect(catalog.total).toBe(250)
    expect(catalog.truncated).toBe(false)
  })

  it('stops at 20 pages and sets truncated', async () => {
    const requestedPages: number[] = []
    server.use(
      http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
        const p = Number(new URL(request.url).searchParams.get('p'))
        requestedPages.push(p)
        return HttpResponse.json({
          items: cards(`p${p}`, CATALOG_PAGE_SIZE),
          total: '5000',
        })
      }),
    )

    const catalog = await fetchModelCatalog()

    expect(MAX_CATALOG_PAGES).toBe(20)
    expect(requestedPages).toEqual(
      Array.from({ length: MAX_CATALOG_PAGES }, (_, index) => index + 1),
    )
    expect(catalog.items).toHaveLength(CATALOG_PAGE_SIZE * MAX_CATALOG_PAGES)
    expect(catalog.truncated).toBe(true)
  })

  it('stops on an empty page instead of looping', async () => {
    const requestedPages: number[] = []
    server.use(
      http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
        const p = Number(new URL(request.url).searchParams.get('p'))
        requestedPages.push(p)
        return HttpResponse.json({
          items: p === 1 ? cards('page', CATALOG_PAGE_SIZE) : [],
          total: '5000',
        })
      }),
    )

    const catalog = await fetchModelCatalog()

    expect(requestedPages).toEqual([1, 2])
    expect(catalog.items).toHaveLength(CATALOG_PAGE_SIZE)
    expect(catalog.truncated).toBe(true)
  })

  it('preserves model ids containing / and :', async () => {
    server.use(
      http.get(`${ORIGIN}/api/pricing`, () =>
        HttpResponse.json({
          items: [card('openai/gpt-5:free'), card('openai/gpt-5')],
          total: '2',
        }),
      ),
    )

    const catalog = await fetchModelCatalog()

    expect(catalog.items.map((item) => item.model_id)).toEqual([
      'openai/gpt-5:free',
      'openai/gpt-5',
    ])
  })

  it('exposes a stable query key for the whole catalogue', () => {
    expect(modelCatalogQueryOptions().queryKey).toEqual([
      'public',
      'models',
      'catalog',
    ])
  })
})

describe('fetchLibraryMetrics', () => {
  it('returns items: [] when the metrics response omits items', async () => {
    let requestedRange = ''
    server.use(
      http.get(`${ORIGIN}/api/pricing/metrics`, ({ request }) => {
        requestedRange = new URL(request.url).searchParams.get('range') ?? ''
        return HttpResponse.json({
          range: '7d',
          window: { start: '2026-10-17T00:00:00Z', end: '2026-10-24T00:00:00Z' },
        })
      }),
    )

    const metrics = await fetchLibraryMetrics('7d')

    expect(requestedRange).toBe('7d')
    expect(metrics.range).toBe('7d')
    expect(metrics.window).toEqual({
      start: '2026-10-17T00:00:00Z',
      end: '2026-10-24T00:00:00Z',
    })
    expect(metrics.items).toEqual([])
  })

  it('propagates a 503 from the metrics endpoint', async () => {
    server.use(
      http.get(`${ORIGIN}/api/pricing/metrics`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'metrics unavailable' },
          {
            status: 503,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    )

    await expect(fetchLibraryMetrics('24h')).rejects.toMatchObject({
      status: 503,
      kind: 'server',
    })
  })

  it('keys metrics per range with retry disabled and a one-minute stale time', () => {
    const options = libraryMetricsQueryOptions('24h')
    expect(options.queryKey).toEqual(['public', 'models', 'metrics', '24h'])
    expect(options.retry).toBe(false)
    expect(options.staleTime).toBe(60_000)
  })
})

describe('indexMetrics', () => {
  it('indexes by model_id, letting a later duplicate win', () => {
    const index = indexMetrics([
      metric('openai/gpt-5', 1),
      metric('openai/gpt-5:free', 2),
      metric('openai/gpt-5', 3),
    ])

    expect(index.size).toBe(2)
    expect(index.get('openai/gpt-5')?.summary.sample_size).toBe(3)
    expect(index.get('openai/gpt-5:free')?.summary.sample_size).toBe(2)
  })
})
