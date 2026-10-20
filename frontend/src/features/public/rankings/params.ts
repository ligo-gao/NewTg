/**
 * `/rankings` 的可分享状态（F-09-29）：`?period=today|week|month|year`。
 *
 * URL 是唯一状态源。非法值在 `validateSearch` 阶段静默回落 `week`，随后由
 * 路由组件用 replace 写回地址栏——不新增历史、也不会把非法值发给后端。
 */

import {
  DEFAULT_RANKINGS_PERIOD,
  normalizeRankingsPeriod,
  type RankingsPeriod,
} from './api'

export interface RankingsSearch {
  period: RankingsPeriod
}

export function validateRankingsSearch(
  search: Record<string, unknown>,
): RankingsSearch {
  return { period: normalizeRankingsPeriod(search.period) }
}

/**
 * 地址栏是否需要规范化：缺省、非法或重复参数都要写回单一合法值。
 * 与 pricing 的 `needsPricingUrlRepair` 同构；memory-router 测试里
 * `window.location` 不属于该路由，调用方会先判断 pathname。
 */
export function needsRankingsUrlRepair(rawSearch: string): boolean {
  const params = new URLSearchParams(rawSearch)
  const values = params.getAll('period')
  if (values.length !== 1) return true
  return values[0] !== normalizeRankingsPeriod(values[0])
}

export function defaultRankingsSearch(): RankingsSearch {
  return { period: DEFAULT_RANKINGS_PERIOD }
}
