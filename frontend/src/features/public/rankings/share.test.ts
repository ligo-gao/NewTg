import { describe, expect, it } from 'vitest'

import type { RankingsProvider } from './api'
import { foldTopProviders } from './share'

function provider(
  rank: number,
  author: string,
  total_tokens: number,
  share_pct: number,
  slot: number | null,
): RankingsProvider {
  return { rank, author, total_tokens, share_pct, slot }
}

const FIVE: RankingsProvider[] = [
  provider(1, 'alpha', 500, 50, 1),
  provider(2, 'beta', 200, 20, 2),
  provider(3, 'gamma', 120, 12, 3),
  provider(4, 'delta', 90, 9, 4),
  provider(5, 'epsilon', 60, 6, 5),
]

describe('foldTopProviders', () => {
  it('passes five or fewer providers through verbatim', () => {
    expect(foldTopProviders(FIVE, 'Other')).toEqual([
      { key: '1-alpha', label: 'alpha', share: 50, tokens: 500, slot: 1 },
      { key: '2-beta', label: 'beta', share: 20, tokens: 200, slot: 2 },
      { key: '3-gamma', label: 'gamma', share: 12, tokens: 120, slot: 3 },
      { key: '4-delta', label: 'delta', share: 9, tokens: 90, slot: 4 },
      { key: '5-epsilon', label: 'epsilon', share: 6, tokens: 60, slot: 5 },
    ])
  })

  it('folds the sixth row onward into one Other row that sums share and tokens', () => {
    const six = [
      ...FIVE,
      provider(6, 'zeta', 30, 3, null),
      provider(7, 'eta', 20, 2, null),
    ]

    expect(foldTopProviders(six, 'Other')).toEqual([
      { key: '1-alpha', label: 'alpha', share: 50, tokens: 500, slot: 1 },
      { key: '2-beta', label: 'beta', share: 20, tokens: 200, slot: 2 },
      { key: '3-gamma', label: 'gamma', share: 12, tokens: 120, slot: 3 },
      { key: '4-delta', label: 'delta', share: 9, tokens: 90, slot: 4 },
      { key: '5-epsilon', label: 'epsilon', share: 6, tokens: 60, slot: 5 },
      { key: 'other', label: 'Other', share: 5, tokens: 50, slot: null },
    ])
  })

  it('uses the supplied Other label and keeps the API numbers unrounded', () => {
    const six = [...FIVE, provider(6, 'zeta', 0.4, 0.3, null)]
    const rows = foldTopProviders(six, 'Other vendors')
    expect(rows[5]).toEqual({
      key: 'other',
      label: 'Other vendors',
      share: 0.3,
      tokens: 0.4,
      slot: null,
    })
  })

  it('does not throw on empty or single-row input', () => {
    expect(foldTopProviders([], 'Other')).toEqual([])
    expect(foldTopProviders([provider(1, 'solo', 10, 100, 1)], 'Other')).toEqual([
      { key: '1-solo', label: 'solo', share: 100, tokens: 10, slot: 1 },
    ])
  })
})
