import { describe, expect, it } from 'vitest'

import {
  MAX_COMPARE,
  canonicalModelsSearch,
  compareIds,
  needsModelsUrlRepair,
  validateModelsSearch,
  withCompareIds,
} from './params'

describe('models URL contract', () => {
  it('returns the documented defaults', () => {
    expect(validateModelsSearch({})).toEqual({
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
    })
  })

  it('trims and lowercases list values and drops duplicates', () => {
    const search = validateModelsSearch({
      q: '  gpt  ',
      cap: 'Vision, text,vision',
      vendor: 'OpenAI, anthropic,OpenAI',
      context: '8K, 128k, 8k',
      endpoint: 'Messages, chat.completions,messages',
    })
    expect(search.q).toBe('gpt')
    expect(search.cap).toBe('vision,text')
    expect(search.vendor).toBe('openai,anthropic')
    expect(search.context).toBe('8k,128k')
    expect(search.endpoint).toBe('messages,chat.completions')
  })

  it('keeps unknown capabilities and vendors but drops unknown context/endpoint enums', () => {
    const search = validateModelsSearch({
      // cap/vendor 的合法域来自目录数据，静态代码无法白名单化。
      cap: 'vision,telepathy',
      vendor: 'anthropic,acme-labs',
      // context/endpoint 是冻结枚举；16k / chat 这类拼写直接丢弃。
      context: '8k,16k,200K+',
      endpoint: 'messages,chat,images.generations',
    })
    expect(search.cap).toBe('vision,telepathy')
    expect(search.vendor).toBe('anthropic,acme-labs')
    expect(search.context).toBe('8k,200k+')
    expect(search.endpoint).toBe('messages,images.generations')
  })

  it('drops non-enum sort/range/view and repairs the URL', () => {
    const search = validateModelsSearch({
      sort: 'bogus',
      range: '30d',
      view: 'grid',
      measured: 'yes',
    })
    expect(search.sort).toBe('measured')
    expect(search.range).toBe('7d')
    expect(search.view).toBe('table')
    expect(search.measured).toBe(false)
    expect(needsModelsUrlRepair('?sort=bogus&range=30d&view=grid', search)).toBe(true)
  })

  it('accepts the documented enum values', () => {
    expect(validateModelsSearch({ sort: 'latency' }).sort).toBe('latency')
    expect(validateModelsSearch({ sort: 'tps' }).sort).toBe('tps')
    expect(validateModelsSearch({ sort: 'success' }).sort).toBe('success')
    expect(validateModelsSearch({ sort: 'context' }).sort).toBe('context')
    expect(validateModelsSearch({ sort: 'name' }).sort).toBe('name')
    expect(validateModelsSearch({ range: '24h' }).range).toBe('24h')
    expect(validateModelsSearch({ view: 'cards' }).view).toBe('cards')
  })

  it('parses the measured flag from its accepted spellings', () => {
    for (const raw of [true, 1, '1', 'true']) {
      expect(validateModelsSearch({ measured: raw }).measured).toBe(true)
    }
    for (const raw of [false, 0, '0', 'false', 'yes', undefined]) {
      expect(validateModelsSearch({ measured: raw }).measured).toBe(false)
    }
  })

  it('truncates compare to four ids and dedupes', () => {
    const search = validateModelsSearch({
      compare: 'openai/gpt-5, anthropic/claude,openai/gpt-5,google/gemini,meta/llama',
    })
    expect(MAX_COMPARE).toBe(4)
    expect(compareIds(search)).toEqual([
      'openai/gpt-5',
      'anthropic/claude',
      'google/gemini',
      'meta/llama',
    ])
    expect(search.compare).toBe('openai/gpt-5,anthropic/claude,google/gemini,meta/llama')
  })

  it('keeps model ids containing / and : intact', () => {
    const search = validateModelsSearch({
      compare: 'openai/gpt-5:free, qwen/qwen3:8b',
    })
    expect(compareIds(search)).toEqual(['openai/gpt-5:free', 'qwen/qwen3:8b'])
    expect(withCompareIds(['openai/gpt-5:free'])).toBe('openai/gpt-5:free')
  })

  it('accepts repeated keys as well as comma lists', () => {
    const search = validateModelsSearch({
      cap: ['vision', 'text,vision'],
      vendor: ['openai', 'anthropic,openai'],
      compare: ['a', 'b,a', 'c', 'd', 'e'],
    })
    expect(search.cap).toBe('vision,text')
    expect(search.vendor).toBe('openai,anthropic')
    expect(compareIds(search)).toEqual(['a', 'b', 'c', 'd'])
  })

  it('truncates the free-text query to 128 characters', () => {
    expect(validateModelsSearch({ q: 'x'.repeat(200) }).q).toHaveLength(128)
  })

  it('round-trips through the canonical form and detects dirty URLs', () => {
    const search = validateModelsSearch({
      q: 'gpt',
      cap: 'vision',
      vendor: 'openai',
      context: '128k',
      endpoint: 'messages',
      measured: '1',
      sort: 'tps',
      range: '24h',
      view: 'cards',
      compare: 'a,b',
    })
    expect(canonicalModelsSearch(search)).toBe(
      'q=gpt&cap=vision&vendor=openai&context=128k&endpoint=messages' +
        '&measured=true&sort=tps&range=24h&view=cards&compare=a%2Cb',
    )
    const reparsed = validateModelsSearch(
      Object.fromEntries(new URLSearchParams(canonicalModelsSearch(search))),
    )
    expect(reparsed).toEqual(search)
    expect(needsModelsUrlRepair(`?${canonicalModelsSearch(search)}`, search)).toBe(false)
    // 未知枚举被降级后，原始地址栏必须触发一次 replace 回写。
    expect(
      needsModelsUrlRepair('?context=16k', validateModelsSearch({ context: '16k' })),
    ).toBe(true)
    // 大小写/空白被归一同样需要回写。
    expect(
      needsModelsUrlRepair('?vendor=OpenAI', validateModelsSearch({ vendor: 'OpenAI' })),
    ).toBe(true)
  })

  it('writes every documented key in a stable order', () => {
    const keys = [...new URLSearchParams(canonicalModelsSearch(validateModelsSearch({}))).keys()]
    expect(keys).toEqual([
      'q',
      'cap',
      'vendor',
      'context',
      'endpoint',
      'measured',
      'sort',
      'range',
      'view',
      'compare',
    ])
  })
})
