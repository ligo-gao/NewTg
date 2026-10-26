import { describe, expect, it } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'

import {
  activeFilterCount,
  contextBucketOf,
  filterEntries,
  sortEntries,
} from './filters'
import type { ModelsSearch, ModelsSort } from './params'
import type { ModelEntry } from './performance'

function search(overrides: Partial<ModelsSearch> = {}): ModelsSearch {
  return {
    q: '',
    cap: '',
    vendor: '',
    context: '',
    endpoint: '',
    measured: false,
    sort: 'measured',
    range: '7d',
    view: 'table',
    compare: '',
    ...overrides,
  }
}

interface EntryOptions {
  model_id?: string
  display_name?: string
  vendor?: string
  capabilities?: string[]
  context_length?: number | null
  endpoints?: string[]
  hasData?: boolean
  p50?: number | null
  tps?: number | null
  successRate?: number | null
  input?: number | null
}

function entry(options: EntryOptions = {}): ModelEntry {
  const modelId = options.model_id ?? 'openai/gpt-4o'
  const contextLength = options.context_length ?? null
  const card: PublicModelCard = {
    model_id: modelId,
    display_name: options.display_name ?? modelId,
    vendor: options.vendor ?? '',
    capabilities: options.capabilities ?? [],
    context_length: contextLength,
    max_output: null,
    mode: 'per_token',
    prompt_price: null,
    completion_price: null,
    cache_read_price: null,
    cache_write_price: null,
    is_free: false,
    group_price: null,
    pricing_schedule: null,
    endpoints: options.endpoints ?? [],
    tags: [],
    description: '',
  }
  const p50 = options.p50 ?? null
  return {
    card,
    kind: 'chat',
    price: {
      input: options.input ?? null,
      output: null,
      cacheRead: null,
    },
    perf: {
      source: options.hasData === true ? 'test' : 'none',
      measure: 'ttft',
      p50,
      p95: null,
      tps: options.tps ?? null,
      successRate: options.successRate ?? null,
      samples: options.hasData === true ? 100 : 0,
      lowSample: false,
      testedAt: null,
      hasData: options.hasData ?? p50 !== null,
    },
  }
}

function ids(entries: readonly ModelEntry[]): string[] {
  return entries.map((item) => item.card.model_id)
}

describe('filterEntries', () => {
  it('requires every selected capability', () => {
    const entries = [
      entry({ model_id: 'both', capabilities: ['Vision', 'Text'] }),
      entry({ model_id: 'only-vision', capabilities: ['vision'] }),
      entry({ model_id: 'none', capabilities: [] }),
    ]

    expect(ids(filterEntries(entries, search({ cap: 'vision,text' })))).toEqual([
      'both',
    ])
  })

  it('matches any selected vendor, context or endpoint', () => {
    const vendors = [
      entry({ model_id: 'oai', vendor: 'OpenAI' }),
      entry({ model_id: 'goog', vendor: 'Google' }),
      entry({ model_id: 'anth', vendor: 'Anthropic' }),
    ]
    expect(
      ids(filterEntries(vendors, search({ vendor: 'openai,anthropic' }))),
    ).toEqual(['oai', 'anth'])

    const contexts = [
      entry({ model_id: '8k', context_length: 8_000 }),
      entry({ model_id: '32k', context_length: 20_000 }),
      entry({ model_id: '128k', context_length: 131_072 }),
    ]
    expect(
      ids(filterEntries(contexts, search({ context: '8k,128k' }))),
    ).toEqual(['8k', '128k'])

    const endpoints = [
      entry({ model_id: 'chat', endpoints: ['chat.completions'] }),
      entry({ model_id: 'embed', endpoints: ['Embeddings'] }),
      entry({ model_id: 'rerank', endpoints: ['rerank'] }),
    ]
    expect(
      ids(filterEntries(endpoints, search({ endpoint: 'embeddings,rerank' }))),
    ).toEqual(['embed', 'rerank'])
  })

  it('searches model_id, display_name and vendor case-insensitively', () => {
    const entries = [
      entry({
        model_id: 'openai/gpt-4o',
        display_name: 'GPT-4o',
        vendor: 'OpenAI',
      }),
      entry({
        model_id: 'acme/friendly',
        display_name: 'Friendly Model',
        vendor: 'Acme',
      }),
    ]

    expect(ids(filterEntries(entries, search({ q: 'gpt-4o' })))).toEqual([
      'openai/gpt-4o',
    ])
    expect(ids(filterEntries(entries, search({ q: 'friendly' })))).toEqual([
      'acme/friendly',
    ])
    expect(ids(filterEntries(entries, search({ q: 'acme' })))).toEqual([
      'acme/friendly',
    ])
    expect(ids(filterEntries(entries, search({ q: '  GPT  ' })))).toEqual([
      'openai/gpt-4o',
    ])
  })

  it('keeps only measured entries when measured is set', () => {
    const entries = [
      entry({ model_id: 'measured', hasData: true }),
      entry({ model_id: 'empty', hasData: false }),
    ]

    expect(ids(filterEntries(entries, search({ measured: true })))).toEqual([
      'measured',
    ])
    expect(ids(filterEntries(entries, search()))).toEqual(['measured', 'empty'])
  })

  it('combines groups with AND semantics', () => {
    const entries = [
      entry({
        model_id: 'match',
        vendor: 'OpenAI',
        capabilities: ['vision'],
        hasData: true,
      }),
      entry({
        model_id: 'unmeasured',
        vendor: 'OpenAI',
        capabilities: ['vision'],
        hasData: false,
      }),
      entry({
        model_id: 'no-vision',
        vendor: 'OpenAI',
        capabilities: ['text'],
        hasData: true,
      }),
    ]

    expect(
      ids(
        filterEntries(
          entries,
          search({ cap: 'vision', vendor: 'openai', measured: true }),
        ),
      ),
    ).toEqual(['match'])
  })
})

describe('contextBucketOf', () => {
  it('maps each boundary', () => {
    expect(contextBucketOf(null)).toBeNull()
    expect(contextBucketOf(8_192)).toBe('8k')
    expect(contextBucketOf(8_193)).toBe('32k')
    expect(contextBucketOf(32_768)).toBe('32k')
    expect(contextBucketOf(32_769)).toBe('128k')
    expect(contextBucketOf(131_072)).toBe('128k')
    expect(contextBucketOf(131_073)).toBe('200k+')
  })
})

describe('sortEntries', () => {
  const entries = [
    entry({ model_id: 'nulls', hasData: false }),
    entry({ model_id: 'slow', hasData: true, p50: 200, tps: 5, successRate: 0.9 }),
    entry({ model_id: 'fast', hasData: true, p50: 80, tps: 20, successRate: 0.99 }),
  ]

  function sorted(sort: ModelsSort): string[] {
    return ids(sortEntries(entries, sort, 'en'))
  }

  it('sorts latency ascending with nulls last', () => {
    expect(sorted('latency')).toEqual(['fast', 'slow', 'nulls'])
  })

  it('sorts tps and success descending with nulls last', () => {
    expect(sorted('tps')).toEqual(['fast', 'slow', 'nulls'])
    expect(sorted('success')).toEqual(['fast', 'slow', 'nulls'])
  })

  it('sorts measured coverage first and never mutates the input', () => {
    expect(sorted('measured')).toEqual(['fast', 'slow', 'nulls'])
    expect(ids(entries)).toEqual(['nulls', 'slow', 'fast'])
  })

  it('sorts context length descending with nulls last', () => {
    const contexts = [
      entry({ model_id: 'none', context_length: null }),
      entry({ model_id: 'large', context_length: 128_000 }),
      entry({ model_id: 'small', context_length: 8_000 }),
    ]
    expect(ids(sortEntries(contexts, 'context', 'en'))).toEqual([
      'large',
      'small',
      'none',
    ])
  })

  it('sorts by input price with unpriced models last', () => {
    const priced = [
      entry({ model_id: 'unpriced' }),
      entry({ model_id: 'price-high', input: 4 }),
      entry({ model_id: 'price-low-b', input: 0.06 }),
      entry({ model_id: 'price-low-a', input: 0.06 }),
    ]

    expect(ids(sortEntries(priced, 'price', 'en'))).toEqual([
      'price-low-a',
      'price-low-b',
      'price-high',
      'unpriced',
    ])
  })

  it('breaks ties by model_id', () => {
    const tied = [
      entry({ model_id: 'b', hasData: true, p50: 100 }),
      entry({ model_id: 'a', hasData: true, p50: 100 }),
    ]
    expect(ids(sortEntries(tied, 'latency', 'en'))).toEqual(['a', 'b'])
  })

  it('sorts names by locale', () => {
    const named = [
      entry({ model_id: 'z', display_name: 'Beta' }),
      entry({ model_id: 'y', display_name: 'alpha' }),
    ]
    expect(ids(sortEntries(named, 'name', 'en'))).toEqual(['y', 'z'])
  })
})

describe('activeFilterCount', () => {
  it('counts the search, every selected list item and the measured switch', () => {
    expect(activeFilterCount(search())).toBe(0)
    expect(activeFilterCount(search({ q: '   ' }))).toBe(0)
    expect(activeFilterCount(search({ q: ' gpt ' }))).toBe(1)
    expect(activeFilterCount(search({ cap: 'vision,text' }))).toBe(2)
    expect(activeFilterCount(search({ vendor: 'openai' }))).toBe(1)
    expect(activeFilterCount(search({ context: '8k' }))).toBe(1)
    expect(activeFilterCount(search({ endpoint: 'embeddings,rerank' }))).toBe(2)
    expect(activeFilterCount(search({ measured: true }))).toBe(1)
    expect(
      activeFilterCount(
        search({
          q: 'gpt',
          cap: 'vision',
          vendor: 'openai',
          context: '8k',
          endpoint: 'embeddings',
          measured: true,
        }),
      ),
    ).toBe(6)
  })

  it('does not count display and range defaults', () => {
    expect(
      activeFilterCount(
        search({ sort: 'latency', range: '24h', view: 'cards', compare: 'a,b' }),
      ),
    ).toBe(0)
  })
})
