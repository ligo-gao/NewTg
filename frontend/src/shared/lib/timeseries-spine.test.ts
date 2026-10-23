import { describe, expect, it } from 'vitest'

import { fillBucketSpine } from './timeseries-spine'

const NOW = Date.parse('2026-10-18T12:34:00Z')

describe('fillBucketSpine', () => {
  it('emits the full 24h hourly window with zeros where there is no traffic', () => {
    const filled = fillBucketSpine(
      [
        { ts: '2026-10-18T01:00:00Z', value: 5 },
        { ts: '2026-10-18T05:00:00Z', value: 7 },
      ],
      '24h',
      'hour',
      NOW,
    )

    expect(filled).toHaveLength(24)
    expect(filled[0]?.ts).toBe('2026-10-17T13:00:00.000Z')
    expect(filled.at(-1)?.ts).toBe('2026-10-18T12:00:00.000Z')
    expect(filled.find((point) => point.ts === '2026-10-18T02:00:00.000Z')?.value).toBe(0)
    expect(filled.find((point) => point.ts === '2026-10-18T05:00:00.000Z')?.value).toBe(7)
  })

  it('uses UTC day buckets for wider ranges', () => {
    const filled = fillBucketSpine([], '7d', 'day', NOW)
    expect(filled).toHaveLength(7)
    expect(filled[0]?.ts).toBe('2026-10-12T00:00:00.000Z')
    expect(filled.at(-1)?.ts).toBe('2026-10-18T00:00:00.000Z')
    expect(filled.every((point) => point.value === 0)).toBe(true)
  })

  it('bounds stray out-of-window buckets so corrupt data cannot explode the spine', () => {
    const filled = fillBucketSpine(
      [
        { ts: '1970-01-01T00:00:00Z', value: 1 },
        { ts: '2099-01-01T00:00:00Z', value: 2 },
      ],
      '30d',
      'day',
      NOW,
    )
    // 30 window days + at most 7 stray days on each side.
    expect(filled.length).toBeLessThanOrEqual(44)
  })
})
