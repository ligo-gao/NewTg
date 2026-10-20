/**
 * 公开定价接口（M6c Task 7 已交付，冻结契约见 handlers_public_pricing.go）。
 *
 * 只用生成客户端的类型化 `apiClient`：路径、查询参数与响应字段都由
 * `api/schema.d.ts` 校验，不在前端另写一份会漂移的接口镜像。
 */

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

import type { PricingQuotaType, PricingSort } from './params'

export type PublicPricingList = components['schemas']['PublicPricingList']
export type PublicModelCard = components['schemas']['PublicModelCard']
export type PublicModelDetail = components['schemas']['PublicModelDetail']
export type PublicGroupPrice = components['schemas']['PublicGroupPrice']
export type PublicPricingMeta = components['schemas']['PublicPricingMeta']
export type PublicFacets = components['schemas']['PublicFacets']
export type PublicFacetOption = components['schemas']['PublicFacetOption']

export interface PricingQuery {
  p: number
  pageSize: number
  search: string
  sort: PricingSort
  vendors: readonly string[]
  capabilities: readonly string[]
  contexts: readonly string[]
  endpointTypes: readonly string[]
  group: string
  quotaType: PricingQuotaType
}

/**
 * 查询键只包含会改变服务端结果集的参数。展示开关（view/M-K/汇率）
 * 与分页大小本身会改变请求，但 view 不会——把它排除可以保证切换
 * 卡片/表格不触发网络请求（F-09-17 验收）。
 */
export function pricingListQueryKey(query: PricingQuery) {
  return [
    'public',
    'pricing',
    query.p,
    query.pageSize,
    query.search,
    query.sort,
    [...query.vendors].sort().join(','),
    [...query.capabilities].sort().join(','),
    [...query.contexts].sort().join(','),
    [...query.endpointTypes].sort().join(','),
    query.group,
    query.quotaType,
  ] as const
}

export async function fetchPricingList(
  query: PricingQuery,
  signal?: AbortSignal,
): Promise<PublicPricingList> {
  const result = await apiClient.GET('/api/pricing', {
    params: {
      query: {
        p: query.p,
        page_size: query.pageSize,
        ...(query.search !== '' ? { search: query.search } : {}),
        sort: query.sort,
        ...(query.vendors.length > 0 ? { vendor: [...query.vendors] } : {}),
        // 后端冻结的形态是重复键（`capability=a&capability=b`）或逗号分隔；
        // openapi-fetch 默认把数组序列化成重复键，正好匹配，无需自拼查询串。
        ...(query.capabilities.length > 0
          ? { capability: [...query.capabilities] }
          : {}),
        ...(query.contexts.length > 0 ? { context: [...query.contexts] } : {}),
        ...(query.endpointTypes.length > 0
          ? { endpointType: [...query.endpointTypes] }
          : {}),
        ...(query.group !== '' ? { group: query.group } : {}),
        quotaType: query.quotaType,
      },
    },
    signal,
  })
  return requireData(result)
}

export function pricingListQueryOptions(query: PricingQuery) {
  return {
    queryKey: pricingListQueryKey(query),
    queryFn: ({ signal }: { signal: AbortSignal }) => fetchPricingList(query, signal),
  }
}

export function pricingDetailQueryKey(modelId: string) {
  return ['public', 'pricing', 'detail', modelId] as const
}

/**
 * 详情读取。`model_id` 可能包含 `/`（供应商前缀），由 openapi-fetch 的
 * path 序列化器统一 `encodeURIComponent`；不要在调用处二次编码。
 */
export async function fetchPricingDetail(
  modelId: string,
  signal?: AbortSignal,
): Promise<PublicModelDetail> {
  const result = await apiClient.GET('/api/pricing/{model_id}', {
    params: { path: { model_id: modelId } },
    signal,
  })
  return requireData(result)
}

export function pricingDetailQueryOptions(modelId: string) {
  return {
    queryKey: pricingDetailQueryKey(modelId),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      fetchPricingDetail(modelId, signal),
    retry: false as const,
  }
}
