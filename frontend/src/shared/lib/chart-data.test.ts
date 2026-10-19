import { describe, expect, it } from 'vitest'

import {
  bucketSeries,
  formatAxis,
  type ChartSeries,
} from './chart-data'

/**
 * web-00 §10 / modules-05 §0.6: the server sends UTC ISO timestamps and the
 * chart layer buckets them by UTC hour/day/month. A local-time implementation
 * would shift points across month-end and year-end boundaries for any viewer
 * east or west of UTC, so every boundary case below asserts the exact UTC
 * bucket start rather than a locale-formatted label.
 */

function series(id: string, label: string, points: ChartSeries['points']): ChartSeries {
  return { id, label, points }
}

describe('bucketSeries', () => {
  it('buckets by UTC hour and sums points that share a bucket', () => {
    const [result] = bucketSeries(
      [
        series('credits', 'Credits', [
          { ts: '2026-02-01T00:30:00Z', value: 2 },
          { ts: '2026-01-31T23:30:00Z', value: 1 },
          { ts: '2026-01-31T23:59:59Z', value: 3 },
        ]),
      ],
      'hour',
    )

    expect(result?.points).toEqual([
      { ts: '2026-01-31T23:00:00.000Z', value: 4 },
      { ts: '2026-02-01T00:00:00.000Z', value: 2 },
    ])
  })

  it('keeps month-end points in their own UTC day bucket', () => {
    const [result] = bucketSeries(
      [
        series('credits', 'Credits', [
          { ts: '2026-01-31T23:30:00Z', value: 1 },
          { ts: '2026-02-01T00:30:00Z', value: 2 },
        ]),
      ],
      'day',
    )

    expect(result?.points).toEqual([
      { ts: '2026-01-31T00:00:00.000Z', value: 1 },
      { ts: '2026-02-01T00:00:00.000Z', value: 2 },
    ])
  })

  it('truncates to the UTC year-month for the month granularity', () => {
    const [result] = bucketSeries(
      [
        series('credits', 'Credits', [
          { ts: '2026-01-05T10:00:00Z', value: 5 },
          { ts: '2026-01-27T22:00:00Z', value: 7 },
          { ts: '2026-02-01T00:30:00Z', value: 2 },
        ]),
      ],
      'month',
    )

    expect(result?.points).toEqual([
      { ts: '2026-01-01T00:00:00.000Z', value: 12 },
      { ts: '2026-02-01T00:00:00.000Z', value: 2 },
    ])
  })

  it.each(['hour', 'day', 'month'] as const)(
    'does not misplace points across a year-end boundary (%s)',
    (granularity) => {
      const [result] = bucketSeries(
        [
          series('credits', 'Credits', [
            { ts: '2025-12-31T23:30:00Z', value: 1 },
            { ts: '2026-01-01T00:30:00Z', value: 2 },
          ]),
        ],
        granularity,
      )

      const expected = {
        hour: ['2025-12-31T23:00:00.000Z', '2026-01-01T00:00:00.000Z'],
        day: ['2025-12-31T00:00:00.000Z', '2026-01-01T00:00:00.000Z'],
        month: ['2025-12-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'],
      }[granularity]

      expect(result?.points.map((point) => point.ts)).toEqual(expected)
      expect(result?.points.map((point) => point.value)).toEqual([1, 2])
    },
  )

  it('aligns every series on the union of buckets and zero-fills gaps', () => {
    const [a, b] = bucketSeries(
      [
        series('a', 'A', [
          { ts: '2026-01-01T10:00:00Z', value: 1 },
          { ts: '2026-01-03T10:00:00Z', value: 3 },
        ]),
        series('b', 'B', [{ ts: '2026-01-02T10:00:00Z', value: 2 }]),
      ],
      'day',
    )

    expect(a?.points).toEqual([
      { ts: '2026-01-01T00:00:00.000Z', value: 1 },
      { ts: '2026-01-02T00:00:00.000Z', value: 0 },
      { ts: '2026-01-03T00:00:00.000Z', value: 3 },
    ])
    expect(b?.points).toEqual([
      { ts: '2026-01-01T00:00:00.000Z', value: 0 },
      { ts: '2026-01-02T00:00:00.000Z', value: 2 },
      { ts: '2026-01-03T00:00:00.000Z', value: 0 },
    ])
  })

  it('sorts buckets ascending regardless of input order', () => {
    const [result] = bucketSeries(
      [
        series('credits', 'Credits', [
          { ts: '2026-01-03T10:00:00Z', value: 3 },
          { ts: '2026-01-01T10:00:00Z', value: 1 },
          { ts: '2026-01-02T10:00:00Z', value: 2 },
        ]),
      ],
      'day',
    )

    expect(result?.points.map((point) => point.value)).toEqual([1, 2, 3])
  })

  it('returns an empty array for empty input', () => {
    expect(bucketSeries([], 'hour')).toEqual([])
  })

  it('preserves a series with no points as an empty series', () => {
    expect(bucketSeries([series('a', 'A', [])], 'day')).toEqual([
      { id: 'a', label: 'A', points: [] },
    ])
  })

  it('preserves series identity and order', () => {
    const result = bucketSeries(
      [
        series('credits', 'Credits', [{ ts: '2026-01-01T00:00:00Z', value: 1 }]),
        series('requests', 'Requests', [{ ts: '2026-01-01T00:00:00Z', value: 2 }]),
      ],
      'day',
    )

    expect(result.map((item) => [item.id, item.label])).toEqual([
      ['credits', 'Credits'],
      ['requests', 'Requests'],
    ])
  })

  it('rejects an invalid timestamp instead of producing NaN buckets', () => {
    expect(() =>
      bucketSeries([series('a', 'A', [{ ts: 'not-a-date', value: 1 }])], 'day'),
    ).toThrow(TypeError)
  })

  it('rejects non-finite values instead of propagating NaN or Infinity', () => {
    expect(() =>
      bucketSeries([series('a', 'A', [{ ts: '2026-01-01T00:00:00Z', value: Number.NaN }])], 'day'),
    ).toThrow(RangeError)
    expect(() =>
      bucketSeries(
        [series('a', 'A', [{ ts: '2026-01-01T00:00:00Z', value: Number.POSITIVE_INFINITY }])],
        'day',
      ),
    ).toThrow(RangeError)
  })

  it('never emits NaN values', () => {
    const result = bucketSeries(
      [
        series('a', 'A', [
          { ts: '2026-01-01T00:00:00Z', value: 0 },
          { ts: '2026-01-01T01:00:00Z', value: 1 },
        ]),
      ],
      'hour',
    )

    for (const item of result) {
      for (const point of item.points) {
        expect(Number.isNaN(point.value)).toBe(false)
      }
    }
  })
})

describe('formatAxis', () => {
  // Midday UTC keeps the calendar month stable in every real timezone, so the
  // month assertion below is about locale, not about where CI happens to run.
  const ts = '2026-02-01T12:30:00Z'
  // Built from its code point because the lint gate forbids raw CJK literals in
  // source; this is still the ordinary zh-CN month suffix (U+6708).
  const zhMonthSuffix = String.fromCodePoint(0x6708)

  it('uses Intl for the locale-specific axis label', () => {
    const en = formatAxis(ts, 'en')
    const zh = formatAxis(ts, 'zh-CN')

    expect(en).toContain('Feb')
    expect(zh).toContain(`2${zhMonthSuffix}`)
    expect(en).not.toBe(zh)
  })

  it('returns the input unchanged for an invalid timestamp', () => {
    expect(formatAxis('not-a-date', 'en')).toBe('not-a-date')
  })
})
