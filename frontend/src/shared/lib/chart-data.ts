/**
 * Chart contract shared by every analytics surface (web-00 §10, modules-05
 * §0.6). M6 ships no chart library (R-01), so this module is the stable seam:
 * a future renderer receives the same `ChartSeries` / `ChartConfig` shapes and
 * must not change them.
 *
 * The server always sends UTC ISO timestamps. Bucketing therefore uses UTC
 * calendar fields — never the viewer's local timezone — because a local-day
 * implementation moves points to the previous or next bucket for viewers east
 * or west of UTC, which is exactly the month-end / year-end misalignment
 * Review Focus 3 calls out.
 */

export type ChartPoint = { ts: string /* UTC ISO */; value: number }

export type ChartSeries = {
  id: string
  label: string
  points: ChartPoint[]
}

export type ChartConfig = {
  kind: 'line' | 'bar' | 'area'
  unit: string
}

export type Granularity = 'hour' | 'day' | 'month'

const HOUR_MS = 3_600_000

function parseTimestamp(ts: string): number {
  const ms = Date.parse(ts)
  if (Number.isNaN(ms)) {
    throw new TypeError(`bucketSeries received an invalid timestamp: ${ts}`)
  }
  return ms
}

function assertFiniteValue(value: number): void {
  // A single NaN or Infinity would poison the whole bucket sum; rejecting it
  // up front is what lets the contract promise "never NaN" (task ruling R-05).
  if (!Number.isFinite(value)) {
    throw new RangeError(
      `bucketSeries received a non-finite value: ${String(value)}`,
    )
  }
}

/**
 * Truncates an instant to the start of its UTC hour, day or month.
 * `Date.UTC` is used for day/month so the result is independent of the host
 * timezone (constructing a local `Date` and zeroing fields would not be).
 */
function bucketStart(ms: number, granularity: Granularity): number {
  const date = new Date(ms)
  switch (granularity) {
    case 'hour':
      return Math.floor(ms / HOUR_MS) * HOUR_MS
    case 'day':
      return Date.UTC(
        date.getUTCFullYear(),
        date.getUTCMonth(),
        date.getUTCDate(),
      )
    case 'month':
      return Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1)
  }
}

/**
 * Groups points into UTC buckets and emits every series over the union of all
 * buckets, ascending by bucket start. Missing buckets are filled with `0` so
 * the renderer can zip series by index without a second alignment pass.
 *
 * Invalid timestamps throw `TypeError`; non-finite values throw `RangeError`.
 * Throwing (rather than returning a typed error) keeps the success path a
 * plain `ChartSeries[]`, which is what the renderer consumes.
 */
export function bucketSeries(
  series: ChartSeries[],
  granularity: Granularity,
): ChartSeries[] {
  if (series.length === 0) return []

  const bucketed = series.map((item) =>
    item.points.map((point) => {
      assertFiniteValue(point.value)
      return {
        bucket: bucketStart(parseTimestamp(point.ts), granularity),
        value: point.value,
      }
    }),
  )

  const buckets = [
    ...new Set(bucketed.flatMap((points) => points.map((point) => point.bucket))),
  ].sort((a, b) => a - b)

  return series.map((item, index) => {
    const sums = new Map<number, number>()
    for (const point of bucketed[index] ?? []) {
      sums.set(point.bucket, (sums.get(point.bucket) ?? 0) + point.value)
    }
    return {
      id: item.id,
      label: item.label,
      points: buckets.map((bucket) => ({
        ts: new Date(bucket).toISOString(),
        value: sums.get(bucket) ?? 0,
      })),
    }
  })
}

/**
 * Locale-aware axis label (web-00 §10). `Intl` localises the wording and number
 * formatting, but `timeZone: 'UTC'` is mandatory: the labels annotate UTC
 * buckets, and rendering them in the host timezone would move a day bucket
 * across month-end (e.g. `2026-01-01T00:00Z` → "Dec 31" at UTC-5) — the exact
 * label-layer misalignment Review Focus 3 forbids. Invalid input is returned
 * unchanged, mirroring `formatRelativeTime` and keeping a malformed upstream
 * value visible instead of blanking the axis.
 */
export function formatAxis(ts: string, locale: string): string {
  const ms = Date.parse(ts)
  if (Number.isNaN(ms)) return ts
  return new Intl.DateTimeFormat(locale, {
    timeZone: 'UTC',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(ms))
}
