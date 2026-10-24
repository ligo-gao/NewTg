import { describe, expect, it } from 'vitest'

import { formatBucketTick, tokenSplit, topActiveBuckets, trendInsights } from './insights'

const ROWS = [
  { bucket: '2026-10-24T01:00:00Z', cost: 100, requests: 2 },
  { bucket: '2026-10-24T02:00:00Z', cost: 0, requests: 0 },
  { bucket: '2026-10-24T03:00:00Z', cost: 300, requests: 6 },
]

describe('trendInsights', () => {
  it('finds the peak, the zero buckets and the per-bucket average', () => {
    expect(trendInsights(ROWS)).toEqual({
      peak: { bucket: '2026-10-24T03:00:00Z', cost: 300, requests: 6 },
      peakShare: 75,
      bucketCount: 3,
      activeBuckets: 2,
      zeroBuckets: 1,
      averageCost: 133.33,
      totalCost: 400,
    })
  })

  it('returns a zeroed shape for an empty window instead of NaN', () => {
    expect(trendInsights([])).toEqual({
      peak: null, peakShare: 0, bucketCount: 0, activeBuckets: 0,
      zeroBuckets: 0, averageCost: 0, totalCost: 0,
    })
  })

  it('keeps peakShare at 0 when every bucket costs nothing', () => {
    const rows = [{ bucket: '2026-10-24T01:00:00Z', cost: 0, requests: 4 }]
    expect(trendInsights(rows).peakShare).toBe(0)
    expect(trendInsights(rows).peak).toEqual(rows[0])
  })

  it('counts a refused-only bucket as active but as a zero-spend bucket', () => {
    const rows = [
      { bucket: '2026-10-24T01:00:00Z', cost: 100, requests: 2 },
      { bucket: '2026-10-24T02:00:00Z', cost: 0, requests: 5 },
    ]
    const insights = trendInsights(rows)
    expect(insights.activeBuckets).toBe(2)
    expect(insights.zeroBuckets).toBe(1)
    expect(topActiveBuckets(rows)).toEqual([rows[0], rows[1]])
  })
})

describe('topActiveBuckets', () => {
  it('drops empty buckets and sorts by cost', () => {
    expect(topActiveBuckets(ROWS)).toEqual([ROWS[2], ROWS[0]])
    expect(topActiveBuckets(ROWS, 1)).toEqual([ROWS[2]])
  })
})

describe('tokenSplit', () => {
  it('splits input/output/cache and derives the per-request average', () => {
    expect(tokenSplit({ prompt: 42_900_000, completion: 7_700_000, cache: 0, requests: 11_284 })).toEqual({
      total: 50_600_000,
      promptPct: 84.78,
      completionPct: 15.22,
      cachePct: 0,
      avgPerRequest: 4484,
      inputToOutput: 5.57,
    })
  })

  it('reports a null ratio when there is no output', () => {
    expect(tokenSplit({ prompt: 10, completion: 0, cache: 0, requests: 1 }).inputToOutput).toBeNull()
    expect(tokenSplit({ prompt: 0, completion: 0, cache: 0, requests: 0 }).avgPerRequest).toBe(0)
  })
})

describe('formatBucketTick', () => {
  it('renders UTC clock time for hourly buckets and a date for daily buckets', () => {
    expect(formatBucketTick('2026-10-24T03:00:00Z', 'hour', 'en')).toBe('03:00')
    expect(formatBucketTick('2026-10-24T03:00:00Z', 'day', 'en')).toBe('Oct 24')
  })
})
