import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { DistPanel } from './dist-panel'
import type { ModelEntry, ModelPerformance } from './performance'

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

function entry({
  id,
  kind,
  p50 = null,
  source = 'test',
}: {
  id: string
  kind: ModelEntry['kind']
  p50?: number | null
  source?: ModelPerformance['source']
}): ModelEntry {
  const card: PublicModelCard = {
    model_id: id,
    display_name: id,
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
  }
  return {
    card,
    kind,
    perf: performance({
      source,
      p50,
      hasData: p50 !== null,
      measure: kind === 'chat' ? 'ttft' : 'latency',
    }),
  }
}

async function renderPanel(entries: ModelEntry[]) {
  return renderWithProviders(<DistPanel entries={entries} range="24h" />, [
    'public',
  ])
}

describe('DistPanel', () => {
  it('only counts chat models with a first-token reading', async () => {
    await renderPanel([
      entry({ id: 'chat/test', kind: 'chat', p50: 300 }),
      entry({ id: 'chat/usage', kind: 'chat', p50: 600, source: 'usage' }),
      // An embedding's end-to-end latency must not enter the first-token bins.
      entry({ id: 'acme/embed', kind: 'embedding', p50: 120 }),
      // A chat model without a reading contributes to the spread only.
      entry({ id: 'chat/empty', kind: 'chat', p50: null }),
    ])

    expect(
      screen.getByRole('img', {
        name: 'First-token P50 distribution for 2 streaming models under the current filter, median 450 ms',
      }),
    ).toBeTruthy()
    expect(
      screen.getByText(
        '4 models in this filter · 2 with streaming samples',
      ),
    ).toBeTruthy()
    // The embedding's 120 ms would land here if the kind filter were missing.
    expect(screen.getByTitle('<250 ms: 0 models')).toBeTruthy()
    expect(screen.getByTitle('250–499 ms: 1 models')).toBeTruthy()
    expect(screen.getByTitle('500–999 ms: 1 models')).toBeTruthy()
  })

  it('renders the median and the spread copy', async () => {
    await renderPanel([
      entry({ id: 'a', kind: 'chat', p50: 100 }),
      entry({ id: 'b', kind: 'chat', p50: 200 }),
      entry({ id: 'c', kind: 'chat', p50: 300 }),
    ])

    expect(screen.getByText('200 ms')).toBeTruthy()
    expect(
      screen.getByText('3 models in this filter · 3 with streaming samples'),
    ).toBeTruthy()
    expect(screen.getByText('First-token P50 median')).toBeTruthy()
  })

  it('renders the empty state without a zero reading', async () => {
    await renderPanel([
      entry({ id: 'chat/empty', kind: 'chat', p50: null }),
      entry({ id: 'acme/embed', kind: 'embedding', p50: 120 }),
    ])

    expect(
      screen.getByRole('img', {
        name: 'No streaming samples under the current filter.',
      }),
    ).toBeTruthy()
    expect(
      screen.getAllByText('No streaming samples under the current filter.')
        .length,
    ).toBeGreaterThan(0)
    // No sample is `—`, never a fabricated `0 ms`.
    expect(screen.getByText('—')).toBeTruthy()
    expect(screen.queryByText('0 ms')).toBeNull()
    expect(screen.getByText('First-token P50 median')).toBeTruthy()
  })
})
