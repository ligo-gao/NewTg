import { describe, expect, it } from 'vitest'

import { formatMs, formatSamples } from './format'

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

  it('groups digits with the requested locale', () => {
    const expected = new Intl.NumberFormat('fr', {
      maximumFractionDigits: 0,
    }).format(2_340)
    expect(formatMs(2_340, 'fr')).toBe(`${expected} ms`)
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
