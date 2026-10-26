import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { CompareDialog } from './compare-dialog'
import type { ModelEntry, ModelPerformance } from './performance'
import type { ModelPrice } from './price'

function card(
  modelId: string,
  overrides: Partial<PublicModelCard> = {},
): PublicModelCard {
  return {
    model_id: modelId,
    display_name: modelId,
    vendor: 'acme',
    capabilities: ['text'],
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
    endpoints: ['chat.completions'],
    tags: [],
    description: '',
    ...overrides,
  }
}

function performance(overrides: Partial<ModelPerformance>): ModelPerformance {
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

function entry(
  modelId: string,
  overrides: Partial<ModelPerformance> = {},
  price: Partial<ModelPrice> = {},
): ModelEntry {
  return {
    card: card(modelId),
    kind: 'chat',
    price: { input: null, output: null, cacheRead: null, ...price },
    perf: performance(overrides),
  }
}

async function renderDialog(entries: ModelEntry[], handlers = {}) {
  return renderWithProviders(
    <CompareDialog open onOpenChange={vi.fn()} entries={entries} {...handlers} />,
    ['public'],
  )
}

describe('CompareDialog', () => {
  it('renders one column per compared model in URL order', async () => {
    // Entries arrive in URL order: Beta before Alpha, even though the catalog
    // would sort Alpha first.
    await renderDialog([
      entry('vendor/beta', { source: 'test', p50: 300 }),
      entry('vendor/alpha', { source: 'test', p50: 200 }),
    ])

    const dialog = screen.getByRole('dialog')
    const headers = within(dialog).getAllByRole('columnheader')
    // The first header is the field-label column.
    expect(headers).toHaveLength(3)
    expect(headers[1]?.textContent).toContain('vendor/beta')
    expect(headers[2]?.textContent).toContain('vendor/alpha')
  })

  it('labels the reading source per model', async () => {
    await renderDialog([
      entry('vendor/tested', { source: 'test', p50: 300, samples: 120 }),
      entry('vendor/usage', { source: 'usage', p50: 400, samples: 40 }),
      entry('vendor/empty'),
    ])

    expect(screen.getByText('Measured test')).toBeTruthy()
    expect(screen.getByText('Live traffic average')).toBeTruthy()
    expect(screen.getByText('No data')).toBeTruthy()
  })

  it('adds input and output price rows per million tokens', async () => {
    await renderDialog([
      entry('vendor/cheap', {}, { input: 0.06, output: 4 }),
      entry('vendor/unknown'),
    ])

    const dialog = screen.getByRole('dialog')
    const inputRow = within(dialog)
      .getByText('Input price / M')
      .closest('tr') as HTMLElement
    // Column order follows the entries: the priced model first, then the
    // honest em dash for the one without a usable price.
    expect(within(inputRow).getByText('¥0.06')).toBeTruthy()
    expect(within(inputRow).getAllByText('—')).toHaveLength(1)

    const outputRow = within(dialog)
      .getByText('Output price / M')
      .closest('tr') as HTMLElement
    expect(within(outputRow).getByText('¥4.00')).toBeTruthy()
    expect(within(outputRow).getAllByText('—')).toHaveLength(1)
  })

  it('renders a per-request model in the per-call unit, not per million tokens', async () => {
    await renderDialog([
      entry('vendor/ocr', {}, { input: 0.5, perCall: true }),
    ])

    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText('Per call')).toBeTruthy()
    expect(within(dialog).getByText('¥0.50')).toBeTruthy()
    // A single per-call row replaces both per-million rows entirely.
    expect(within(dialog).queryByText('Input price / M')).toBeNull()
    expect(within(dialog).queryByText('Output price / M')).toBeNull()
    expect(within(dialog).queryByText(/\/ ?M/)).toBeNull()
  })

  it('omits the P95 subline when the reading has no percentile', async () => {
    await renderDialog([
      entry('vendor/avg', {
        source: 'usage',
        measure: 'latency',
        p50: 240,
        p95: null,
      }),
    ])

    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText('240 ms')).toBeTruthy()
    // An average has no P95; printing `P95 —` would invent a percentile row.
    expect(within(dialog).queryByText(/^P95 /)).toBeNull()
    expect(within(dialog).queryByText('P95 —')).toBeNull()
    // The end-to-end qualifier keeps the number from reading as first-token.
    expect(within(dialog).getByText('Total latency')).toBeTruthy()
  })

  it('uses the table wording for success rate and throughput', async () => {
    await renderDialog([
      entry('vendor/tested', {
        source: 'test',
        p50: 300,
        tps: 42.5,
        successRate: 0.992,
        samples: 3,
      }),
    ])

    const dialog = screen.getByRole('dialog')
    // `successLabel()` folds the rate into the sentence (two decimals); the
    // bare `{rate}` placeholder must never leak into the dialog.
    expect(within(dialog).getByText('Success 99.20%')).toBeTruthy()
    expect(screen.queryByText(/\{rate\}/)).toBeNull()
    // `tpsText()`: one decimal, exactly like the table and the card.
    expect(within(dialog).getByText('42.5 t/s')).toBeTruthy()
  })

  it('never invents a success rate for a model without one', async () => {
    await renderDialog([entry('vendor/silent')])

    expect(screen.getByText('No success rate yet')).toBeTruthy()
  })

  it('hides remove controls when no handler is passed', async () => {
    const first = await renderDialog([entry('vendor/beta')])
    expect(
      screen.queryByRole('button', { name: /Remove .* from the comparison/ }),
    ).toBeNull()
    expect(
      screen.queryByRole('button', { name: 'Clear comparison' }),
    ).toBeNull()
    first.unmount()

    const onRemove = vi.fn()
    const onClear = vi.fn()
    await renderDialog([entry('vendor/beta')], { onRemove, onClear })

    expect(
      screen.getByRole('button', {
        name: 'Remove vendor/beta from the comparison',
      }),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Clear comparison' }),
    ).toBeTruthy()
  })
})
