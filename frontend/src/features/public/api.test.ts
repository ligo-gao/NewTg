import { describe, expect, it } from 'vitest'

import { normalizeModelMetrics } from './api'

const VALID = {
  summary: {
    avg_latency_ms: 820,
    success_rate: 0.997,
    tps: 42.5,
    failed_24h: 12,
    sample_size: 1543,
  },
  per_group: [
    { group: 'default', avg_ttft_ms: 210, avg_latency_ms: 800, success_rate: 0.998 },
  ],
}

describe('normalizeModelMetrics (F-09-25 data-field table)', () => {
  it('accepts null no-data values and preserves valid numbers', () => {
    const normalized = normalizeModelMetrics({
      summary: {
        avg_latency_ms: null,
        success_rate: null,
        tps: null,
        failed_24h: null,
        sample_size: 0,
      },
      per_group: [
        { group: 'default', avg_ttft_ms: null, avg_latency_ms: null, success_rate: null },
      ],
    })

    expect(normalized.summary.avg_latency_ms).toBeNull()
    expect(normalized.per_group[0]?.avg_ttft_ms).toBeNull()
    expect(normalizeModelMetrics(VALID).summary.success_rate).toBe(0.997)
  })

  it.each([
    ['negative latency', { summary: { ...VALID.summary, avg_latency_ms: -1 } }],
    ['success rate above 1', { summary: { ...VALID.summary, success_rate: 1.5 } }],
    ['negative success rate', { summary: { ...VALID.summary, success_rate: -0.1 } }],
    ['negative tps', { summary: { ...VALID.summary, tps: -0.5 } }],
    ['negative failed count', { summary: { ...VALID.summary, failed_24h: -1 } }],
    ['negative sample size', { summary: { ...VALID.summary, sample_size: -1 } }],
    [
      'negative group ttft',
      {
        per_group: [
          { group: 'default', avg_ttft_ms: -5, avg_latency_ms: 800, success_rate: 0.9 },
        ],
      },
    ],
    [
      'group success rate above 1',
      {
        per_group: [
          { group: 'default', avg_ttft_ms: 100, avg_latency_ms: 800, success_rate: 1.01 },
        ],
      },
    ],
  ])('rejects out-of-range values: %s', (_name, patch) => {
    expect(() => normalizeModelMetrics({ ...VALID, ...patch })).toThrow(
      /out of range/,
    )
  })
})
