import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

/**
 * 模型配置页的数据访问（modules-02 §2.14 / F-02-18）。
 *
 * 只复用既有契约：`GET /api/tokens` 与 `PATCH /api/tokens/{id}`。模型候选集
 * 来自 `GET /api/status.usable_groups`（由调用方从缓存读取），这里不再请求，
 * 避免同一屏出现两份可能分叉的模型清单。
 */
export type Token = components['schemas']['Token']
export type UsableGroup = components['schemas']['UsableGroup']
export type TokenList = components['schemas']['TokenList']

/**
 * 令牌列表的契约上限（page_size ≤ 100）。模型配置页是「一次看全」的集中
 * 视图，分页只会让批量操作变得不可预测，因此单页取满。
 */
export const TOKEN_PAGE_SIZE = 100

export async function listUserTokens(): Promise<Token[]> {
  const data = requireData(
    await apiClient.GET('/api/tokens', {
      params: { query: { p: 1, page_size: TOKEN_PAGE_SIZE } },
    }),
  )
  // 生成的 TokenList 交叉类型会把 `items` 退化为 unknown[]；这里恢复元素
  // 类型，不重新声明任何线上字段。
  return (data.items ?? []) as Token[]
}

/**
 * 更新单个令牌的模型限制。`model_limits` 为空数组 = 不限。
 *
 * PATCH 采用乐观锁：必须原样回传本行加载时的 `updated_at`，否则 409。
 * 服务端还会逐项校验「模型 ∈ 该令牌分组的可用模型」，越权放宽返回 422，
 * 前端多选器只是便利，不承担授权判断。
 */
export async function updateTokenModelLimits(
  token: Token,
  modelLimits: string[],
): Promise<Token> {
  return requireData(
    await apiClient.PATCH('/api/tokens/{id}', {
      params: { path: { id: token.id } },
      body: { model_limits: modelLimits, updated_at: token.updated_at },
    }),
  )
}

function groupModels(groups: readonly UsableGroup[], name: string): string[] {
  return groups.find((group) => group.name === name)?.models ?? []
}

/**
 * 该令牌可选的模型清单。
 *
 * - 普通令牌：取其绑定分组的模型；
 * - `auto` 令牌：取 `auto_groups` 各分组模型的并集（自动分组没有单一分组）；
 * - 分组不可用（被管理员下线、或跨版本残留）：空数组，多选器显示空态，
 *   绝不把用户不可用分组的模型展示出来（§2.15 安全纪律）。
 */
export function modelsForToken(
  token: Token,
  groups: readonly UsableGroup[],
): string[] {
  if (token.group === 'auto') {
    const names = token.auto_groups ?? []
    const union = new Set<string>()
    for (const name of names) {
      for (const model of groupModels(groups, name)) union.add(model)
    }
    return [...union].sort((a, b) => a.localeCompare(b))
  }
  return groupModels(groups, token.group)
}

/** 可用模型的全集（用于判断已保存的默认模型是否仍然可选）。 */
export function allUsableModels(groups: readonly UsableGroup[]): Set<string> {
  const union = new Set<string>()
  for (const group of groups) {
    for (const model of group.models) union.add(model)
  }
  return union
}

/** 令牌模型限制摘要：null/空 = 不限，否则计数。 */
export function modelLimitsSummary(limits: string[] | null | undefined): {
  unlimited: boolean
  count: number
} {
  if (limits === null || limits === undefined || limits.length === 0) {
    return { unlimited: true, count: 0 }
  }
  return { unlimited: false, count: limits.length }
}
