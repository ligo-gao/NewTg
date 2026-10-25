import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { CompareDialog } from './compare-dialog'
import type { ModelEntry, ModelPerformance } from './performance'

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
): ModelEntry {
  return {
    card: card(modelId),
    kind: 'chat',
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
