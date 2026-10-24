/**
 * 失败排障传输层。
 *
 * 两个端点都走生成的 `apiClient`，响应类型直接取自 `operations`，因此契约
 * 漂移会在 `typecheck` 阶段暴露，而不是在事故现场才发现字段对不上。
 * `requireData` 把 openapi-fetch 的可选 `data` 收窄为实际返回值。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { operations } from '@/api/schema'

export type RecentFailures =
  operations['listRecentFailures']['responses'][200]['content']['application/json']
export type RecentFailureItem = RecentFailures['items'][number]
export type RecentFailuresQuery = NonNullable<
  operations['listRecentFailures']['parameters']['query']
>
export type RequestTriage =
  operations['getRequestTriage']['responses'][200]['content']['application/json']

/**
 * 最近失败。`query.owner` 由页面上的归属筛选驱动；`counts` 只统计本次
 * 返回的条目（契约已注明），调用方不得把它当作整个窗口的总数。
 */
export async function fetchRecentFailures(
  query: RecentFailuresQuery,
  signal?: AbortSignal,
): Promise<RecentFailures> {
  return requireData(
    await apiClient.GET('/api/usage-logs/triage/recent', {
      params: { query },
      signal,
    }),
  )
}

/**
 * 单条请求的判定结果。格式正确但查无此 ID 时服务端返回 200 +
 * `found: false`，所以调用方必须按 `found` 分支，而不是按 HTTP 状态码。
 */
export async function fetchRequestTriage(
  requestId: string,
  signal?: AbortSignal,
): Promise<RequestTriage> {
  return requireData(
    await apiClient.GET('/api/usage-logs/triage/{request_id}', {
      params: { path: { request_id: requestId } },
      signal,
    }),
  )
}
