import { describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'

import { formatRmb, formatRmbRange, priceOf, pricePerMillion } from './price'

function card(overrides: Partial<PublicModelCard> = {}): PublicModelCard {
  return {
    model_id: 'openai/gpt-4o',
    display_name: 'GPT-4o',
    vendor: 'openai',
    capabilities: [],
    context_length: null,
    max_output: null,
    mode: 'per_token',
    prompt_price: null,
    completion_price: null,
    cache_read_price: null,
    cache_write_price: null,
    is_free: false,
    group_price: null,
    pricing_schedule: null,
    endpoints: [],
    tags: [],
    description: '',
    ...overrides,
  }
}

describe('pricePerMillion', () => {
  it('converts a per-million credit string to yuan with quota_per_unit', () => {
    expect(pricePerMillion('30000', 500000)).toBe(0.06)
    expect(pricePerMillion('2000000', 500000)).toBe(4)
    expect(pricePerMillion('0', 500000)).toBe(0)
  })

  it('returns null when the price or the quota unit is unusable', () => {
    expect(pricePerMillion(null, 500000)).toBeNull()
    expect(pricePerMillion('abc', 500000)).toBeNull()
    expect(pricePerMillion('30000', undefined)).toBeNull()
    expect(pricePerMillion('30000', 0)).toBeNull()
  })

  it('keeps a huge int64 price finite', () => {
    expect(Number.isFinite(pricePerMillion('1000000000000000000', 1))).toBe(true)
  })
})

describe('priceOf', () => {
  it('maps the three price fields and keeps missing prices null', () => {
    const priced = priceOf(
      card({
        model_id: 'priced',
        prompt_price: '30000',
        completion_price: '60000',
        cache_read_price: '15000',
      }),
      500000,
    )
    expect(priced).toEqual({
      input: 0.06,
      output: 0.12,
      cacheRead: 0.03,
    })

    expect(priceOf(card(), 500000)).toEqual({
      input: null,
      output: null,
      cacheRead: null,
    })
    expect(priceOf(card({ prompt_price: '30000' }), undefined)).toEqual({
      input: null,
      output: null,
      cacheRead: null,
    })
  })

  it('treats per_request prompt_price as a per-call price', () => {
    // 30000 credits per call at 500000 credits per yuan = ¥0.06 per call.
    // completion/cache fields are meaningless in this mode and must stay null.
    const priced = priceOf(
      card({
        mode: 'per_request',
        prompt_price: '30000',
        completion_price: '60000',
        cache_read_price: '15000',
      }),
      500000,
    )
    expect(priced).toEqual({
      input: 0.06,
      output: null,
      cacheRead: null,
      perCall: true,
    })
  })

  it('renders no prices for a mode it does not understand', () => {
    expect(
      priceOf(
        card({
          mode: 'per_batch',
          prompt_price: '30000',
          completion_price: '60000',
        }),
        500000,
      ),
    ).toEqual({
      input: null,
      output: null,
      cacheRead: null,
    })
  })
})

describe('formatRmb', () => {
  it('formats yuan with two decimals and a range', () => {
    expect(formatRmb(0.06, 'zh-CN')).toBe('¥0.06')
    expect(formatRmb(null, 'zh-CN')).toBeNull()
    expect(formatRmbRange(0.06, 4, 'zh-CN')).toBe('¥0.06–¥4.00')
    expect(formatRmbRange(4, 4, 'zh-CN')).toBe('¥4.00')
    expect(formatRmbRange(0.06, null, 'zh-CN')).toBeNull()
  })

  it('collapses a range whose endpoints format identically', () => {
    // ¥0.0002 and ¥0.0018 are different inputs but both print ¥0.00; a
    // `¥0.00–¥0.00` range would invent a spread that readers cannot see.
    expect(formatRmbRange(0.0002, 0.0018, 'en')).toBe('¥0.00')
  })
})
