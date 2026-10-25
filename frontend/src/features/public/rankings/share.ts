/**
 * 上游份额聚合的唯一实现：前 5 名逐行保留，其余合成一行 `Other`。
 *
 * `/rankings` 的份额条（`ShareBar`）与右栏供应商列表用的是同一条规则，
 * 分开实现会漂移（例如一边改截断位置、另一边忘了求和）。份额与 token 都是
 * 上游读数之和，不重新归一化、不四舍五入。
 */

import type { RankingsProvider } from './api'

export interface FoldedProvider {
  key: string
  label: string
  share: number
  tokens: number
  slot: number | null
}

export function foldTopProviders(
  providers: readonly RankingsProvider[],
  otherLabel: string,
): FoldedProvider[] {
  const top = providers.slice(0, 5)
  const rest = providers.slice(5)
  return [
    ...top.map((provider) => ({
      key: `${provider.rank}-${provider.author}`,
      label: provider.author,
      share: provider.share_pct,
      tokens: provider.total_tokens,
      slot: provider.slot,
    })),
    ...(rest.length > 0
      ? [
          {
            key: 'other',
            label: otherLabel,
            share: rest.reduce((sum, provider) => sum + provider.share_pct, 0),
            tokens: rest.reduce(
              (sum, provider) => sum + provider.total_tokens,
              0,
            ),
            slot: null,
          },
        ]
      : []),
  ]
}
