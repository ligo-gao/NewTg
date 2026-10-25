import { describe, expect, it } from 'vitest'

import {
  formatMs,
  formatPercent,
  formatSamples,
  formatTPS,
  latencyHistogram,
  latencyStripGeom,
  median,
} from './format'

describe('formatMs', () => {
  it('renders null as an em dash', () => {
    expect(formatMs(null)).toBe('—')
  })

  it('rounds milliseconds below ten seconds', () => {
    expect(formatMs(349.6)).toBe('350 ms')
    expect(formatMs(0)).toBe('0 ms')
  })

  it('switches to seconds with one decimal at ten seconds', () => {
    expect(formatMs(12_400)).toBe('12.4 s')
    expect(formatMs(10_000)).toBe('10.0 s')
  })
})

describe('formatTPS', () => {
  it('renders null as an em dash', () => {
    expect(formatTPS(null)).toBe('—')
  })

  it('keeps one decimal below ten and rounds above', () => {
    expect(formatTPS(8.44)).toBe('8.4 t/s')
    expect(formatTPS(12.7)).toBe('13 t/s')
  })
})

describe('formatPercent', () => {
  it('renders null as an em dash and one decimal otherwise', () => {
    expect(formatPercent(null)).toBe('—')
    expect(formatPercent(0.998)).toBe('99.8%')
    expect(formatPercent(1)).toBe('100.0%')
    expect(formatPercent(0)).toBe('0.0%')
  })
})

describe('formatSamples', () => {
  it('renders zero samples as zero, not an em dash', () => {
    expect(formatSamples(0, 'en')).toBe('0')
  })

  it('groups below ten thousand and compacts above', () => {
    expect(formatSamples(9_999, 'en')).toBe('9,999')
    expect(formatSamples(10_000, 'en')).toBe('10K')
    expect(formatSamples(1_200_000, 'en')).toBe('1.2M')
  })
})

describe('latencyStripGeom', () => {
  it('returns a marker only when P95 is missing or P50 is missing', () => {
    expect(latencyStripGeom(null, null)).toEqual({
      leftPct: 0,
      widthPct: 0,
      markerOnly: true,
    })
    expect(latencyStripGeom(1_000, null)).toEqual({
      leftPct: 25,
      widthPct: 0,
      markerOnly: true,
    })
  })

  it('draws the P50–P95 band on the fixed 0–4000 ms scale', () => {
    expect(latencyStripGeom(1_000, 2_000)).toEqual({
      leftPct: 25,
      widthPct: 25,
      markerOnly: false,
    })
    expect(latencyStripGeom(0, 4_000)).toEqual({
      leftPct: 0,
      widthPct: 100,
      markerOnly: false,
    })
  })

  it('never leaves the 0–100 range for long latencies or inverted bands', () => {
    expect(latencyStripGeom(5_000, 8_000)).toEqual({
      leftPct: 100,
      widthPct: 0,
      markerOnly: false,
    })
    expect(latencyStripGeom(3_900, 8_000)).toEqual({
      leftPct: 97.5,
      widthPct: 2.5,
      markerOnly: false,
    })
    expect(latencyStripGeom(2_000, 1_000)).toEqual({
      leftPct: 50,
      widthPct: 0,
      markerOnly: false,
    })
  })
})

describe('latencyHistogram', () => {
  it('counts the four fixed buckets', () => {
    expect(latencyHistogram([])).toEqual([0, 0, 0, 0])
    expect(
      latencyHistogram([100, 249, 250, 499, 500, 999, 1_000, 5_000]),
    ).toEqual([2, 2, 2, 2])
  })
})

describe('median', () => {
  it('returns null for an empty list and averages even counts', () => {
    expect(median([])).toBeNull()
    expect(median([5])).toBe(5)
    expect(median([1, 3])).toBe(2)
    expect(median([1, 2, 3])).toBe(2)
    expect(median([4, 1, 3, 2])).toBe(2.5)
  })

  it('does not mutate the input', () => {
    const values = [3, 1, 2]
    expect(median(values)).toBe(2)
    expect(values).toEqual([3, 1, 2])
  })
})
