import type { components } from '@/api/schema'

import { usageRangeParams, type UsageLogRange, type UsageLogSearch } from '../search'

export type UsageStatsTimeseriesItem =
  components['schemas']['UsageStatsTimeseriesItem']
export type StatsGranularity = 'hour' | 'day'

const HOUR_MS = 3_600_000
const DAY_MS = 86_400_000

const RANGE_BUCKETS: Record<UsageLogRange, number> = {
  '24h': 24,
  '7d': 7,
  '14d': 14,
  '30d': 30,
}

// Bounded clamp for server buckets outside the computed window (clock skew or
// a partially-covered first bucket). A corrupt far-past/far-future timestamp
// must not allocate a huge spine.
const MAX_STRAY_HOURS = 168
const MAX_STRAY_DAYS = 7

function stepMs(granularity: StatsGranularity): number {
  return granularity === 'hour' ? HOUR_MS : DAY_MS
}

function bucketStart(ms: number, step: number): number {
  return Math.floor(ms / step) * step
}

/**
 * Materialises the UTC bucket spine for the selected window and merges the
 * server's sparse rows into it (F-05-03 §3.4 / R-M6-2).
 *
 * Why the frontend fills the gaps: `/api/usage-stats/timeseries` returns only
 * buckets that exist. Drawing those rows directly would connect hours with no
 * traffic as if they were adjacent, and would shrink the x-axis to the first
 * and last active bucket. Every bucket in the window is therefore emitted,
 * missing ones as `0`. Bucketing uses UTC calendar arithmetic — a local-time
 * implementation would move a day bucket across month/year boundaries for
 * viewers outside UTC.
 *
 * The window is computed from the same `range`/`start`/`end` the request was
 * sent with, but the server's window edge is not bucket-aligned: for a 24h
 * preset the first row can belong to the hour *before* `end - 23h`. Those real
 * buckets are merged into the spine (clamped to a bounded stray distance, the
 * same rule as the dashboard trend) so the chart's Σcost equals the summary
 * card instead of dropping the partial first bucket.
 *
 * An empty response stays empty (the caller renders the empty state) instead
 * of a flat zero line that looks like measured data.
 */
export function fillTimeseries(
  items: UsageStatsTimeseriesItem[],
  search: UsageLogSearch,
  granularity: StatsGranularity,
  now: number = Date.now(),
): UsageStatsTimeseriesItem[] {
  if (items.length === 0) return []

  const step = stepMs(granularity)
  const range = usageRangeParams(search)
  let windowStart: number
  let windowEnd: number

  if ('start' in range) {
    windowStart = bucketStart(Date.parse(range.start), step)
    // The backend window is [start, end): when `end` falls exactly on a
    // bucket boundary, the bucket starting at `end` holds no row.
    windowEnd = bucketStart(Date.parse(range.end) - 1, step)
  } else {
    const count = RANGE_BUCKETS[range.range]
    windowEnd = bucketStart(now, step)
    windowStart = windowEnd - (count - 1) * step
  }
  if (
    !Number.isFinite(windowStart) ||
    !Number.isFinite(windowEnd) ||
    windowEnd < windowStart
  ) {
    return []
  }

  const sums = new Map<number, { cost: number; requests: number }>()
  let firstServerBucket: number | undefined
  let lastServerBucket: number | undefined
  for (const item of items) {
    const ms = Date.parse(item.bucket)
    if (Number.isNaN(ms)) continue
    const bucket = bucketStart(ms, step)
    const current = sums.get(bucket) ?? { cost: 0, requests: 0 }
    current.cost += Number.isFinite(item.cost) ? item.cost : 0
    current.requests += Number.isFinite(item.requests) ? item.requests : 0
    sums.set(bucket, current)
    firstServerBucket =
      firstServerBucket === undefined
        ? bucket
        : Math.min(firstServerBucket, bucket)
    lastServerBucket =
      lastServerBucket === undefined
        ? bucket
        : Math.max(lastServerBucket, bucket)
  }

  const strayBuckets = granularity === 'hour' ? MAX_STRAY_HOURS : MAX_STRAY_DAYS
  const clamp = (value: number, low: number, high: number): number =>
    Math.min(Math.max(value, low), high)
  const start = Math.min(
    windowStart,
    clamp(
      firstServerBucket ?? windowStart,
      windowStart - strayBuckets * step,
      windowEnd + strayBuckets * step,
    ),
  )
  const finish = Math.max(
    windowEnd,
    clamp(
      lastServerBucket ?? windowEnd,
      windowStart - strayBuckets * step,
      windowEnd + strayBuckets * step,
    ),
  )

  const rows: UsageStatsTimeseriesItem[] = []
  for (let bucket = start; bucket <= finish; bucket += step) {
    const sum = sums.get(bucket)
    rows.push({
      bucket: new Date(bucket).toISOString(),
      cost: sum?.cost ?? 0,
      requests: sum?.requests ?? 0,
    })
  }
  return rows
}
