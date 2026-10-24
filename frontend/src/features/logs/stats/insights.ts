/**
 * Derived metrics for the usage-stats trend and token-mix panels.
 *
 * Every function here is pure: the stats tab maps the server rows into
 * `TrendPoint`s once and these helpers turn them into the numbers the cards,
 * chart and rail render. All shares and averages are rounded to two decimals
 * so the UI never shows a long float tail; percentages of an empty window are
 * `0` rather than `NaN`.
 */

export interface TrendPoint {
  bucket: string
  cost: number
  requests: number
}

export interface TrendInsights {
  peak: TrendPoint | null
  /** peak.cost / totalCost × 100, two decimals; 0 when totalCost is 0. */
  peakShare: number
  bucketCount: number
  /** Buckets with traffic (`requests > 0`) — "有流量时段". */
  activeBuckets: number
  /** Buckets with no spend (`cost === 0`) — the spend-pane zero note. */
  zeroBuckets: number
  /** totalCost / bucketCount, two decimals; 0 for an empty window. */
  averageCost: number
  totalCost: number
}

function round2(value: number): number {
  return Math.round(value * 100) / 100
}

export function trendInsights(rows: TrendPoint[]): TrendInsights {
  const bucketCount = rows.length
  const totalCost = rows.reduce((sum, row) => sum + row.cost, 0)

  // `>` (not `>=`) keeps the earlier bucket when two buckets tie on cost.
  let peak: TrendPoint | null = null
  for (const row of rows) {
    if (peak === null || row.cost > peak.cost) peak = row
  }

  // Activity and spend are deliberately different measures. `activeBuckets`
  // counts traffic, so a refused-only hour (requests but no billable spend)
  // still counts as active; `zeroBuckets` counts spend, so that same hour is
  // a zero bucket for the spend-pane note. The two need not be complementary.
  const zeroBuckets = rows.filter((row) => row.cost === 0).length
  const activeBuckets = rows.filter((row) => row.requests > 0).length

  return {
    peak,
    peakShare:
      totalCost === 0 || peak === null ? 0 : round2((peak.cost / totalCost) * 100),
    bucketCount,
    activeBuckets,
    zeroBuckets,
    averageCost: bucketCount === 0 ? 0 : round2(totalCost / bucketCount),
    totalCost,
  }
}

/**
 * The busiest buckets by cost for the rail's "活跃时段" list. Activity means
 * traffic (`requests > 0`), not spend: a bucket can have requests and no cost
 * (all refused, or a free model) and still belongs in the list, ranked by the
 * cost it did spend.
 */
export function topActiveBuckets(rows: TrendPoint[], limit?: number): TrendPoint[] {
  return rows
    .filter((row) => row.requests > 0)
    .sort((a, b) => b.cost - a.cost)
    .slice(0, limit)
}

export function tokenSplit(input: {
  prompt: number
  completion: number
  cache: number
  requests: number
}): {
  total: number
  promptPct: number
  completionPct: number
  cachePct: number
  avgPerRequest: number
  /** prompt / completion, two decimals; null when completion is 0. */
  inputToOutput: number | null
} {
  const total = input.prompt + input.completion + input.cache
  const share = (part: number): number =>
    total === 0 ? 0 : round2((part / total) * 100)

  return {
    total,
    promptPct: share(input.prompt),
    completionPct: share(input.completion),
    cachePct: share(input.cache),
    avgPerRequest:
      input.requests === 0 ? 0 : Math.round(total / input.requests),
    inputToOutput:
      input.completion === 0 ? null : round2(input.prompt / input.completion),
  }
}

/**
 * Axis tick label for a UTC bucket. `timeZone: 'UTC'` is mandatory for the
 * same reason as `formatAxis` in `@/shared/lib/chart-data`: a day bucket
 * labelled in the viewer's timezone slides across month-end. Malformed input
 * is returned unchanged so a bad value stays visible instead of blanking the
 * axis.
 */
export function formatBucketTick(
  ts: string,
  granularity: 'hour' | 'day',
  locale: string,
): string {
  const ms = Date.parse(ts)
  if (Number.isNaN(ms)) return ts

  const options: Intl.DateTimeFormatOptions =
    granularity === 'hour'
      ? { timeZone: 'UTC', hour12: false, hour: '2-digit', minute: '2-digit' }
      : { timeZone: 'UTC', month: 'short', day: 'numeric' }

  return new Intl.DateTimeFormat(locale, options).format(new Date(ms))
}
