import { describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'

import type { MetricsItem } from './api'
import { LOW_SAMPLE_THRESHOLD, entriesOf, performanceOf } from './performance'

type LatestTest = NonNullable<MetricsItem['latest_test']>

function metrics(overrides: Partial<MetricsItem> = {}): MetricsItem {
  return {
    model_id: 'openai/gpt-4o',
    summary: { sample_size: 0 },
    latest_test: null,
    ...overrides,
  }
}

function latestTest(overrides: Partial<LatestTest> = {}): LatestTest {
  return {
    ttft_ms_p50: null,
    ttft_ms_p95: null,
    latency_ms_p50: null,
    latency_ms_p95: null,
    tps: null,
    success_rate: 1,
    sample_size: 100,
    tested_at: '2026-10-24T00:00:00Z',
    ...overrides,
  }
}

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

describe('performanceOf: measured test source wins', () => {
  it('uses test P50/P95 and TTFT for chat', () => {
    const perf = performanceOf(
      'chat',
      metrics({
        latest_test: latestTest({
          ttft_ms_p50: 320,
          ttft_ms_p95: 610,
          latency_ms_p50: 900,
          latency_ms_p95: 1400,
          tps: 42,
          success_rate: 0.987,
          sample_size: 120,
        }),
      }),
    )

    expect(perf.source).toBe('test')
    expect(perf.measure).toBe('ttft')
    expect(perf.p50).toBe(320)
    expect(perf.p95).toBe(610)
    expect(perf.tps).toBe(42)
    expect(perf.successRate).toBe(0.987)
    expect(perf.samples).toBe(120)
    expect(perf.lowSample).toBe(false)
    expect(perf.testedAt).toBe('2026-10-24T00:00:00Z')
    expect(perf.hasData).toBe(true)
  })

  it('falls back to test latency when TTFT percentiles are missing', () => {
    const perf = performanceOf(
      'chat',
      metrics({
        latest_test: latestTest({
          ttft_ms_p50: null,
          ttft_ms_p95: null,
          latency_ms_p50: 850,
          latency_ms_p95: 1200,
        }),
      }),
    )

    expect(perf.source).toBe('test')
    expect(perf.measure).toBe('latency')
    expect(perf.p50).toBe(850)
    expect(perf.p95).toBe(1200)

    const missingP95 = performanceOf(
      'chat',
      metrics({
        latest_test: latestTest({
          ttft_ms_p50: 300,
          ttft_ms_p95: null,
        }),
      }),
    )

    // A missing percentile stays null; the P50 must never stand in for it.
    expect(missingP95.measure).toBe('ttft')
    expect(missingP95.p95).toBeNull()
  })
})

describe('performanceOf: usage summary fallback', () => {
  it('uses the usage average for chat and never invents a P95', () => {
    const perf = performanceOf(
      'chat',
      metrics({
        summary: {
          sample_size: 40,
          avg_ttft_ms: 400,
          avg_latency_ms: 910,
          tps: 30,
          success_rate: 0.98,
        },
      }),
    )

    expect(perf.source).toBe('usage')
    expect(perf.measure).toBe('ttft')
    expect(perf.p50).toBe(400)
    expect(perf.p95).toBeNull()
    expect(perf.tps).toBe(30)
    expect(perf.successRate).toBe(0.98)
    expect(perf.samples).toBe(40)
    expect(perf.testedAt).toBeNull()
    expect(perf.hasData).toBe(true)
  })

  it('uses latency averages for non-chat kinds', () => {
    const perf = performanceOf(
      'embedding',
      metrics({
        // TTFT is chat-only; a test row without latency percentiles cannot
        // satisfy the selection rule for non-chat kinds.
        latest_test: latestTest({ ttft_ms_p50: 90 }),
        summary: {
          sample_size: 12,
          avg_ttft_ms: 110,
          avg_latency_ms: 240,
        },
      }),
    )

    expect(perf.source).toBe('usage')
    expect(perf.measure).toBe('latency')
    expect(perf.p50).toBe(240)
    expect(perf.p95).toBeNull()
  })

  it('prefers test data over usage and labels the source', () => {
    const perf = performanceOf(
      'chat',
      metrics({
        summary: { sample_size: 400, avg_ttft_ms: 900, avg_latency_ms: 1500 },
        latest_test: latestTest({ ttft_ms_p50: 250, ttft_ms_p95: 480 }),
      }),
    )

    expect(perf.source).toBe('test')
    expect(perf.p50).toBe(250)
    expect(perf.p95).toBe(480)
    expect(perf.samples).toBe(100)
  })
})

describe('performanceOf: missing data', () => {
  it('returns source none for a missing item', () => {
    const perf = performanceOf('chat', undefined)

    expect(perf.source).toBe('none')
    expect(perf.measure).toBe('ttft')
    expect(perf.p50).toBeNull()
    expect(perf.p95).toBeNull()
    expect(perf.tps).toBeNull()
    expect(perf.successRate).toBeNull()
    expect(perf.samples).toBe(0)
    expect(perf.lowSample).toBe(false)
    expect(perf.testedAt).toBeNull()
    expect(perf.hasData).toBe(false)
  })

  it('marks runs under 20 samples as low sample', () => {
    expect(LOW_SAMPLE_THRESHOLD).toBe(20)

    const short = performanceOf(
      'chat',
      metrics({ latest_test: latestTest({ ttft_ms_p50: 300, sample_size: 19 }) }),
    )
    expect(short.lowSample).toBe(true)

    const enough = performanceOf(
      'chat',
      metrics({ latest_test: latestTest({ ttft_ms_p50: 300, sample_size: 20 }) }),
    )
    expect(enough.lowSample).toBe(false)
  })

  it('hasData is false when the source has no usable average', () => {
    const perf = performanceOf(
      'chat',
      metrics({
        summary: { sample_size: 10, avg_ttft_ms: null, avg_latency_ms: null },
        latest_test: latestTest({ tps: 55 }),
      }),
    )

    expect(perf.source).toBe('none')
    expect(perf.p50).toBeNull()
    expect(perf.p95).toBeNull()
    expect(perf.tps).toBeNull()
    expect(perf.successRate).toBeNull()
    expect(perf.samples).toBe(10)
    expect(perf.hasData).toBe(false)
    expect(perf.lowSample).toBe(false)
  })
})

describe('entriesOf', () => {
  it('joins each card with its indexed metrics and classified kind', () => {
    const cards = [
      card({ model_id: 'openai/gpt-4o', endpoints: ['chat.completions'] }),
      card({ model_id: 'acme/embed', endpoints: ['embeddings'] }),
    ]
    const index = new Map<string, MetricsItem>([
      [
        'openai/gpt-4o',
        metrics({
          model_id: 'openai/gpt-4o',
          latest_test: latestTest({ ttft_ms_p50: 210, ttft_ms_p95: 500 }),
        }),
      ],
    ])

    const entries = entriesOf(cards, index)

    expect(entries).toHaveLength(2)
    expect(entries[0]?.card.model_id).toBe('openai/gpt-4o')
    expect(entries[0]?.kind).toBe('chat')
    expect(entries[0]?.perf.source).toBe('test')
    expect(entries[0]?.perf.p50).toBe(210)
    expect(entries[1]?.kind).toBe('embedding')
    expect(entries[1]?.perf.source).toBe('none')
  })
})
