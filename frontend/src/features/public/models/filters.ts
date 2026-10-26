/**
 * 模型库的筛选、排序与筛选计数（纯函数）。
 *
 * 筛选语义与定价页一致：能力是「全部满足」（AND），供应商/上下文/端点
 * 是「任一满足」（OR），组与组之间是 AND；搜索匹配 model_id、display_name
 * 与 vendor。排序永远是稳定副本，nulls last，最终以 model_id 兜底，
 * 保证同一份目录在任意视图下顺序确定。
 */

import {
  normalizeCapabilities,
  normalizeContexts,
  normalizeEndpointTypes,
  normalizeVendors,
} from '@/features/public/pricing/params'

import type { ModelsSearch, ModelsSort } from './params'
import type { ModelEntry } from './performance'
import { contextBucketOf } from './taxonomy'

export { contextBucketOf }

function compareStrings(a: string, b: string): number {
  if (a < b) return -1
  if (a > b) return 1
  return 0
}

function compareNullable(
  a: number | null,
  b: number | null,
  direction: 1 | -1,
): number {
  if (a === null && b === null) return 0
  if (a === null) return 1
  if (b === null) return -1
  return (a - b) * direction
}

function matchesSearch(entry: ModelEntry, q: string): boolean {
  if (q === '') return true
  const { card } = entry
  return [card.model_id, card.display_name, card.vendor].some((field) =>
    field.toLowerCase().includes(q),
  )
}

export function filterEntries(
  entries: readonly ModelEntry[],
  search: ModelsSearch,
): ModelEntry[] {
  const q = search.q.trim().toLowerCase()
  const capabilities = normalizeCapabilities(search.cap)
  const vendors = normalizeVendors(search.vendor)
  const contexts = normalizeContexts(search.context)
  const endpoints = normalizeEndpointTypes(search.endpoint)

  return entries.filter((entry) => {
    const { card, perf } = entry
    if (search.measured && !perf.hasData) return false
    if (!matchesSearch(entry, q)) return false

    if (capabilities.length > 0) {
      const modelCapabilities = new Set(
        (card.capabilities ?? []).map((value) => value.trim().toLowerCase()),
      )
      if (!capabilities.every((value) => modelCapabilities.has(value))) {
        return false
      }
    }

    if (vendors.length > 0) {
      const vendor = card.vendor.trim().toLowerCase()
      if (!vendors.includes(vendor)) return false
    }

    if (contexts.length > 0) {
      const bucket = contextBucketOf(card.context_length)
      if (bucket === null || !contexts.includes(bucket)) return false
    }

    if (endpoints.length > 0) {
      const modelEndpoints = (card.endpoints ?? []).map((value) =>
        value.trim().toLowerCase(),
      )
      if (!endpoints.some((value) => modelEndpoints.includes(value))) {
        return false
      }
    }

    return true
  })
}

function compareForSort(
  a: ModelEntry,
  b: ModelEntry,
  sort: ModelsSort,
  locale: string,
): number {
  switch (sort) {
    case 'name':
      return a.card.display_name.localeCompare(b.card.display_name, locale)
    case 'latency':
      return compareNullable(a.perf.p50, b.perf.p50, 1)
    case 'price':
      // 输入价升序（便宜优先）；无价模型排在最后，平局交给 model_id。
      return compareNullable(a.price.input, b.price.input, 1)
    case 'tps':
      return compareNullable(a.perf.tps, b.perf.tps, -1)
    case 'success':
      return compareNullable(a.perf.successRate, b.perf.successRate, -1)
    case 'context':
      return compareNullable(a.card.context_length, b.card.context_length, -1)
    case 'measured':
      // 「实测覆盖优先」：有读数的模型在前，同组内交给 model_id 兜底。
      return Number(b.perf.hasData) - Number(a.perf.hasData)
  }
}

/** 排序副本；平局按 model_id 升序，保证目录顺序确定。 */
export function sortEntries(
  entries: readonly ModelEntry[],
  sort: ModelsSort,
  locale: string,
): ModelEntry[] {
  return [...entries].sort((a, b) => {
    const compared = compareForSort(a, b, sort, locale)
    if (compared !== 0) return compared
    return compareStrings(a.card.model_id, b.card.model_id)
  })
}

/**
 * 工具条筛选按钮的计数：搜索词、每个已选列表项、`measured` 各计 1。
 * sort/range/view/compare 是展示与视图状态，不是筛选条件。
 */
export function activeFilterCount(search: ModelsSearch): number {
  const listCount =
    normalizeCapabilities(search.cap).length +
    normalizeVendors(search.vendor).length +
    normalizeContexts(search.context).length +
    normalizeEndpointTypes(search.endpoint).length
  return (
    (search.q.trim() === '' ? 0 : 1) +
    listCount +
    (search.measured ? 1 : 0)
  )
}
