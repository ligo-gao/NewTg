import { describe, expect, it } from 'vitest'

import {
  DEFAULT_REDEMPTION_PAGE_SIZE,
  hasRedemptionFilters,
  normalizeRedemptionSearch,
} from './redemption-search'

describe('normalizeRedemptionSearch', () => {
  it('keeps a whitelisted page size so refresh/share preserves the table shape', () => {
    expect(normalizeRedemptionSearch({ p: '2', size: '50' })).toEqual({
      p: 2,
      size: 50,
    })
    expect(normalizeRedemptionSearch({ size: 10 })).toEqual({ p: 1, size: 10 })
  })

  it('drops sizes outside the frozen 10/20/50 list instead of forwarding them', () => {
    // 契约本身允许 1–100，但下拉只提供三档；其他值渲染时会得到一个
    // 下拉框里不存在的选中项，因此在入口就降级为默认值。
    expect(normalizeRedemptionSearch({ size: '25' }).size).toBeUndefined()
    expect(normalizeRedemptionSearch({ size: '0' }).size).toBeUndefined()
    expect(normalizeRedemptionSearch({ size: 'abc' }).size).toBeUndefined()
    expect(normalizeRedemptionSearch({ size: '-10' }).size).toBeUndefined()
  })

  it('defaults an absent page size to 20', () => {
    expect(normalizeRedemptionSearch({}).size ?? DEFAULT_REDEMPTION_PAGE_SIZE).toBe(20)
  })

  it('normalizes the remaining filters and flags when any is active', () => {
    const search = normalizeRedemptionSearch({
      p: '3',
      status: 'expired',
      batch: '  launch  ',
      min: '10',
      max: '20',
    })

    expect(search).toEqual({
      p: 3,
      status: 'expired',
      batch: 'launch',
      min: 10,
      max: 20,
    })
    expect(hasRedemptionFilters(search)).toBe(true)
    expect(hasRedemptionFilters({ p: 1, size: 50 })).toBe(false)
  })
})
