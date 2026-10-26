import { describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { renderWithProviders } from '@/test-support/providers'

import { ModelHero } from './hero'
import type { ModelEntry, ModelPerformance } from './performance'
import type { ModelPrice } from './price'

function performance(
  overrides: Partial<ModelPerformance> = {},
): ModelPerformance {
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
  price,
  perf,
}: {
  id: string
  name: string
  price?: Partial<ModelPrice>
  perf?: Partial<ModelPerformance>
}): ModelEntry {
  const card: PublicModelCard = {
    model_id: id,
    display_name: name,
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
  }
  return {
    card,
    kind: 'chat',
    price: { input: null, output: null, cacheRead: null, ...price },
    perf: performance(perf),
  }
}

/** 三个条目：一个实测首字 + 两端输入价，两个没有读数。 */
const ENTRIES: readonly ModelEntry[] = [
  entry({
    id: 'openai/gpt-5',
    name: 'GPT-5',
    price: { input: 0.06 },
    perf: { source: 'test', p50: 2340, hasData: true, samples: 200 },
  }),
  entry({
    id: 'acme/embed',
    name: 'Embed',
    price: { input: 4 },
    perf: { measure: 'latency' },
  }),
  entry({ id: 'meta/llama', name: 'Llama' }),
]

function statValue(label: HTMLElement): string {
  return label.closest('div')?.querySelector('dd')?.textContent?.trim() ?? ''
}

function renderHero(entries: readonly ModelEntry[] = ENTRIES) {
  return renderWithProviders(<ModelHero entries={entries} />, ['public'])
}

describe('ModelHero', () => {
  it('renders exactly four stat cells', async () => {
    const { container, getByText } = await renderHero()

    expect(container.querySelectorAll('dd')).toHaveLength(4)
    expect(statValue(getByText('Models'))).toBe('3')
    expect(statValue(getByText('With measurements'))).toBe('1')

    const fastest = statValue(getByText('Fastest P50'))
    expect(fastest).toContain('2,340 ms')
    expect(fastest).toContain('openai/gpt-5')

    expect(statValue(getByText('Input price / million tokens'))).toBe(
      '¥0.06–¥4.00',
    )
  })

  it('renders an em dash when no entry has a price', async () => {
    const { getByText, queryByText } = await renderHero([
      entry({ id: 'acme/embed', name: 'Embed' }),
    ])

    expect(statValue(getByText('Input price / million tokens'))).toBe('—')
    expect(queryByText('¥0.00')).toBeNull()
  })

  it('omits the fastest model id when nothing is measured', async () => {
    const { getByText, queryByText } = await renderHero([
      entry({ id: 'meta/llama', name: 'Llama' }),
    ])

    expect(statValue(getByText('Fastest P50'))).toBe('—')
    expect(queryByText('meta/llama')).toBeNull()
  })

  it('only considers first-token readings for the fastest cell', async () => {
    // 端到端延迟不是首字延迟：即便数值更小，也不能冒充最快首字。
    const { getByText, queryByText } = await renderHero([
      entry({
        id: 'acme/embed',
        name: 'Embed',
        perf: { source: 'usage', measure: 'latency', p50: 90, hasData: true },
      }),
    ])

    expect(statValue(getByText('Fastest P50'))).toBe('—')
    expect(queryByText('acme/embed')).toBeNull()
  })

  it('picks the smallest P50 among several first-token readings', async () => {
    const { getByText } = await renderHero([
      entry({
        id: 'slow/model',
        name: 'Slow',
        perf: { source: 'test', p50: 900, hasData: true },
      }),
      entry({
        id: 'fast/model',
        name: 'Fast',
        perf: { source: 'test', p50: 200, hasData: true },
      }),
    ])

    const fastest = statValue(getByText('Fastest P50'))
    expect(fastest).toContain('200 ms')
    expect(fastest).toContain('fast/model')
    expect(fastest).not.toContain('900 ms')
  })

  it('keeps per-call prices out of the per-million input range', async () => {
    const { getByText } = await renderHero([
      entry({ id: 'token/model', name: 'Token', price: { input: 0.06 } }),
      entry({
        id: 'call/model',
        name: 'Call',
        price: { input: 100, perCall: true },
      }),
    ])

    // ¥100 per call is not ¥100 per million tokens; mixing them would publish
    // a range in which the two ends have different units.
    expect(statValue(getByText('Input price / million tokens'))).toBe('¥0.06')
  })

  it('does not render a capability chip row', async () => {
    const { queryAllByRole, queryByRole } = await renderHero()

    expect(queryByRole('group', { name: 'Capabilities' })).toBeNull()
    // 页头唯一允许出现的可交互元素是页面级操作，不是筛选 chip。
    expect(queryAllByRole('button')).toEqual([])
  })
})
