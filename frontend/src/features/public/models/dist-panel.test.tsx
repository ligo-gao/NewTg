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
  measure = 'ttft',
}: {
  id: string
  kind: ModelEntry['kind']
  p50?: number | null
  source?: ModelPerformance['source']
  measure?: ModelPerformance['measure']
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
    price: { input: null, output: null, cacheRead: null },
    perf: performance({
      source,
      p50,
      hasData: p50 !== null,
      measure,
    }),
  }
}

async function renderPanel(entries: ModelEntry[], ready = true) {
  return renderWithProviders(
    <DistPanel entries={entries} range="24h" ready={ready} />,
    ['public'],
  )
}

describe('DistPanel', () => {
  it('only counts chat models with a first-token reading', async () => {
    await renderPanel([
      entry({ id: 'chat/test', kind: 'chat', p50: 300 }),
      // Usage source with measure 'ttft' is the mean first-token latency, which
      // still describes first-token behaviour and must be counted.
      entry({
        id: 'chat/usage',
        kind: 'chat',
        p50: 600,
        source: 'usage',
        measure: 'ttft',
      }),
      // End-to-end latency (measure 'latency') must be excluded even for chat.
      entry({
        id: 'chat/e2e',
        kind: 'chat',
        p50: 400,
        measure: 'latency',
      }),
      // An embedding's end-to-end latency must not enter the first-token bins.
      entry({
        id: 'acme/embed',
        kind: 'embedding',
        p50: 120,
        measure: 'latency',
      }),
      // A chat model without a reading contributes to the spread only.
      entry({ id: 'chat/empty', kind: 'chat', p50: null }),
    ])

    expect(
      screen.getByRole('img', {
        name: 'Median first-token latency across 2 chat models under the current filter: 450 ms',
      }),
    ).toBeTruthy()
    expect(
      screen.getByText(
        '5 models in this filter · 2 contribute first-token readings',
      ),
    ).toBeTruthy()
    // The embedding's 120 ms and the chat end-to-end 400 ms would land here if
    // only the kind filter were applied.
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
      screen.getByText(
        '3 models in this filter · 3 contribute first-token readings',
      ),
    ).toBeTruthy()
    expect(screen.getByText('Median first-token latency')).toBeTruthy()
  })

  it('renders the empty state without a zero reading', async () => {
    await renderPanel([
      entry({ id: 'chat/empty', kind: 'chat', p50: null }),
      entry({ id: 'acme/embed', kind: 'embedding', p50: 120 }),
      entry({ id: 'chat/e2e', kind: 'chat', p50: 400, measure: 'latency' }),
    ])

    expect(
      screen.getByRole('img', {
        name: 'No first-token readings under the current filter.',
      }),
    ).toBeTruthy()
    expect(
      screen.getAllByText('No first-token readings under the current filter.')
        .length,
    ).toBeGreaterThan(0)
    // No sample is `—`, never a fabricated `0 ms`.
    expect(screen.getByText('—')).toBeTruthy()
    expect(screen.queryByText('0 ms')).toBeNull()
    expect(screen.getByText('Median first-token latency')).toBeTruthy()
  })

  it('stays out of the way until the catalog is ready', async () => {
    const { container } = await renderPanel(
      [entry({ id: 'chat/empty', kind: 'chat', p50: null })],
      false,
    )

    expect(container.textContent).toBe('')
    expect(
      screen.queryByText('No first-token readings under the current filter.'),
    ).toBeNull()
  })
})
