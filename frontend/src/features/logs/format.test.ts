import { describe, expect, it } from 'vitest'

import { describeDiscount, discountFold, discountRateHint } from './format'

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

describe('discountRateHint', () => {
  it('uses the fold form for Chinese and Japanese', () => {
    expect(discountRateHint(90, 'zh-CN')).toEqual({ kind: 'fold', value: '9' })
    expect(discountRateHint(90, 'zh-TW')).toEqual({ kind: 'fold', value: '9' })
    expect(discountRateHint(90, 'ja')).toEqual({ kind: 'fold', value: '9' })
  })

  it('uses the charged percentage for every other locale', () => {
    for (const locale of ['en', 'fr', 'ru', 'vi']) {
      expect(discountRateHint(90, locale)).toEqual({
        kind: 'percent',
        value: '90',
      })
    }
  })

  it('keeps a fractional rate expressed in each unit', () => {
    // `discountFold(0.855)` lands on "8.55"; assert the observed value rather
    // than a rounded guess.
    expect(discountRateHint(85.5, 'zh-CN')).toEqual({
      kind: 'fold',
      value: '8.55',
    })
    expect(discountRateHint(85.5, 'en')).toEqual({
      kind: 'percent',
      value: '85.5',
    })
  })

  it('handles both ends of the scale', () => {
    expect(discountRateHint(0, 'zh-CN')).toEqual({ kind: 'fold', value: '0' })
    expect(discountRateHint(100, 'zh-CN')).toEqual({ kind: 'fold', value: '10' })
    expect(discountRateHint(0, 'en')).toEqual({ kind: 'percent', value: '0' })
    expect(discountRateHint(100, 'en')).toEqual({
      kind: 'percent',
      value: '100',
    })
  })

  it('falls back to an em dash for non-finite input', () => {
    expect(discountRateHint(Number.NaN, 'en')).toEqual({
      kind: 'percent',
      value: '—',
    })
  })
})
