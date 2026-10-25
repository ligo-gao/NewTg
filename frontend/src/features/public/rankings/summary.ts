/**
 * 排行榜页面的纯派生计算。所有可视图读数都从这里出发，页面组件不再自行
 * 拼百分数或做除法：零分母的契约（返回 0 / null，绝不产出 NaN）只在这里
 * 定义一次，也只在 `summary.test.ts` 里被钉住。
 */

import type { RankingsModel } from './api'

export type RankingsMetric = 'tokens' | 'requests'

export interface RankingsSummary {
  totalTokens: number
  totalRequests: number
  listedModels: number
  providerCount: number
}

/** 服务端缺失的厂商档案统一折叠成这个标签，不猜、不用模型名反推。 */
const UNKNOWN_AUTHOR = 'Unknown'

/** `''` / 纯空白都视为缺失；返回值可直接参与展示与去重。 */
function normalizeAuthor(author: string): string {
  const trimmed = author.trim()
  return trimmed === '' ? UNKNOWN_AUTHOR : trimmed
}

export function rankingsSummary(
  models: readonly RankingsModel[],
): RankingsSummary {
  let totalTokens = 0
  let totalRequests = 0
  const authors = new Set<string>()
  for (const model of models) {
    totalTokens += model.total_tokens
    totalRequests += model.requests
    authors.add(normalizeAuthor(model.author))
  }
  return {
    totalTokens,
    totalRequests,
    listedModels: models.length,
    providerCount: authors.size,
  }
}

/** Share of the listed total on a 0–100 scale; 0 when the total is 0. */
export function metricShare(value: number, total: number): number {
  return total === 0 ? 0 : (value / total) * 100
}

/** Integer tokens per request, or null when requests === 0. */
export function tokensPerRequest(
  tokens: number,
  requests: number,
): number | null {
  // 零请求没有平均值，返回 null 让调用方渲染空态；其余按四舍五入取整，
  // 与「每次请求平均 token」的展示口径一致。
  return requests <= 0 ? null : Math.round(tokens / requests)
}
