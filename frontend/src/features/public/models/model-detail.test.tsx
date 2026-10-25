import { screen, within } from '@testing-library/react'
import { delay, HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { PublicModelCard, PublicModelDetail } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { ModelDetail } from './model-detail'
import type { ModelEntry } from './performance'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
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

async function renderDetail(entry: ModelEntry = ENTRY) {
  return renderWithProviders(<ModelDetail entry={entry} />, ['public'])
}

describe('ModelDetail', () => {
  it('renders the card fields before detail arrives', async () => {
    server.use(
      http.get(`${ORIGIN}/api/pricing/:modelId`, async () => {
        await delay('infinite')
        return HttpResponse.json(DETAIL)
      }),
    )
    await renderDetail()

    expect(screen.getByText('Flagship reasoning model')).toBeTruthy()
    expect(screen.getByText('128K')).toBeTruthy()
    expect(screen.getByText('32K')).toBeTruthy()
    expect(screen.getByText('Chat completions')).toBeTruthy()
    expect(screen.getByText('Messages')).toBeTruthy()
    expect(screen.getByText('Text')).toBeTruthy()
    expect(screen.getByText('Vision')).toBeTruthy()
    expect(screen.getByText('Loading model details')).toBeTruthy()
  })

  it('shows modalities, knowledge cutoff and release date', async () => {
    serveDetail()
    await renderDetail()

    expect(await screen.findByText('Text · Image')).toBeTruthy()
    expect(screen.getByText(expectedMonth('en', '2025-03'))).toBeTruthy()
    expect(screen.getByText(expectedDate('en', '2025-08-07'))).toBeTruthy()
    expect(screen.getByText('120')).toBeTruthy()
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
    expect(screen.getByText('128K')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })
})
