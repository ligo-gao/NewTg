import { fireEvent, screen, waitFor, within } from '@testing-library/react'
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
afterEach(() => {
  server.resetHandlers()
  Reflect.deleteProperty(navigator, 'clipboard')
})
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
  price: { input: 0.06, output: 4, cacheRead: null },
  perf: perf({
    source: 'test',
    measure: 'ttft',
    p50: 350,
    p95: 700,
    tps: 42.5,
    successRate: 0.992,
    samples: 3,
    lowSample: true,
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
  price: { input: null, output: null, cacheRead: null },
  perf: perf({
    source: 'usage',
    measure: 'latency',
    p50: 240,
    tps: 30,
    successRate: 0.98,
    samples: 1_204,
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
  price: { input: null, output: null, cacheRead: null },
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

/** Content rows in DOM order (the header row excluded). */
function bodyRows(): HTMLElement[] {
  return within(screen.getByRole('table')).getAllByRole('row').slice(1)
}

function textOf(row: HTMLElement, selector: string): string {
  const cell = row.querySelector(selector)
  expect(cell).toBeTruthy()
  return cell?.textContent ?? ''
}

describe('Register: table view', () => {
  it('renders one table row per entry', async () => {
    await renderRegister()

    const table = screen.getByRole('table')
    // The header row is the extra one.
    expect(within(table).getAllByRole('row')).toHaveLength(4)
  })

  it('contains its visually hidden labels inside the scroll container', async () => {
    await renderRegister()

    // `sr-only` is absolutely positioned, so without a positioned ancestor the
    // caption and header spans resolve against the initial containing block and
    // escape the wrapper's overflow-x clip, adding phantom page-level scroll.
    const wrapper = screen.getByRole('table').parentElement
    expect(wrapper?.classList.contains('table-wrap')).toBe(true)
    expect(wrapper?.classList.contains('relative')).toBe(true)
  })

  it('renders the price column with input and output per million', async () => {
    await renderRegister()

    expect(
      screen.getByRole('columnheader', { name: /Price ¥\/million tokens/ }),
    ).toBeTruthy()
    expect(screen.getByText('Input · output')).toBeTruthy()

    // The whole cell pins input-before-output; two loose matches would pass
    // even if the prices were swapped.
    expect(textOf(bodyRows()[0] as HTMLElement, '.price-cell')).toBe(
      '¥0.06 · ¥4.00',
    )

    // A row without prices keeps its two positions separate, not a fake zero.
    const priceCell = bodyRows()[2]?.querySelector('.price-cell')
    expect(priceCell).toBeTruthy()
    expect(
      within(priceCell as HTMLElement).getAllByText('—'),
    ).toHaveLength(2)
  })

  it('keeps a missing price as its own em dash beside the present one', async () => {
    const partial: ModelEntry = {
      ...TESTED,
      price: { input: 0.06, output: null, cacheRead: null },
    }
    await renderRegister({ entries: [partial] })

    expect(textOf(bodyRows()[0] as HTMLElement, '.price-cell')).toBe(
      '¥0.06 · —',
    )
  })

  it('hides the context column when no row has context or output', async () => {
    serveDetails()
    await renderRegister({
      entries: [AVERAGED, EMPTY],
      expandedId: AVERAGED.card.model_id,
    })

    expect(screen.getAllByRole('columnheader')).toHaveLength(7)
    expect(
      screen.queryByRole('columnheader', { name: 'Context / output' }),
    ).toBeNull()

    const detail = document.getElementById('model-detail-acme/embed')
    expect(detail?.querySelector('td')?.getAttribute('colspan')).toBe('7')
  })

  it('shows the context column when at least one row has values', async () => {
    serveDetails()
    await renderRegister({ expandedId: TESTED.card.model_id })

    expect(screen.getAllByRole('columnheader')).toHaveLength(8)
    expect(
      screen.getByRole('columnheader', { name: 'Context / output' }),
    ).toBeTruthy()

    const detail = document.getElementById('model-detail-openai/gpt-5')
    expect(detail?.querySelector('td')?.getAttribute('colspan')).toBe('8')
  })

  it('summarises the evidence source in the evidence column', async () => {
    await renderRegister()

    const rows = bodyRows()
    const tested = rows[0] as HTMLElement
    const averaged = rows[1] as HTMLElement
    const none = rows[2] as HTMLElement

    // Test source reports probe runs and a two-decimal success rate.
    expect(textOf(tested, '.evidence-cell')).toContain('3 probe runs')
    expect(textOf(tested, '.evidence-cell')).toContain('Success 99.20%')
    // Low sample survives the column rewrite.
    expect(textOf(tested, '.evidence-cell')).toContain('Low sample')

    // Usage source reports grouped call counts, never percentile language.
    expect(textOf(averaged, '.evidence-cell')).toContain('1,204 calls')
    expect(textOf(averaged, '.evidence-cell')).toContain('Success 98.00%')

    expect(textOf(none, '.evidence-cell')).toContain('Not measured')
    expect(textOf(none, '.evidence-cell')).toContain('No success rate yet')
  })

  it('labels end-to-end readings so they are never mistaken for TTFT', async () => {
    await renderRegister()

    const rows = bodyRows()
    expect(textOf(rows[0] as HTMLElement, '.lat-cell')).toContain('350 ms')
    expect(textOf(rows[0] as HTMLElement, '.lat-cell')).toContain('P95 700 ms')
    expect(textOf(rows[0] as HTMLElement, '.lat-cell')).not.toContain(
      'Total latency',
    )

    // An average has no P95, and a non-chat measurement must say so.
    expect(textOf(rows[1] as HTMLElement, '.lat-cell')).toContain('240 ms')
    expect(textOf(rows[1] as HTMLElement, '.lat-cell')).not.toContain('P95')
    expect(textOf(rows[1] as HTMLElement, '.lat-cell')).toContain(
      'Total latency',
    )
  })

  it('shows capability chips and the vendor under the model name', async () => {
    await renderRegister()

    const tested = bodyRows()[0] as HTMLElement
    expect(within(tested).getByText('GPT-5')).toBeTruthy()
    expect(within(tested).getByText('openai/gpt-5')).toBeTruthy()
    for (const chip of ['text', 'Vision', 'hologram', 'openai']) {
      expect(within(tested).getByText(chip)).toBeTruthy()
    }
  })

  it('shows the profile placeholder and prints a matched id once', async () => {
    // No profile at all: one explicit placeholder instead of empty chips.
    const bare: ModelEntry = {
      ...EMPTY,
      card: card({ model_id: 'acme/bare' }),
    }
    await renderRegister({ entries: [bare] })

    expect(screen.getByText('Profile not filled in')).toBeTruthy()
    // display_name falls back to model_id, so the id is printed once, not twice.
    expect(screen.getAllByText('acme/bare')).toHaveLength(1)
  })

  it('prints the model id once when display_name is empty', async () => {
    const blank: ModelEntry = {
      ...EMPTY,
      card: card({ model_id: 'acme/raw', display_name: '' }),
    }
    await renderRegister({ entries: [blank] })

    expect(screen.getAllByText('acme/raw')).toHaveLength(1)
  })

  it('copies the model id, not the display name', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })
    const onToggleExpand = vi.fn()

    await renderRegister({ onToggleExpand })

    // The accessible name carries the row's model, so each row is distinct.
    const button = screen.getByRole('button', { name: 'Copy model ID GPT-5' })

    // Reachable by keyboard even though it only appears on hover/focus.
    expect(button.tabIndex).toBeGreaterThanOrEqual(0)
    expect(button.hasAttribute('hidden')).toBe(false)
    expect(button.classList.contains('pointer-events-none')).toBe(false)
    expect(button.className).toContain('focus-visible:opacity-100')

    fireEvent.click(button)
    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith('openai/gpt-5')
    })
    expect(onToggleExpand).not.toHaveBeenCalled()
  })

  it('never renders a capability barcode column or a call button', async () => {
    await renderRegister()

    expect(
      screen.queryByRole('columnheader', { name: 'Capabilities' }),
    ).toBeNull()
    expect(screen.getByRole('table').querySelector('.barcode')).toBeNull()
    expect(screen.queryByRole('button', { name: /^Call / })).toBeNull()
  })

  it('renders an em dash, never 0, when a measurement is missing', async () => {
    await renderRegister()

    const rows = bodyRows()
    const emptyRow = rows[2] as HTMLElement
    expect(emptyRow).toBeTruthy()

    expect(within(emptyRow).queryByText('0 ms')).toBeNull()
    expect(textOf(emptyRow, '.evidence-cell')).toContain('No success rate yet')
    expect(textOf(emptyRow, '.lat-cell')).toBe('—')
    expect(textOf(emptyRow, '.tps-cell')).toBe('—')
    expect(
      within(emptyRow.querySelector('.price-cell') as HTMLElement).getAllByText(
        '—',
      ),
    ).toHaveLength(2)
  })

  it('renders an em dash for a negative throughput', async () => {
    const negative: ModelEntry = {
      ...EMPTY,
      perf: perf({ source: 'test', p50: 100, tps: -3, samples: 5, hasData: true }),
    }
    await renderRegister({ entries: [negative] })

    expect(textOf(bodyRows()[0] as HTMLElement, '.tps-cell')).toBe('—')
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

  it('renders the success rate without leaking the key placeholder', async () => {
    await renderRegister({ view: 'cards' })

    expect(screen.getByText('Success 99.20%')).toBeTruthy()
    expect(screen.getByText('No success rate yet')).toBeTruthy()
    expect(screen.queryByText(/\{rate\}/)).toBeNull()
  })
})
