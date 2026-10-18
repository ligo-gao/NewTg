import { describe, expect, it } from 'vitest'

import {
  formatCredits,
  formatCurrencyApprox,
  formatTokenAmount,
} from './format'

describe('formatCredits', () => {
  it('formats an integer with thousands separators and no decimals', () => {
    expect(formatCredits(1000000)).toBe('1,000,000')
  })

  it('formats zero', () => {
    expect(formatCredits(0)).toBe('0')
  })

  it('rejects negative values (the caller adds U+2212)', () => {
    expect(() => formatCredits(-5)).toThrow()
  })

  it('rejects non-integer values (credits are always integers)', () => {
    expect(() => formatCredits(1.5)).toThrow()
  })
})

describe('formatTokenAmount', () => {
  it('abbreviates values at or above one million with one decimal', () => {
    expect(formatTokenAmount(1200000)).toBe('1.2M')
  })

  it('formats values below the threshold with grouping', () => {
    expect(formatTokenAmount(999999)).toBe('999,999')
  })

  it('applies the abbreviation threshold to the rounded value', () => {
    // 999,999.6 would otherwise print as the grouped "1,000,000".
    expect(formatTokenAmount(999999.6)).toBe('1.0M')
    expect(formatTokenAmount(999999.4)).toBe('999,999')
  })

  it('keeps one decimal for an exact million', () => {
    expect(formatTokenAmount(1000000)).toBe('1.0M')
  })
})

describe('formatCurrencyApprox', () => {
  it('renders two decimals with the approximation prefix', () => {
    expect(formatCurrencyApprox(12.345)).toBe('≈12.35')
  })

  it('always renders two decimals', () => {
    expect(formatCurrencyApprox(12)).toBe('≈12.00')
  })
})
