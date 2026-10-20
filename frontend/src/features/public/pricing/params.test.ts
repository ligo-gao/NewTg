import { describe, expect, it } from 'vitest'

import {
  activeFilterCount,
  canonicalPricingSearch,
  capabilityList,
  clearedFilters,
  contextList,
  endpointTypeList,
  needsPricingUrlRepair,
  retainFacetValues,
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
      capability: '',
      context: '',
      endpointType: '',
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

  it('normalizes the new metadata filters as comma lists and repeated keys', () => {
    const search = validatePricingSearch({
      capability: 'Vision, text,vision',
      context: ['128k', '8k,128k'],
      endpointType: 'chat.completions,generateContent',
    })
    expect(search.capability).toBe('vision,text')
    expect(search.context).toBe('128k,8k')
    // 端点枚举统一小写，与服务端 facet 的 value 形态一致。
    expect(search.endpointType).toBe('chat.completions,generatecontent')
    expect(capabilityList(search)).toEqual(['vision', 'text'])
    expect(contextList(search)).toEqual(['128k', '8k'])
    expect(endpointTypeList(search)).toEqual(['chat.completions', 'generatecontent'])
  })

  it('drops unknown context buckets and endpoint enums but keeps valid zero-hit values', () => {
    const search = validatePricingSearch({
      // 8k/200k+ 是冻结枚举：目录暂无命中也必须保留（服务端会返回空集），
      // 16k / chat 之类不在枚举里的拼写直接丢弃。
      context: '8k,16k,200K+',
      endpointType: 'messages,chat,images.generations',
    })
    expect(search.context).toBe('8k,200k+')
    expect(search.endpointType).toBe('messages,images.generations')
  })

  it('keeps capability unknown to the client and lets the facets prune it later', () => {
    // capability 的合法域来自目录，validateSearch 无法静态判断。
    expect(validatePricingSearch({ capability: 'telepathy' }).capability).toBe(
      'telepathy',
    )
  })

  it('serializes the new filters into a canonical round trip', () => {
    const search = validatePricingSearch({
      vendor: 'openai',
      capability: 'vision',
      context: '128k',
      endpointType: 'messages',
    })
    const reparsed = validatePricingSearch(
      Object.fromEntries(new URLSearchParams(canonicalPricingSearch(search))),
    )
    expect(reparsed).toEqual(search)
    expect(needsPricingUrlRepair(`?${canonicalPricingSearch(search)}`, search)).toBe(
      false,
    )
    // 未知值被降级后，原始地址栏必须触发一次修复回写。
    expect(
      needsPricingUrlRepair('?context=16k', validatePricingSearch({ context: '16k' })),
    ).toBe(true)
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

  it('counts and clears the three metadata filter groups', () => {
    const search = validatePricingSearch({
      capability: 'vision,text',
      context: '8k,128k',
      endpointType: 'messages,rerank',
    })
    expect(activeFilterCount(search)).toBe(6)

    const cleared = clearedFilters(search)
    expect(cleared).toMatchObject({
      capability: '',
      context: '',
      endpointType: '',
    })
    expect(activeFilterCount(cleared)).toBe(0)
  })

  it('only prunes missing facet values when the result set is non-empty', () => {
    const known = new Set(['vision'])
    // 有结果：facet 候选集可作为合法域，缺失值剔除。
    expect(retainFacetValues(['vision', 'telepathy'], known, true)).toEqual([
      'vision',
    ])
    // 零结果：facet 只反映「其它筛选下的命中」，缺失不能判定为未知。
    expect(retainFacetValues(['vision', 'telepathy'], known, false)).toEqual([
      'vision',
      'telepathy',
    ])
  })

  it('detects every silently downgraded raw parameter (F-09-22)', () => {
    const base = validatePricingSearch({})
    // 规范化后的地址栏无需修正。
    expect(needsPricingUrlRepair(`?${canonicalPricingSearch(base)}`, base)).toBe(false)
    // page_size 非法（服务端只认 24/48/96/20/50/100）。
    expect(needsPricingUrlRepair('?page_size=30', validatePricingSearch({ page_size: '30' }))).toBe(true)
    expect(needsPricingUrlRepair('?page_size=0', validatePricingSearch({ page_size: '0' }))).toBe(true)
    expect(needsPricingUrlRepair('?page_size=101', validatePricingSearch({ page_size: '101' }))).toBe(true)
    // 非布尔展示开关。
    expect(
      needsPricingUrlRepair(
        '?dualCurrency=yes',
        validatePricingSearch({ dualCurrency: 'yes' }),
      ),
    ).toBe(true)
    // 搜索前后空白。
    expect(
      needsPricingUrlRepair('?search=%20gpt%20', validatePricingSearch({ search: ' gpt ' })),
    ).toBe(true)
    // vendor 大小写归一。
    expect(
      needsPricingUrlRepair('?vendor=OpenAI', validatePricingSearch({ vendor: 'OpenAI' })),
    ).toBe(true)
  })
})
