import { describe, expect, it } from 'vitest'

import {
  activeFilterCount,
  clearedFilters,
  validatePricingSearch,
  vendorList,
  withVendors,
} from './params'

describe('pricing URL contract (F-09-22)', () => {
  it('falls back to documented defaults for an empty URL', () => {
    expect(validatePricingSearch({})).toEqual({
      p: 1,
      page_size: 24,
      search: '',
      sort: 'name',
      vendor: '',
      group: '',
      quotaType: 'all',
      view: 'card',
      tokenUnit: 'M',
      dualCurrency: false,
      rechargePrice: false,
    })
  })

  it('downgrades invalid p/page_size/sort/quotaType per field', () => {
    const search = validatePricingSearch({
      p: 'abc',
      page_size: 33,
      sort: 'foo',
      quotaType: 'bogus',
      view: 'nope',
    })
    expect(search.p).toBe(1)
    expect(search.page_size).toBe(24)
    expect(search.sort).toBe('name')
    expect(search.quotaType).toBe('all')
    expect(search.view).toBe('card')
  })

  it('uses the table density default and accepts both view size sets', () => {
    expect(validatePricingSearch({ view: 'table' }).page_size).toBe(50)
    expect(validatePricingSearch({ view: 'table', page_size: '24' }).page_size).toBe(24)
    expect(validatePricingSearch({ page_size: '96' }).page_size).toBe(96)
  })

  it('normalizes vendor from comma lists and repeated keys', () => {
    expect(validatePricingSearch({ vendor: 'OpenAI, anthropic,openai' }).vendor).toBe(
      'openai,anthropic',
    )
    expect(validatePricingSearch({ vendor: ['openai', 'x,y'] }).vendor).toBe(
      'openai,x,y',
    )
    expect(vendorList(validatePricingSearch({ vendor: 'a,b' }))).toEqual(['a', 'b'])
    expect(withVendors(['b', 'a', 'b'])).toBe('b,a')
  })

  it('truncates search to 128 characters', () => {
    expect(validatePricingSearch({ search: 'x'.repeat(200) }).search).toHaveLength(128)
  })

  it('counts active filters and clears only filters, keeping display preferences', () => {
    const search = validatePricingSearch({
      search: 'gpt',
      vendor: 'openai',
      group: 'vip',
      quotaType: 'token',
      view: 'table',
      tokenUnit: 'K',
      dualCurrency: '1',
      p: 3,
    })
    expect(activeFilterCount(search)).toBe(4)
    expect(clearedFilters(search)).toMatchObject({
      p: 1,
      search: '',
      vendor: '',
      group: '',
      quotaType: 'all',
      view: 'table',
      tokenUnit: 'K',
      dualCurrency: true,
    })
  })
})
