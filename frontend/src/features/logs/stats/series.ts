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
  let startMs: number
  let lastMs: number

  if ('start' in range) {
    startMs = bucketStart(Date.parse(range.start), step)
    // The backend window is [start, end): when `end` falls exactly on a
    // bucket boundary, the bucket starting at `end` holds no row.
    lastMs = bucketStart(Date.parse(range.end) - 1, step)
  } else {
    const count = RANGE_BUCKETS[range.range]
    lastMs = bucketStart(now, step)
    startMs = lastMs - (count - 1) * step
  }
  if (!Number.isFinite(startMs) || !Number.isFinite(lastMs) || lastMs < startMs) {
    return []
  }

  const sums = new Map<number, { cost: number; requests: number }>()
  let hasInWindow = false
  for (const item of items) {
    const ms = Date.parse(item.bucket)
    if (Number.isNaN(ms)) continue
    const bucket = bucketStart(ms, step)
    if (bucket < startMs || bucket > lastMs) continue
    hasInWindow = true
    const current = sums.get(bucket) ?? { cost: 0, requests: 0 }
    current.cost += Number.isFinite(item.cost) ? item.cost : 0
    current.requests += Number.isFinite(item.requests) ? item.requests : 0
    sums.set(bucket, current)
  }
  // Clock skew between browser and server can put every row outside the
  // computed window. The chart must show the empty state, not fabricated zeros.
  if (!hasInWindow) return []

  const rows: UsageStatsTimeseriesItem[] = []
  for (let bucket = startMs; bucket <= lastMs; bucket += step) {
    const sum = sums.get(bucket)
    rows.push({
      bucket: new Date(bucket).toISOString(),
      cost: sum?.cost ?? 0,
      requests: sum?.requests ?? 0,
    })
  }
  return rows
}
