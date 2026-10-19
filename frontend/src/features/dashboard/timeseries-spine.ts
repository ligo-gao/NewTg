import type { ChartPoint } from '@/shared/lib/chart-data'

import type { UsageGranularity, UsageRange } from './api'

/**
 * The backend only returns buckets that exist and delegates zero-fill to the
 * frontend (handlers_usage_logs.go). A trend drawn from those rows alone would
 * connect hours/days with no traffic and shrink the x-axis to the first/last
 * active bucket. This module materialises the full UTC bucket spine for the
 * selected window, then merges the server points into it.
 */
const HOUR_MS = 3_600_000
const DAY_MS = 86_400_000

const RANGE_BUCKETS: Record<UsageRange, number> = {
  '24h': 24,
  '7d': 7,
  '14d': 14,
  '30d': 30,
}

// Stray server buckets (clock skew) are merged into the spine, but only within
// a bounded distance from the selected window: a corrupt far-past/far-future
// timestamp must not allocate hundreds of thousands of points.
const MAX_STRAY_HOURS = 168
const MAX_STRAY_DAYS = 7

function stepMs(granularity: UsageGranularity): number {
  return granularity === 'hour' ? HOUR_MS : DAY_MS
}

/**
 * Returns bucket-aligned points covering the whole selected window plus any
 * server bucket outside it (clock skew between the browser and the server must
 * not drop real data). Missing buckets within the spine are emitted as `0`,
 * never as gaps. UTC alignment keeps a day bucket from drifting across a
 * month/year boundary for viewers outside UTC.
 */
export function fillBucketSpine(
  points: ChartPoint[],
  range: UsageRange,
  granularity: UsageGranularity,
  now: number = Date.now(),
): ChartPoint[] {
  const step = stepMs(granularity)
  const end = Math.floor(now / step) * step
  const count = RANGE_BUCKETS[range]
  const windowStart = end - (count - 1) * step

  const sums = new Map<number, number>()
  let firstServerBucket: number | undefined
  let lastServerBucket: number | undefined
  for (const point of points) {
    const ms = Date.parse(point.ts)
    if (Number.isNaN(ms) || !Number.isFinite(point.value)) continue
    const bucket = Math.floor(ms / step) * step
    sums.set(bucket, (sums.get(bucket) ?? 0) + point.value)
    firstServerBucket =
      firstServerBucket === undefined ? bucket : Math.min(firstServerBucket, bucket)
    lastServerBucket =
      lastServerBucket === undefined ? bucket : Math.max(lastServerBucket, bucket)
  }

  const strayBuckets = granularity === 'hour' ? MAX_STRAY_HOURS : MAX_STRAY_DAYS
  const clamp = (value: number, low: number, high: number): number =>
    Math.min(Math.max(value, low), high)
  const start = Math.min(
    windowStart,
    clamp(
      firstServerBucket ?? windowStart,
      windowStart - strayBuckets * step,
      end + strayBuckets * step,
    ),
  )
  const finish = Math.max(
    end,
    clamp(
      lastServerBucket ?? end,
      windowStart - strayBuckets * step,
      end + strayBuckets * step,
    ),
  )

  const filled: ChartPoint[] = []
  for (let bucket = start; bucket <= finish; bucket += step) {
    filled.push({
      ts: new Date(bucket).toISOString(),
      value: sums.get(bucket) ?? 0,
    })
  }
  return filled
}
