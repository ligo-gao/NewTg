import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

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

// 输入价 60_000 / 4_000_000 积分，配合 quota_per_unit = 1_000_000 即
// ¥0.06 / ¥4.00：这两个数字只会在 meta 真贯穿到 `entry.price` 时出现。
const CHEAP = card('acme/cheap', {
  display_name: 'Cheap',
  prompt_price: '60000',
})

const DEAR = card('acme/dear', {
  display_name: 'Dear',
  prompt_price: '4000000',
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
      summary: { sample_size: 3 },
      latest_test: {
        ttft_ms_p50: 300,
        ttft_ms_p95: 600,
        latency_ms_p50: 500,
        latency_ms_p95: 900,
        tps: 42,
        success_rate: 0.99,
        sample_size: 3,
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
        sample_size: 1_204,
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

const originalMatchMedia = window.matchMedia

/**
 * jsdom ships no `matchMedia`, and `useMediaQuery` treats a missing API as
 * "wide". These cases cross both width gates (1200px rail, 1440px table, 760px
 * cards), so install a stub that answers every `min-width` / `max-width` query
 * for one concrete viewport width.
 */
function mockViewport(width: number): void {
  window.matchMedia = ((query: string) => {
    const min = /min-width:\s*(\d+)px/.exec(query)
    const max = /max-width:\s*(\d+)px/.exec(query)
    const matches =
      min !== null
        ? width >= Number(min[1])
        : max !== null
          ? width <= Number(max[1])
          : true
    return {
      matches,
      media: query,
      onchange: null,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      addListener: () => undefined,
      removeListener: () => undefined,
      dispatchEvent: () => false,
    }
  }) as unknown as typeof window.matchMedia
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => mockViewport(1600))
afterEach(() => {
  server.resetHandlers()
  window.matchMedia = originalMatchMedia
})
afterAll(() => server.close())

interface ServeOptions {
  items?: PublicModelCard[]
  total?: number
  catalogStatus?: number
  metricsStatus?: number
  /** 计费元数据；默认 `{}`（没有 `quota_per_unit`）。 */
  meta?: Record<string, unknown>
  /** 模拟裁剪过的旧版响应：整个 `meta` 字段缺失。 */
  omitMeta?: boolean
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
        ...(options.omitMeta ? {} : { meta: options.meta ?? {} }),
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
    http.get(`${ORIGIN}/api/pricing/:modelId`, ({ params }) => {
      const modelId = String(params.modelId)
      const source =
        items.find((item) => item.model_id === modelId) ?? items[0] ?? GPT
      return HttpResponse.json({
        ...source,
        modalities: ['text'],
        knowledge_cutoff: null,
        released_at: null,
        group_prices: [],
        parameters: [],
        rate_limits: [],
        auth_style: 'bearer',
      })
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

/** 读取页头统计格子的 `dd` 文本；`label` 是英文 `dt` 文案。 */
function statValue(label: string): string {
  return (
    screen.getByText(label).closest('div')?.querySelector('dd')?.textContent?.trim() ??
    ''
  )
}

describe('ModelsPage', () => {
  it('renders the catalog joined with metrics', async () => {
    serveCatalog()
    await renderPage()

    expect(await screen.findByText('GPT-5')).toBeTruthy()
    // The hero's fastest-reading note shows the model id, so scope the
    // catalog assertions to the register itself.
    const table = screen.getByRole('table')
    expect(within(table).getByText('GPT-5')).toBeTruthy()
    expect(within(table).getByText('Embed')).toBeTruthy()
    // The evidence column proves the metrics join happened: the measured chat
    // model reports probe runs, while the embedding has no probe record and
    // falls back to real-call counts.
    const tested = within(table).getByText('GPT-5').closest('tr')
    expect(tested).toBeTruthy()
    expect(
      await within(tested as HTMLElement).findByText('3 probe runs'),
    ).toBeTruthy()
    const averaged = within(table).getByText('Embed').closest('tr')
    expect(averaged).toBeTruthy()
    expect(
      await within(averaged as HTMLElement).findByText('1,204 calls'),
    ).toBeTruthy()
  })

  it('forces cards below 760px and never writes the forced view to the URL', async () => {
    mockViewport(600)
    serveCatalog()
    const onChange = vi.fn<ChangeHandler>()
    await renderPage({ ...BASE, view: 'table' }, onChange)
    await screen.findByText('GPT-5')

    // The URL still says table; the layout overrides it with cards.
    expect(screen.queryByRole('table')).toBeNull()
    expect(document.querySelectorAll('.cards > li')).toHaveLength(2)

    // The toggle still renders, but reports the view that is actually shown,
    // and the unreachable table side is explicitly marked unavailable (with
    // its reason announced) rather than a button that silently ignores clicks.
    expect(
      screen
        .getByRole('button', { name: 'Cards' })
        .getAttribute('aria-pressed'),
    ).toBe('true')
    const tableToggle = screen.getByRole('button', {
      name: 'Table',
    }) as HTMLButtonElement
    expect(tableToggle.getAttribute('aria-pressed')).toBe('false')
    expect(tableToggle.getAttribute('aria-disabled')).toBe('true')
    // 原生 disabled 会把按钮移出 Tab 序列；保持可聚焦并朗读原因。
    expect(tableToggle.disabled).toBe(false)
    expect(tableToggle.title).toBe('Table view needs a wider window')

    // A view-only click cannot persist the forced view.
    fireEvent.click(tableToggle)
    expect(onChange).not.toHaveBeenCalled()
    expect(
      screen
        .getByRole('button', { name: 'Cards' })
        .getAttribute('aria-pressed'),
    ).toBe('true')

    // Any other toolbar change keeps the URL's own view, never the forced one.
    fireEvent.click(screen.getByRole('button', { name: 'Last 24h' }))
    const last = onChange.mock.lastCall?.[0] as ModelsSearch
    expect(last.range).toBe('24h')
    expect(last.view).toBe('table')
  })

  it('marks the table toggle unavailable between 761 and 1439px instead of lying', async () => {
    // The table needs 1440px: at 1280/1366 it cannot fit seven columns even
    // though it is well above the 760px card fallback.
    mockViewport(1280)
    serveCatalog()
    const onChange = vi.fn<ChangeHandler>()
    await renderPage({ ...BASE, view: 'table' }, onChange)
    await screen.findByText('GPT-5')

    // The URL still says `table`; the list renders cards and the toggle must
    // report that reality rather than claim a table that is not on screen.
    expect(screen.queryByRole('table')).toBeNull()
    expect(document.querySelectorAll('.cards > li')).toHaveLength(2)

    const tableToggle = screen.getByRole('button', {
      name: 'Table',
    }) as HTMLButtonElement
    expect(tableToggle.disabled).toBe(false)
    expect(tableToggle.getAttribute('aria-disabled')).toBe('true')
    expect(tableToggle.getAttribute('aria-pressed')).toBe('false')
    expect(
      screen
        .getByRole('button', { name: 'Cards' })
        .getAttribute('aria-pressed'),
    ).toBe('true')

    // Clicking the unavailable view cannot persist something unrenderable.
    fireEvent.click(tableToggle)
    expect(onChange).not.toHaveBeenCalled()
    expect(screen.queryByRole('table')).toBeNull()
  })

  it('keeps the list when metrics fail and shows the unavailable banner', async () => {
    serveCatalog({ metricsStatus: 503 })
    // `measured: true` would normally require a reading; with metrics down the
    // filter is bypassed instead of emptying the list.
    await renderPage({ ...BASE, measured: true })

    expect(
      await screen.findByText(
        'Live measurements are unavailable right now. Capabilities and limits are still shown.',
      ),
    ).toBeTruthy()
    // The catalog list is still complete; only readings degrade to `—`.
    expect(screen.getByText('GPT-5')).toBeTruthy()
    expect(screen.getByText('Embed')).toBeTruthy()
  })

  it('no longer renders the distribution panel', async () => {
    serveCatalog()
    await renderPage()

    await screen.findByText('GPT-5')
    // The `models.dist.*` copy keys stay in the locales, but the v3 page has
    // no distribution block: neither its median nor its title may appear.
    expect(screen.queryByText('Median first-token latency')).toBeNull()
    expect(screen.queryByText(/Current filter ·/)).toBeNull()
  })

  it('keeps the glossary and the metrics-unavailable banner', async () => {
    serveCatalog({ metricsStatus: 503 })
    await renderPage()

    expect(
      await screen.findByText(
        'Live measurements are unavailable right now. Capabilities and limits are still shown.',
      ),
    ).toBeTruthy()

    fireEvent.click(
      screen.getByRole('button', { name: 'How these numbers are measured' }),
    )
    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByText('How these numbers are measured'),
    ).toBeTruthy()
  })

  it('states the data window in the page footer', async () => {
    serveCatalog()
    await renderPage()

    expect(await screen.findByText('Data window: Last 7 days')).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Last 24h' }))
    expect(await screen.findByText('Data window: Last 24 hours')).toBeTruthy()
  })

  it('brings the metrics banner back when the range changes', async () => {
    serveCatalog({ metricsStatus: 503 })
    await renderPage()

    const banner =
      'Live measurements are unavailable right now. Capabilities and limits are still shown.'
    expect(await screen.findByText(banner)).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(screen.queryByText(banner)).toBeNull()

    // A new measurement window must not inherit the dismissed banner.
    fireEvent.click(screen.getByRole('button', { name: 'Last 24h' }))
    expect(await screen.findByText(banner)).toBeTruthy()
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

  it('opens the call dialog from the expanded row and keeps it expanded', async () => {
    serveCatalog()
    await renderPage()
    await screen.findByText('GPT-5')

    fireEvent.click(screen.getByRole('button', { name: 'Details GPT-5' }))

    const detail = await waitFor(() => {
      const element = document.getElementById('model-detail-openai/gpt-5')
      expect(element).toBeTruthy()
      return element as HTMLElement
    })
    fireEvent.click(
      within(detail).getByRole('button', { name: 'View call examples' }),
    )

    expect(await screen.findByRole('dialog')).toBeTruthy()
    expect(
      screen.getByRole('heading', { name: 'Call GPT-5' }),
    ).toBeTruthy()
    // Opening the dialog must not collapse the row behind it. The dialog makes
    // the page inert, so the background button is only reachable as hidden.
    expect(document.getElementById('model-detail-openai/gpt-5')).toBeTruthy()
    expect(
      screen
        .getByRole('button', {
          name: 'Hide details GPT-5',
          hidden: true,
        })
        .getAttribute('aria-expanded'),
    ).toBe('true')
  })

  it('reads /api/status once for the page, not once per expanded row', async () => {
    serveCatalog()
    // `server.use` 后注册的先匹配：用它把站点地址的读取次数记下来。
    let statusCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/status`, () => {
        statusCalls += 1
        return HttpResponse.json(STATUS)
      }),
    )
    await renderPage()
    await screen.findByText('GPT-5')
    const afterLoad = statusCalls
    expect(afterLoad).toBeGreaterThan(0)

    // 展开详情会挂载一次 ModelDetail。站点地址由页面读一次后下传，详情不再
    // 自取：`staleTime: 0` 会让每个挂载的消费者各打一次，行数越多越糟。
    fireEvent.click(screen.getByRole('button', { name: 'Details GPT-5' }))
    await waitFor(() =>
      expect(
        document.getElementById('model-detail-openai/gpt-5'),
      ).toBeTruthy(),
    )
    fireEvent.click(screen.getByRole('button', { name: 'Details Embed' }))

    expect(statusCalls).toBe(afterLoad)
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

  it('threads the catalog quota_per_unit into the hero price range', async () => {
    serveCatalog({
      items: [CHEAP, DEAR],
      meta: { quota_per_unit: 1_000_000 },
    })
    await renderPage()

    // Only reachable if the catalog meta really flows into `entry.price`:
    // passing `undefined` as `entriesOf`'s third argument would render `—`.
    expect(await screen.findByText('¥0.06–¥4.00')).toBeTruthy()
  })

  it('renders an em dash for prices when the catalog omits meta', async () => {
    serveCatalog({ items: [CHEAP], omitMeta: true })
    await renderPage()

    expect(await screen.findByText('Cheap')).toBeTruthy()
    expect(statValue('Input price / million tokens')).toBe('—')
  })

  it('renders an em dash for prices when quota_per_unit is missing', async () => {
    serveCatalog({ items: [CHEAP], meta: {} })
    await renderPage()

    expect(await screen.findByText('Cheap')).toBeTruthy()
    expect(statValue('Input price / million tokens')).toBe('—')
  })
})
