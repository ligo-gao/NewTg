import { describe, expect, it } from 'vitest'

import type { RankingsModel } from './api'
import { metricShare, rankingsSummary, tokensPerRequest } from './summary'

describe('rankings summary derivation', () => {
  it('adds the listed models and counts distinct authors, folding blanks to Unknown', () => {
    const models = [
      { rank: 1, model: 'a', display_name: 'a', author: 'Unknown', total_tokens: 90, requests: 900, delta_tokens: null, delta_pct: null, prev_rank: null },
      { rank: 2, model: 'b', display_name: 'b', author: 'Unknown', total_tokens: 10, requests: 100, delta_tokens: null, delta_pct: null, prev_rank: null },
    ] satisfies RankingsModel[]
    expect(rankingsSummary(models)).toEqual({
      totalTokens: 100, totalRequests: 1000, listedModels: 2, providerCount: 1,
    })
  })

  it('never divides by a zero total', () => {
    expect(metricShare(0, 0)).toBe(0)
    expect(metricShare(5, 0)).toBe(0)
    expect(tokensPerRequest(10, 0)).toBeNull()
    expect(tokensPerRequest(176_585_172, 2_595)).toBe(68_048)
  })
})
