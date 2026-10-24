import { describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'

import {
  capabilitySlots,
  contextBucketOf,
  facetsOf,
  kindForEndpoints,
  kindOf,
  monogram,
} from './taxonomy'

function card(
  overrides: Partial<PublicModelCard> & { model_id: string },
): PublicModelCard {
  return {
    display_name: overrides.model_id,
    vendor: '',
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

describe('capabilitySlots', () => {
  it('counts mixed-case capabilities once per model and keeps the first spelling', () => {
    const slots = capabilitySlots([
      card({ model_id: 'a', capabilities: ['Vision', 'vision', 'TEXT'] }),
      card({ model_id: 'b', capabilities: ['vision', 'text', 'Unknown_Cap'] }),
    ])

    expect(slots).toEqual([
      { value: 'text', label: 'TEXT', count: 2 },
      { value: 'vision', label: 'Vision', count: 2 },
      { value: 'unknown_cap', label: 'Unknown_Cap', count: 1 },
    ])
  })

  it('sorts by count descending and breaks ties by value ascending', () => {
    const slots = capabilitySlots([
      card({ model_id: 'a', capabilities: ['b', 'a'] }),
      card({ model_id: 'b', capabilities: ['b', 'c'] }),
      card({ model_id: 'c', capabilities: ['c'] }),
    ])

    expect(slots.map((slot) => [slot.value, slot.count])).toEqual([
      ['b', 2],
      ['c', 2],
      ['a', 1],
    ])
  })

  it('keeps unknown capabilities and trims their display label', () => {
    const slots = capabilitySlots([
      card({ model_id: 'a', capabilities: ['  Vision  ', '', 'brand_new'] }),
    ])

    expect(slots).toEqual([
      { value: 'brand_new', label: 'brand_new', count: 1 },
      { value: 'vision', label: 'Vision', count: 1 },
    ])
  })

  it('truncates to 12 by default while preserving the shared order', () => {
    const capabilities = Array.from({ length: 15 }, (_, i) =>
      `cap_${String(i).padStart(2, '0')}`,
    )
    const slots = capabilitySlots([card({ model_id: 'a', capabilities })])

    expect(slots).toHaveLength(12)
    expect(slots.map((slot) => slot.value)).toEqual(capabilities.slice(0, 12))
    expect(capabilitySlots([card({ model_id: 'a', capabilities })], 2)).toHaveLength(2)
  })
})

describe('facetsOf', () => {
  it('keeps every capability value and counts only valid vendor/context/endpoint enums', () => {
    const capabilities = Array.from({ length: 15 }, (_, i) => `cap_${i}`)
    const facets = facetsOf([
      card({
        model_id: 'a',
        vendor: 'OpenAI',
        capabilities,
        context_length: 8_000,
        endpoints: ['chat.completions', 'embeddings'],
      }),
      card({
        model_id: 'b',
        vendor: '   ',
        context_length: 20_000,
        endpoints: ['embeddings', 'not.an.endpoint'],
      }),
      card({
        model_id: 'c',
        vendor: 'openai',
        context_length: null,
        endpoints: ['CHAT.COMPLETIONS', 'chat.completions'],
      }),
    ])

    // The facet list is not truncated: the top-12 bar uses capabilitySlots.
    expect(facets.capabilities).toHaveLength(15)
    expect(facets.vendors).toEqual([{ value: 'openai', count: 2 }])
    expect(facets.contexts).toEqual([
      { value: '32k', count: 1 },
      { value: '8k', count: 1 },
    ])
    expect(facets.endpoints).toEqual([
      { value: 'chat.completions', count: 2 },
      { value: 'embeddings', count: 2 },
    ])
  })
})

describe('kindForEndpoints / kindOf', () => {
  it('maps each endpoint family and falls back to chat', () => {
    expect(kindForEndpoints([])).toBe('chat')
    expect(kindForEndpoints(['chat.completions'])).toBe('chat')
    expect(kindForEndpoints(['messages'])).toBe('chat')
    expect(kindForEndpoints(['generateContent'])).toBe('chat')
    expect(kindForEndpoints(['embeddings'])).toBe('embedding')
    expect(kindForEndpoints(['images.generations'])).toBe('image')
    expect(kindForEndpoints(['audio.speech'])).toBe('speech')
    expect(kindForEndpoints(['audio.transcriptions'])).toBe('transcription')
    expect(kindForEndpoints(['rerank'])).toBe('rerank')
  })

  it('prefers a specific endpoint over the chat fallback', () => {
    expect(kindForEndpoints(['chat.completions', 'embeddings'])).toBe('embedding')
    expect(kindForEndpoints(['chat.completions', 'rerank'])).toBe('rerank')
    expect(kindForEndpoints(['images.generations', 'embeddings'])).toBe('image')
  })

  it('classifies a card from its endpoint list', () => {
    expect(kindOf(card({ model_id: 'x', endpoints: ['embeddings'] }))).toBe(
      'embedding',
    )
    expect(kindOf(card({ model_id: 'y', endpoints: [] }))).toBe('chat')
  })
})

describe('contextBucketOf', () => {
  it('maps every boundary to the frozen bucket set', () => {
    expect(contextBucketOf(null)).toBeNull()
    expect(contextBucketOf(0)).toBe('8k')
    expect(contextBucketOf(8_192)).toBe('8k')
    expect(contextBucketOf(8_193)).toBe('32k')
    expect(contextBucketOf(32_768)).toBe('32k')
    expect(contextBucketOf(32_769)).toBe('128k')
    expect(contextBucketOf(131_072)).toBe('128k')
    expect(contextBucketOf(131_073)).toBe('200k+')
  })
})

describe('monogram', () => {
  it('uses the first two letters of the vendor uppercased', () => {
    expect(monogram('OpenAI', 'GPT-4o')).toBe('OP')
    expect(monogram('deepseek', 'DeepSeek-V3')).toBe('DE')
  })

  it('falls back to the display name and takes one CJK character', () => {
    expect(monogram('', 'GPT-4o')).toBe('GP')
    expect(monogram('   ', 'Claude')).toBe('CL')
    expect(monogram(String.fromCodePoint(0x667a, 0x8c31), 'GLM-4')).toBe(
      String.fromCodePoint(0x667a),
    )
    expect(monogram('', '')).toBe('')
  })
})
