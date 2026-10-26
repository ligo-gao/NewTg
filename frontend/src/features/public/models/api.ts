/**
 * 模型库（`/models`）的目录与批量指标客户端。
 *
 * 目录一次要拿到**完整**模型集合：能力槽计数、对比栏的可用性判断、以及
 * 排序都基于全量目录，不能只看当前页。公开定价接口固定 offset 分页，所以
 * 这里逐页拉取到 `total` 为止；服务端筛选参数一个都不传，保证拿到的是
 * 与定价页同一份全集。
 *
 * 指标接口 `/api/pricing/metrics` 一次返回目录内所有模型的窗口汇总，
 * 页面用 `indexMetrics` 建索引后按 `model_id` 查表。类型一律取自生成的
 * `api/schema.d.ts`，不在前端另写一份会漂移的响应镜像。
 */

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import type { PublicModelCard } from '@/features/public/pricing/api'

import type { ModelsRange } from './params'

/** 公开目录分页上限（服务端把 `page_size` 截断到 100）。 */
export const CATALOG_PAGE_SIZE = 100

/** 目录最多翻 20 页；超过即视为截断，避免异常 `total` 造成无限请求。 */
export const MAX_CATALOG_PAGES = 20

export type PublicModelMetricsList = components['schemas']['PublicModelMetricsList']
export type MetricsItem = components['schemas']['PublicModelMetricsItem']
export type PublicPricingMeta = components['schemas']['PublicPricingMeta']

export interface ModelCatalog {
  items: PublicModelCard[]
  total: number
  /** 服务端报告的 `total` 大于实际取到的条数（达到页数上限或遇到空页）。 */
  truncated: boolean
  /**
   * 计费元数据（`quota_per_unit` 等）。公开定价接口每页都会下发，但它是
   * 全局配置而非分页资源：这里只保留第一页的值，后续页忽略。
   *
   * 可选：被裁剪的旧版响应可能整个字段都不出现。展示层必须显式判空后
   * 再读 `quota_per_unit`，缺失时价格退化成一整列 `—`，而不是白屏。
   */
  meta?: PublicPricingMeta
}

export type LibraryMetrics = Omit<PublicModelMetricsList, 'range'> & {
  range: ModelsRange
}

/**
 * 拉取完整目录。
 *
 * 停止条件：取够 `total`、遇到空页（防止 `total` 虚高时死循环）、或翻满
 * `MAX_CATALOG_PAGES`。跨页按 `model_id` 去重——重复项只保留第一次出现，
 * 避免 React key 冲突；`total` 仍以服务端计数为准，因此去重不会伪装成
 * 完整目录。`total` 不是合法整数时退化为「已取到的条数」。
 */
export async function fetchModelCatalog(
  signal?: AbortSignal,
): Promise<ModelCatalog> {
  const items: PublicModelCard[] = []
  const seen = new Set<string>()
  let total = 0
  let meta: PublicPricingMeta | undefined

  for (let p = 1; p <= MAX_CATALOG_PAGES; p += 1) {
    const page = requireData(
      await apiClient.GET('/api/pricing', {
        params: {
          query: { p, page_size: CATALOG_PAGE_SIZE, sort: 'name' },
        },
        signal,
      }),
    )

    // 计费元数据是全局配置：只用第一页的值，避免后续页异常覆盖。
    if (meta === undefined) meta = page.meta

    const parsedTotal = Number.parseInt(page.total, 10)
    for (const item of page.items) {
      if (seen.has(item.model_id)) continue
      seen.add(item.model_id)
      items.push(item)
    }

    if (Number.isNaN(parsedTotal)) {
      // 无法解析总数时以已取数量为准：本轮之后循环即结束。
      total = items.length
    } else {
      total = parsedTotal
    }

    if (page.items.length === 0) break
    if (items.length >= total) break
  }

  // 第一页缺失 meta 时保持 `undefined`（不伪造计费数据），由展示层判空。
  return { items, total, truncated: items.length < total, meta }
}

export function modelCatalogQueryOptions() {
  return {
    queryKey: ['public', 'models', 'catalog'] as const,
    queryFn: ({ signal }: { signal: AbortSignal }) => fetchModelCatalog(signal),
  }
}

/**
 * 批量指标不是分页资源：一次请求覆盖全集，由页面按 `model_id` 索引。
 * `window` 缺失时用空串兜底，页面只需判空字符串，不会因契约漂移白屏。
 */
export async function fetchLibraryMetrics(
  range: ModelsRange,
  signal?: AbortSignal,
): Promise<LibraryMetrics> {
  const data = requireData(
    await apiClient.GET('/api/pricing/metrics', {
      params: { query: { range } },
      signal,
    }),
  )

  return {
    range: data.range as ModelsRange,
    window: {
      start: data.window?.start ?? '',
      end: data.window?.end ?? '',
    },
    items: data.items ?? [],
  }
}

export function libraryMetricsQueryOptions(range: ModelsRange) {
  return {
    queryKey: ['public', 'models', 'metrics', range] as const,
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      fetchLibraryMetrics(range, signal),
    // 指标是带失败降级的展示数据：失败不重试，避免错误态反复闪烁；
    // 一分钟内切排序/视图不重新请求。
    retry: false as const,
    staleTime: 60_000,
  }
}

/** 以 `model_id` 建索引；重复时后者覆盖前者（防御服务端重复行）。 */
export function indexMetrics(
  items: readonly MetricsItem[],
): Map<string, MetricsItem> {
  const index = new Map<string, MetricsItem>()
  for (const item of items) index.set(item.model_id, item)
  return index
}
