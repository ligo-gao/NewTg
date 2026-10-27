import { describe, expect, it } from 'vitest'

import { describeDiscount, discountFold } from './format'

describe('describeDiscount', () => {
  it('reports a missing rate as none', () => {
    expect(describeDiscount(null)).toEqual({ kind: 'none' })
    expect(describeDiscount(undefined)).toEqual({ kind: 'none' })
    expect(describeDiscount(Number.NaN)).toEqual({ kind: 'none' })
    expect(describeDiscount(Number.POSITIVE_INFINITY)).toEqual({ kind: 'none' })
  })

  it('renders the charged percentage rather than a fold or off label', () => {
    expect(describeDiscount(0.9)).toEqual({ kind: 'percent', value: '90' })
    expect(describeDiscount(0.855)).toEqual({ kind: 'percent', value: '85.5' })
    expect(describeDiscount(0.01)).toEqual({ kind: 'percent', value: '1' })
  })

  it('keeps the free and full-price ends of the scale distinct', () => {
    // 0 is a granted full waiver, not a missing value.
    expect(describeDiscount(0)).toEqual({ kind: 'percent', value: '0' })
    expect(describeDiscount(1)).toEqual({ kind: 'percent', value: '100' })
  })

  it('clamps out-of-range ratios to 0..1', () => {
    expect(describeDiscount(-0.5)).toEqual({ kind: 'percent', value: '0' })
    expect(describeDiscount(1.5)).toEqual({ kind: 'percent', value: '100' })
  })

  it('leaves the fold helper used by the discount editor untouched', () => {
    expect(discountFold(0.01)).toBe('0.1')
  })
})
