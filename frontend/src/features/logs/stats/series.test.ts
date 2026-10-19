import { describe, expect, it } from 'vitest'

import { DEFAULT_USAGE_LOG_SEARCH } from '../search'
import { fillTimeseries } from './series'

const NOW = Date.parse('2026-10-19T12:34:00Z')
const HOUR_MS = 3_600_000

function hoursAgo(hours: number): string {
  const bucket = Math.floor((NOW - hours * HOUR_MS) / HOUR_MS) * HOUR_MS
  return new Date(bucket).toISOString()
}

describe('usage stats timeseries spine', () => {
  it('returns an empty list for an empty response so the UI shows its empty state', () => {
    expect(
      fillTimeseries([], { ...DEFAULT_USAGE_LOG_SEARCH, range: '24h' }, 'hour', NOW),
    ).toEqual([])
  })

  it('fills missing interior UTC buckets with zeros', () => {
    const rows = fillTimeseries(
      [
        { bucket: hoursAgo(3), cost: 10, requests: 1 },
        { bucket: hoursAgo(0), cost: 30, requests: 3 },
      ],
      { ...DEFAULT_USAGE_LOG_SEARCH, range: '24h' },
      'hour',
      NOW,
    )

    expect(rows).toHaveLength(24)
    // 12:34Z rounds down to 12:00Z; the returned rows run from the start of
    // the 24h window to the current hour.
    expect(rows.at(-1)?.bucket).toBe('2026-10-19T12:00:00.000Z')
    const filled = rows.find((row) => row.bucket === hoursAgo(2))
    expect(filled).toMatchObject({ cost: 0, requests: 0 })
    expect(rows.find((row) => row.bucket === hoursAgo(3))?.cost).toBe(10)
  })

  it('keeps the partial first bucket that starts before the computed window', () => {
    // Backend window is [now - 24h, now), which is not bucket-aligned: its
    // first hourly bucket starts one hour before `end - 23h`. Dropping it
    // would make the chart Σcost disagree with the summary card.
    const rows = fillTimeseries(
      [{ bucket: hoursAgo(24), cost: 107, requests: 1 }],
      { ...DEFAULT_USAGE_LOG_SEARCH, range: '24h' },
      'hour',
      NOW,
    )

    expect(rows[0]?.bucket).toBe(hoursAgo(24))
    expect(rows.reduce((sum, row) => sum + row.cost, 0)).toBe(107)
  })

  it('uses the custom window and day buckets when asked', () => {
    const rows = fillTimeseries(
      [
        { bucket: '2026-10-10T00:00:00Z', cost: 5, requests: 1 },
        { bucket: '2026-10-13T00:00:00Z', cost: 7, requests: 1 },
      ],
      {
        ...DEFAULT_USAGE_LOG_SEARCH,
        range: 'custom',
        start: '2026-10-10T00:00:00Z',
        end: '2026-10-14T00:00:00Z',
      },
      'day',
      NOW,
    )

    expect(rows.map((row) => row.bucket)).toEqual([
      '2026-10-10T00:00:00.000Z',
      '2026-10-11T00:00:00.000Z',
      '2026-10-12T00:00:00.000Z',
      '2026-10-13T00:00:00.000Z',
    ])
    expect(rows[1]).toMatchObject({ cost: 0, requests: 0 })
  })
})
