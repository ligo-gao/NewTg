import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { delay, HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { PublicModelCard, PublicModelDetail } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { ModelDetail, type ModelDetailProps } from './model-detail'
import type { ModelEntry } from './performance'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  Reflect.deleteProperty(navigator, 'clipboard')
})
afterAll(() => server.close())

const CARD: PublicModelCard = {
  model_id: 'openai/gpt-5',
  display_name: 'GPT-5',
  vendor: 'openai',
  capabilities: ['text', 'vision'],
  context_length: 128_000,
  max_output: 32_000,
  mode: 'per_token',
  prompt_price: null,
  completion_price: null,
  cache_read_price: null,
  cache_write_price: null,
  is_free: false,
  group_price: null,
  pricing_schedule: null,
  endpoints: ['chat.completions', 'messages'],
  tags: [],
  description: 'Flagship reasoning model',
}

const ENTRY: ModelEntry = {
  card: CARD,
  kind: 'chat',
  price: { input: 0.06, output: 4, cacheRead: 0.5 },
  perf: {
    source: 'test',
    measure: 'ttft',
    p50: 350,
    p95: 700,
    tps: 42.5,
    successRate: 0.998,
    samples: 120,
    lowSample: false,
    testedAt: '2026-10-24T00:00:00Z',
    hasData: true,
  },
}

const DETAIL: PublicModelDetail = {
  ...CARD,
  modalities: ['text', 'image'],
  knowledge_cutoff: '2025-03',
  released_at: '2025-08-07',
  group_prices: [],
  parameters: [],
  rate_limits: [],
  auth_style: 'bearer',
}

/** `/api/status` is what supplies the access block's base URL. */
function serveStatus(serverAddress: string | null = 'https://api.example.com/'): void {
  server.use(
    http.get(`${ORIGIN}/api/status`, () =>
      HttpResponse.json({
        system_initialized: true,
        auth: {},
        site: serverAddress === null ? {} : { server_address: serverAddress },
      }),
    ),
  )
}

function serveDetail(detail: PublicModelDetail = DETAIL): void {
  server.use(
    http.get(`${ORIGIN}/api/pricing/:modelId`, () => HttpResponse.json(detail)),
  )
}

function expectedMonth(locale: string, value: string): string {
  const [year, month] = value.split('-').map(Number)
  return new Intl.DateTimeFormat(locale, {
    year: 'numeric',
    month: 'short',
    timeZone: 'UTC',
  }).format(new Date(Date.UTC(year ?? 0, (month ?? 1) - 1, 1)))
}

function expectedDate(locale: string, value: string): string {
  const [year, month, day] = value.split('-').map(Number)
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeZone: 'UTC',
  }).format(new Date(Date.UTC(year ?? 0, (month ?? 1) - 1, day ?? 1)))
}

async function renderDetail(
  entry: ModelEntry = ENTRY,
  overrides: Partial<ModelDetailProps> = {},
) {
  serveStatus()
  return renderWithProviders(
    <ModelDetail
      entry={entry}
      range="7d"
      onCall={vi.fn()}
      onToggleCompare={vi.fn()}
      compared={false}
      compareFull={false}
      {...overrides}
    />,
    ['public'],
  )
}

describe('ModelDetail', () => {
  it('renders the four detail blocks in order', async () => {
    serveDetail()
    await renderDetail()

    const headings = screen
      .getAllByRole('heading', { level: 4 })
      .map((heading) => heading.textContent)
    expect(headings).toEqual([
      'Measurements · Last 7 days',
      'Pricing',
      'Access',
      'Notes',
    ])
  })

  it('renders the window named by the selected range', async () => {
    serveDetail()
    await renderDetail(ENTRY, { range: '24h' })

    expect(
      screen.getByRole('heading', { level: 4, name: 'Measurements · Last 24 hours' }),
    ).toBeTruthy()
  })

  it('renders the probe readings with both percentiles and the probe note', async () => {
    serveDetail()
    await renderDetail()

    expect(screen.getByText('350 / 700 ms')).toBeTruthy()
    expect(screen.getByText('42.5 t/s')).toBeTruthy()
    expect(screen.getByText('99.80%')).toBeTruthy()
    expect(screen.getByText('120 calls')).toBeTruthy()
    expect(
      screen.getByText('120 probe runs; the rest are real call samples.'),
    ).toBeTruthy()
  })

  it('shows one number when only an average is available', async () => {
    const averaged: ModelEntry = {
      ...ENTRY,
      perf: {
        ...ENTRY.perf,
        source: 'usage',
        p50: 240,
        p95: null,
        samples: 1_204,
      },
    }
    serveDetail()
    await renderDetail(averaged)

    // An average has no percentile, so only the P50 value may be printed — the
    // page must never invent a P95 or leak `null`.
    expect(screen.getByText('First token P50 / P95')).toBeTruthy()
    expect(screen.getByText('240 ms')).toBeTruthy()
    expect(screen.queryByText(/\d+ \/ \d+ ms/)).toBeNull()
    expect(screen.queryByText(/null/)).toBeNull()
  })

  it('renders an em dash for a missing cache read price', async () => {
    const partial: ModelEntry = {
      ...ENTRY,
      price: { input: 0.06, output: 4, cacheRead: null },
    }
    serveDetail()
    await renderDetail(partial)

    const cacheRow = screen.getByText('Cache read').closest('div')
    expect(cacheRow).toBeTruthy()
    expect(within(cacheRow as HTMLElement).getByText('—')).toBeTruthy()
    expect(screen.getByText('¥0.06 / M')).toBeTruthy()
    expect(screen.getByText('¥4.00 / M')).toBeTruthy()
    expect(screen.getByText('Per token')).toBeTruthy()
  })

  it('renders the free billing mode when the model is free', async () => {
    const free: ModelEntry = {
      ...ENTRY,
      card: { ...CARD, is_free: true },
    }
    serveDetail()
    await renderDetail(free)

    expect(screen.getByText('Free')).toBeTruthy()
  })

  it('keeps the call examples reachable from the access block', async () => {
    const onCall = vi.fn()
    serveDetail()
    await renderDetail(ENTRY, { onCall })

    expect(
      await screen.findByText(
        /POST https:\/\/api\.example\.com\/v1\/chat\/completions/,
      ),
    ).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'View call examples' }))

    expect(onCall).toHaveBeenCalledTimes(1)
    expect(onCall).toHaveBeenCalledWith(ENTRY)
  })

  it('keeps a relative endpoint and a hint when the site address is unset', async () => {
    serveDetail()
    serveStatus(null)
    await renderWithProviders(
      <ModelDetail
        entry={ENTRY}
        range="7d"
        onCall={vi.fn()}
        onToggleCompare={vi.fn()}
        compared={false}
        compareFull={false}
      />,
      ['public'],
    )

    expect(screen.getByText(/POST \/v1\/chat\/completions/)).toBeTruthy()
    expect(
      await screen.findByText(
        'This site has no public address configured; replace the relative path with your gateway URL.',
      ),
    ).toBeTruthy()
  })

  it('copies the model id from the access block', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })
    serveDetail()
    await renderDetail()

    fireEvent.click(screen.getByRole('button', { name: 'Copy model_id' }))

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith('openai/gpt-5')
    })
  })

  it('toggles the compare selection from the access block', async () => {
    const onToggleCompare = vi.fn()
    serveDetail()
    await renderDetail(ENTRY, { onToggleCompare })

    const button = screen.getByRole('button', { name: 'Compare' })
    expect(button.getAttribute('aria-pressed')).toBe('false')

    fireEvent.click(button)
    expect(onToggleCompare).toHaveBeenCalledWith('openai/gpt-5')
  })

  it('disables the compare button once the selection is full', async () => {
    serveDetail()
    await renderDetail(ENTRY, { compareFull: true })

    const button = screen.getByRole('button', {
      name: 'Compare',
    }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    expect(button.getAttribute('title')).toBe('Compare is full (max 4)')
  })

  it('falls back to a no-samples sentence', async () => {
    const empty: ModelEntry = {
      ...ENTRY,
      perf: {
        ...ENTRY.perf,
        source: 'none',
        p50: null,
        p95: null,
        tps: null,
        successRate: null,
        samples: 0,
        hasData: false,
      },
    }
    serveDetail()
    await renderDetail(empty)

    expect(
      screen.getByText('No measurements in this window yet.'),
    ).toBeTruthy()
    expect(screen.queryByText('Calls in window')).toBeNull()
    expect(screen.queryByText('First token P50 / P95')).toBeNull()
  })

  it('renders the card fields before detail arrives', async () => {
    server.use(
      http.get(`${ORIGIN}/api/pricing/:modelId`, async () => {
        await delay('infinite')
        return HttpResponse.json(DETAIL)
      }),
    )
    await renderDetail()

    expect(screen.getByText('Flagship reasoning model')).toBeTruthy()
    expect(
      screen.getByText('Profile: openai · 128K context · max output 32K.'),
    ).toBeTruthy()
    expect(screen.getByText('Loading model details')).toBeTruthy()
  })

  it('shows modalities, knowledge cutoff and release date', async () => {
    serveDetail()
    await renderDetail()

    expect(await screen.findByText('Text · Image')).toBeTruthy()
    expect(screen.getByText(expectedMonth('en', '2025-03'))).toBeTruthy()
    expect(screen.getByText(expectedDate('en', '2025-08-07'))).toBeTruthy()
  })

  it('renders an em dash for a missing modality list', async () => {
    serveDetail({ ...DETAIL, modalities: [] })
    await renderDetail()

    const row = (await screen.findByText('Input / output')).closest('div')
    expect(row).toBeTruthy()
    expect(within(row as HTMLElement).getByText('—')).toBeTruthy()
  })

  it('keeps the card fields and offers retry when detail fails', async () => {
    server.use(
      http.get(`${ORIGIN}/api/pricing/:modelId`, () =>
        HttpResponse.json({ type: 'server_error' }, { status: 500 }),
      ),
    )
    await renderDetail()

    expect(
      await screen.findByText('Model details could not be loaded.'),
    ).toBeTruthy()
    expect(screen.getByText('Flagship reasoning model')).toBeTruthy()
    expect(
      screen.getByText('Profile: openai · 128K context · max output 32K.'),
    ).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })
})
