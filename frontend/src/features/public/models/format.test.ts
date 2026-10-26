import { describe, expect, it } from 'vitest'

import {
  formatMs,
  formatPercent,
  formatSamples,
  formatTPS,
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
