import { fireEvent, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { ModelHero } from './hero'
import type { ModelEntry, ModelPerformance } from './performance'
import type { CapabilitySlot } from './taxonomy'

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
  name,
  vendor,
  capabilities,
  perf,
}: {
  id: string
  name: string
  vendor: string
  capabilities: string[]
  perf: Partial<ModelPerformance>
}): ModelEntry {
  const card: PublicModelCard = {
    model_id: id,
    display_name: name,
    vendor,
    capabilities,
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
  return { card, kind: 'chat', perf: performance(perf) }
}

const ENTRIES: readonly ModelEntry[] = [
  entry({
    id: 'openai/gpt-5',
    name: 'GPT-5',
    vendor: 'openai',
    capabilities: ['vision', 'tool_use'],
    perf: { source: 'test', p50: 350, hasData: true, samples: 200 },
  }),
  entry({
    id: 'anthropic/claude',
    name: 'Claude',
    vendor: 'anthropic',
    capabilities: ['vision', 'reasoning'],
    perf: {},
  }),
  entry({
    id: 'meta/llama',
    name: 'Llama',
    vendor: '',
    capabilities: ['text'],
    perf: { source: 'usage', p50: 900, hasData: true, samples: 40 },
  }),
]

const SLOTS: readonly CapabilitySlot[] = [
  { value: 'vision', label: 'vision', count: 2 },
  { value: 'text', label: 'text', count: 1 },
]

function statValue(label: HTMLElement): string {
  return label.closest('div')?.querySelector('dd')?.textContent?.trim() ?? ''
}

function renderHero(activeCapability = '', onPickCapability = vi.fn()) {
  return renderWithProviders(
    <ModelHero
      entries={ENTRIES}
      slots={SLOTS}
      activeCapability={activeCapability}
      onPickCapability={onPickCapability}
    />,
    ['public'],
  )
}

describe('ModelHero', () => {
  it('counts models, vendors, measured entries and capability values', async () => {
    const { getByText } = await renderHero()

    expect(statValue(getByText('Models'))).toBe('3')
    expect(statValue(getByText('Vendors'))).toBe('2')
    expect(statValue(getByText('With measurements'))).toBe('2')
    // vision + tool_use + reasoning + text; duplicates across models count once.
    expect(statValue(getByText('Capabilities'))).toBe('4')
  })

  it('names the fastest measured model with its latency', async () => {
    const { getByText } = await renderHero()

    expect(getByText('350 ms')).toBeTruthy()
    expect(getByText('GPT-5')).toBeTruthy()
  })

  it('marks the active capability slot with aria-pressed', async () => {
    const { getByRole } = await renderHero('vision')

    const bar = getByRole('group', { name: 'Capabilities' })
    const buttons = within(bar).getAllByRole('button')
    // Order and length come from slots, not from the 12-slot catalogue cap.
    expect(buttons.map((button) => button.textContent)).toEqual(['vision', 'text'])
    expect(buttons[0]?.getAttribute('aria-pressed')).toBe('true')
    expect(buttons[1]?.getAttribute('aria-pressed')).toBe('false')
  })

  it('reports the picked capability', async () => {
    const onPickCapability = vi.fn()
    const { getByRole } = await renderHero('', onPickCapability)

    fireEvent.click(getByRole('button', { name: 'text' }))

    expect(onPickCapability).toHaveBeenCalledWith('text')
  })
})
