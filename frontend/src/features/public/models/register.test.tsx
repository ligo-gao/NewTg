import { fireEvent, screen, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { PublicModelCard, PublicModelDetail } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import type { ModelEntry, ModelPerformance } from './performance'
import { Register, type RegisterProps } from './register'
import type { CapabilitySlot } from './taxonomy'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function card(
  overrides: Partial<PublicModelCard> & { model_id: string },
): PublicModelCard {
  return {
    display_name: overrides.model_id,
    vendor: '',
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

function detailOf(source: PublicModelCard): PublicModelDetail {
  return {
    ...source,
    modalities: ['text'],
    knowledge_cutoff: null,
    released_at: null,
    group_prices: [],
    parameters: [],
    rate_limits: [],
    auth_style: 'bearer',
  }
}

function perf(overrides: Partial<ModelPerformance>): ModelPerformance {
  return {
    source: 'none',
    measure: 'ttft',
    p50: null,
    p95: null,
    tps: null,
    successRate: null,
    samples: 0,
    lowSample: false,
    testedAt: null,
    hasData: false,
    ...overrides,
  }
}

const TESTED: ModelEntry = {
  card: card({
    model_id: 'openai/gpt-5',
    display_name: 'GPT-5',
    vendor: 'openai',
    capabilities: ['text', 'Vision', 'hologram'],
    context_length: 128_000,
    max_output: 32_000,
    description: 'Flagship reasoning model',
    endpoints: ['chat.completions'],
  }),
  kind: 'chat',
  perf: perf({
    source: 'test',
    measure: 'ttft',
    p50: 350,
    p95: 700,
    tps: 42.5,
    successRate: 0.998,
    samples: 120,
    testedAt: '2026-10-24T00:00:00Z',
    hasData: true,
  }),
}

const AVERAGED: ModelEntry = {
  card: card({
    model_id: 'acme/embed',
    display_name: 'Embed',
    vendor: 'acme',
    capabilities: ['embedding'],
    endpoints: ['embeddings'],
  }),
  kind: 'embedding',
  perf: perf({
    source: 'usage',
    measure: 'latency',
    p50: 240,
    tps: 30,
    successRate: 0.98,
    samples: 40,
    hasData: true,
  }),
}

const EMPTY: ModelEntry = {
  card: card({
    model_id: 'acme/empty',
    display_name: 'Empty',
    vendor: 'acme',
  }),
  kind: 'chat',
  perf: perf({}),
}

const SLOTS: CapabilitySlot[] = [
  'text',
  'vision',
  'audio',
  'video',
  'function_calling',
  'tool_use',
  'reasoning',
  'embedding',
  'rerank',
  'image_generation',
  'speech',
  'transcription',
].map((value) => ({ value, label: value, count: 1 }))

function props(overrides: Partial<RegisterProps> = {}): RegisterProps {
  return {
    entries: [TESTED, AVERAGED, EMPTY],
    view: 'table',
    compareIds: [],
    slots: SLOTS,
    expandedId: null,
    onToggleExpand: vi.fn(),
    onToggleCompare: vi.fn(),
    onOpenCompare: vi.fn(),
    onCall: vi.fn(),
    ...overrides,
  }
}

function serveDetails(): void {
  server.use(
    http.get(`${ORIGIN}/api/pricing/:modelId`, ({ params }) =>
      HttpResponse.json(
        detailOf(
          card({
            model_id: String(params.modelId),
            display_name: String(params.modelId),
          }),
        ),
      ),
    ),
  )
}

async function renderRegister(overrides: Partial<RegisterProps> = {}) {
  return renderWithProviders(<Register {...props(overrides)} />, ['public'])
}

describe('Register: table view', () => {
  it('renders one table row per entry', async () => {
    await renderRegister()

    const table = screen.getByRole('table')
    // The header row is the extra one.
    expect(within(table).getAllByRole('row')).toHaveLength(4)
  })

  it('fills the twelve capability slots in the shared order', async () => {
    await renderRegister()

    const rows = within(screen.getByRole('table')).getAllByRole('row')
    const barcode = rows[1]?.querySelector('.barcode')
    expect(barcode).toBeTruthy()

    const ticks = Array.from(barcode?.querySelectorAll('i') ?? [])
    expect(ticks).toHaveLength(12)
    // 'text' is first and 'Vision' matches the lowercase 'vision' slot; every
    // other slot stays empty, so a reordered set of slots breaks this.
    expect(ticks.map((tick) => tick.className)).toEqual([
      'on',
      'on',
      '',
      '',
      '',
      '',
      '',
      '',
      '',
      '',
      '',
      '',
    ])
  })

  it('keeps an unknown capability visible in the row title', async () => {
    await renderRegister()

    const barcode = screen
      .getByRole('table')
      .querySelector('.barcode')
    expect(barcode?.getAttribute('aria-label')).toBe(
      'text, Vision, hologram',
    )
    expect(barcode?.getAttribute('title')).toBe('text, Vision, hologram')
  })

  it('renders an em dash, never 0, when a measurement is missing', async () => {
    await renderRegister()

    const rows = within(screen.getByRole('table')).getAllByRole('row')
    const emptyRow = rows[3]
    expect(emptyRow).toBeTruthy()

    expect(within(emptyRow as HTMLElement).queryByText('0 ms')).toBeNull()
    expect(within(emptyRow as HTMLElement).queryByText('0.0%')).toBeNull()
    expect(
      within(emptyRow as HTMLElement).getAllByText('—').length,
    ).toBeGreaterThanOrEqual(3)
  })

  it('labels measured test, live average and no-data sources', async () => {
    await renderRegister()

    expect(screen.getByText('Measured test')).toBeTruthy()
    expect(screen.getByText('Live traffic average')).toBeTruthy()
    expect(screen.getByText('No data')).toBeTruthy()
    // A usage-sourced average must say so; it is not a percentile.
    expect(screen.getByText('average')).toBeTruthy()
    // The usage strip must not borrow a P95 from the average.
    const usageRow = within(screen.getByRole('table')).getAllByRole('row')[2]
    expect(
      usageRow?.querySelector('.range-strip')?.getAttribute('aria-label'),
    ).toContain('P95 —')
  })

  it('disables an unselected compare box once four are chosen', async () => {
    const many = [1, 2, 3, 4, 5].map((index) => ({
      ...EMPTY,
      card: card({
        model_id: `acme/model-${index}`,
        display_name: `Model ${index}`,
      }),
    }))
    const selected = many.slice(0, 4).map((entry) => entry.card.model_id)

    await renderRegister({ entries: many, compareIds: selected })

    expect(screen.getByLabelText('Compare Model 1')).toHaveProperty(
      'disabled',
      false,
    )
    expect(screen.getByLabelText('Compare Model 5')).toHaveProperty(
      'disabled',
      true,
    )
    expect(
      screen.getByLabelText('Compare Model 5').getAttribute('title'),
    ).toBe('Compare is full (max 4)')
  })

  it('calls onCall without expanding the row', async () => {
    const onCall = vi.fn()
    const onToggleExpand = vi.fn()
    await renderRegister({ onCall, onToggleExpand })

    fireEvent.click(screen.getByRole('button', { name: 'Call GPT-5' }))

    expect(onCall).toHaveBeenCalledTimes(1)
    expect(onCall).toHaveBeenCalledWith(TESTED)
    expect(onToggleExpand).not.toHaveBeenCalled()
  })

  it('mounts the detail only for the expanded row', async () => {
    serveDetails()
    await renderRegister({ expandedId: TESTED.card.model_id })

    const toggle = screen.getByRole('button', { name: 'Hide details GPT-5' })
    expect(toggle.getAttribute('aria-expanded')).toBe('true')
    expect(toggle.getAttribute('aria-controls')).toBe(
      'model-detail-openai/gpt-5',
    )
    expect(document.getElementById('model-detail-openai/gpt-5')).toBeTruthy()
    expect(document.getElementById('model-detail-acme/embed')).toBeNull()
  })
})

describe('Register: cards view', () => {
  it('renders one card per entry in cards view', async () => {
    await renderRegister({ view: 'cards' })

    expect(screen.queryByRole('table')).toBeNull()
    expect(screen.getAllByRole('listitem')).toHaveLength(3)
  })
})
