import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { ModelsPage } from './page'
import type { ModelsSearch } from './params'

const ORIGIN = 'http://localhost:3000'

const STATUS = {
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
  site: { server_address: 'https://api.example.com/' },
  nav: {},
}

function card(
  modelId: string,
  overrides: Partial<PublicModelCard> = {},
): PublicModelCard {
  return {
    model_id: modelId,
    display_name: modelId,
    vendor: 'acme',
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
    ...overrides,
  }
}

const GPT = card('openai/gpt-5', {
  display_name: 'GPT-5',
  vendor: 'openai',
  capabilities: ['text', 'vision'],
  context_length: 128_000,
  max_output: 32_000,
  endpoints: ['chat.completions'],
})

const EMBED = card('acme/embed', {
  display_name: 'Embed',
  vendor: 'acme',
  capabilities: ['embedding'],
  endpoints: ['embeddings'],
})

const METRICS = {
  '24h': {
    range: '24h',
    window: { start: '2026-10-23T00:00:00Z', end: '2026-10-24T00:00:00Z' },
    items: metricsItems(),
  },
  '7d': {
    range: '7d',
    window: { start: '2026-10-17T00:00:00Z', end: '2026-10-24T00:00:00Z' },
    items: metricsItems(),
  },
}

function metricsItems() {
  return [
    {
      model_id: 'openai/gpt-5',
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
    {
      model_id: 'acme/embed',
      summary: {
        avg_latency_ms: 120,
        avg_ttft_ms: null,
        success_rate: 0.98,
        tps: 30,
        sample_size: 50,
      },
      latest_test: null,
    },
  ]
}

const BASE: ModelsSearch = {
  q: '',
  cap: '',
  vendor: '',
  context: '',
  endpoint: '',
  measured: false,
  sort: 'measured',
  range: '7d',
  view: 'table',
  compare: '',
}

type ChangeHandler = (next: ModelsSearch, replace?: boolean) => void

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

interface ServeOptions {
  items?: PublicModelCard[]
  total?: number
  catalogStatus?: number
  metricsStatus?: number
  onCatalog?: (page: number) => Response
  onMetrics?: (range: string) => Response
}

function serveCatalog(options: ServeOptions = {}) {
  const items = options.items ?? [GPT, EMBED]
  const total = options.total ?? items.length

  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    http.get(`${ORIGIN}/api/pricing`, ({ request }) => {
      const page = Number(new URL(request.url).searchParams.get('p') ?? '1')
      if (options.onCatalog !== undefined) return options.onCatalog(page)
      if (options.catalogStatus !== undefined) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'unavailable' },
          { status: options.catalogStatus },
        )
      }
      return HttpResponse.json({
        items: page === 1 ? items : [],
        total: String(total),
        page,
        page_size: 100,
        facets: {
          capabilities: [],
          vendors: [],
          contexts: [],
          endpoints: [],
          groups: [],
        },
        meta: {},
      })
    }),
    http.get(`${ORIGIN}/api/pricing/metrics`, ({ request }) => {
      const range = new URL(request.url).searchParams.get('range') ?? '7d'
      if (options.onMetrics !== undefined) return options.onMetrics(range)
      if (options.metricsStatus !== undefined) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'unavailable' },
          { status: options.metricsStatus },
        )
      }
      return HttpResponse.json(
        METRICS[range === '24h' ? '24h' : '7d'],
      )
    }),
  )
}

function Harness({
  initial = BASE,
  onChange,
}: {
  initial?: ModelsSearch
  onChange?: ChangeHandler
}) {
  const [search, setSearch] = useState(initial)
  return (
    <ModelsPage
      search={search}
      onSearchChange={(next, replace) => {
        onChange?.(next, replace)
        setSearch(next)
      }}
    />
  )
}

function renderPage(initial: ModelsSearch = BASE, onChange?: ChangeHandler) {
  return renderWithProviders(
    <Harness initial={initial} onChange={onChange} />,
    ['public'],
  )
}

describe('ModelsPage', () => {
  it('renders the catalog joined with metrics', async () => {
    serveCatalog()
    await renderPage()

    expect(await screen.findByText('GPT-5')).toBeTruthy()
    expect(screen.getByText('Embed')).toBeTruthy()
    // The chat model takes the measured-test reading; the embedding falls back
    // to the real-call average, which proves the metrics join happened.
    expect(await screen.findByText('Measured test')).toBeTruthy()
    expect(
      await screen.findByText(/Total latency average 120 ms/),
    ).toBeTruthy()
  })

  it('keeps the list when metrics fail and shows the unavailable banner', async () => {
    serveCatalog({ metricsStatus: 503 })
    await renderPage()

    expect(
      await screen.findByText(
        'Live measurements are unavailable right now. Capabilities and limits are still shown.',
      ),
    ).toBeTruthy()
    // The catalog list is still complete; only readings degrade to `—`.
    expect(screen.getByText('GPT-5')).toBeTruthy()
    expect(screen.getByText('Embed')).toBeTruthy()
  })

  it('shows a retry state when the catalog fails', async () => {
    let calls = 0
    serveCatalog({
      onCatalog: () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'unavailable' },
            { status: 503 },
          )
        }
        return HttpResponse.json({
          items: [GPT, EMBED],
          total: '2',
          page: 1,
          page_size: 100,
          facets: {
            capabilities: [],
            vendors: [],
            contexts: [],
            endpoints: [],
            groups: [],
          },
          meta: {},
        })
      },
    })
    await renderPage()

    expect(
      await screen.findByText('The model catalog could not be loaded.'),
    ).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('GPT-5')).toBeTruthy()
    expect(calls).toBe(2)
  })

  it('reports a truncated catalog', async () => {
    serveCatalog({ items: [GPT, EMBED, card('acme/third')], total: 5 })
    await renderPage()

    expect(
      await screen.findByText('Showing the first 3 models of 5.'),
    ).toBeTruthy()
  })

  it('writes filter changes back to the URL', async () => {
    serveCatalog()
    const onChange = vi.fn<ChangeHandler>()
    await renderPage(BASE, onChange)
    await screen.findByText('GPT-5')

    fireEvent.click(screen.getByRole('checkbox', { name: 'Vision' }))

    await waitFor(() => {
      expect(onChange).toHaveBeenCalled()
    })
    const last = onChange.mock.lastCall?.[0] as ModelsSearch
    expect(last.cap).toBe('vision')
    // The embedding has no 'vision' capability; only the table row is
    // filtered, not the hero stats.
    const table = await screen.findByRole('table')
    expect(within(table).queryByText('Embed')).toBeNull()
  })

  it('switches range and refetches metrics with ?range=24h', async () => {
    const ranges: string[] = []
    serveCatalog({
      onMetrics: (range) => {
        ranges.push(range)
        return HttpResponse.json(METRICS[range === '24h' ? '24h' : '7d'])
      },
    })
    await renderPage()
    await screen.findByText('GPT-5')

    fireEvent.click(screen.getByRole('button', { name: 'Last 24h' }))

    await waitFor(() => {
      expect(ranges).toContain('24h')
    })
  })

  it('repairs compare ids that are not in the catalog', async () => {
    serveCatalog()
    const onChange = vi.fn<ChangeHandler>()
    await renderPage(
      { ...BASE, compare: 'openai/gpt-5,ghost/missing' },
      onChange,
    )

    await waitFor(() => {
      const repaired = onChange.mock.calls.find(
        ([next, replace]) =>
          replace === true && next.compare === 'openai/gpt-5',
      )
      expect(repaired).toBeTruthy()
    })
  })

  it('opens the call dialog from a row without expanding it', async () => {
    serveCatalog()
    await renderPage()
    await screen.findByText('GPT-5')

    fireEvent.click(screen.getByRole('button', { name: 'Call GPT-5' }))

    expect(await screen.findByRole('dialog')).toBeTruthy()
    expect(
      screen.getByRole('heading', { name: 'Call GPT-5' }),
    ).toBeTruthy()
    expect(document.getElementById('model-detail-openai/gpt-5')).toBeNull()
  })

  it('shows the empty state and clears every filter', async () => {
    serveCatalog()
    const onChange = vi.fn<ChangeHandler>()
    await renderPage({ ...BASE, q: 'no-such-model' }, onChange)

    expect(
      await screen.findByText('No models match these filters'),
    ).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }))

    expect(await screen.findByText('GPT-5')).toBeTruthy()
    const last = onChange.mock.lastCall?.[0] as ModelsSearch
    expect(last.q).toBe('')
    expect(last.cap).toBe('')
    expect(last.vendor).toBe('')
    expect(last.context).toBe('')
    expect(last.endpoint).toBe('')
    expect(last.measured).toBe(false)
  })
})
