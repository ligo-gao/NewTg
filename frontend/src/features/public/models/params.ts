/**
 * `/models` 的 URL 契约（模型库公开页）。
 *
 * URL 是唯一状态源：`validateSearch` 把非法值降级成文档默认值，路由组件
 * 再按 `needsModelsUrlRepair` 用 replace 写回地址栏，因此分享出去的链接
 * 始终只含合法值，也不会把非法值发给后端。
 *
 * 复用 `pricing/params.ts` 的 `CONTEXT_BUCKETS` / `ENDPOINT_TYPES` 冻结枚举
 * 与列表归一函数：两页面对同一组后端枚举的 URL 口径必须一致。
 */

import {
  normalizeCapabilities,
  normalizeContexts,
  normalizeEndpointTypes,
  normalizeVendors,
  SEARCH_MAX_LENGTH,
  withCapabilities,
  withContexts,
  withEndpointTypes,
  withVendors,
} from '@/features/public/pricing/params'

export type ModelsSort =
  | 'measured'
  | 'name'
  | 'latency'
  | 'tps'
  | 'success'
  | 'context'
export type ModelsView = 'table' | 'cards'
export type ModelsRange = '24h' | '7d'

/** 对比栏最多同时放四个模型；多余 id 从尾部截断。 */
export const MAX_COMPARE = 4

export interface ModelsSearch {
  /** 自由文本搜索，trim 后上限 128 字符。 */
  q: string
  /** 逗号分隔的能力多选（动态集合，不校验成员）。 */
  cap: string
  /** 逗号分隔的供应商多选（动态集合，不校验成员）。 */
  vendor: string
  /** 逗号分隔的上下文分档（CONTEXT_BUCKETS 白名单）。 */
  context: string
  /** 逗号分隔的端点类型（ENDPOINT_TYPES 白名单）。 */
  endpoint: string
  /** 「只看有实测数据」开关。 */
  measured: boolean
  sort: ModelsSort
  range: ModelsRange
  view: ModelsView
  /** 逗号分隔的对比模型 id，保序、去重、最多 MAX_COMPARE 个。 */
  compare: string
}

function toText(value: unknown): string {
  if (typeof value === 'string') return value
  // 路由默认解析器会把 `?q=123` 解析成 number，统一回文本。
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return ''
}

function toBoolean(value: unknown): boolean {
  return value === true || value === 1 || value === '1' || value === 'true'
}

/**
 * 对比 id 只做 trim/去重/截断，不转小写：模型 id 大小写敏感，`openai/gpt-5`
 * 与 `openai/gpt-5:free` 中的 `/`、`:` 也必须原样保留。
 */
function normalizeCompare(value: unknown): string[] {
  const raw = Array.isArray(value) ? value : [value]
  const seen = new Set<string>()
  const out: string[] = []
  for (const item of raw) {
    if (typeof item !== 'string') continue
    for (const part of item.split(',')) {
      const id = part.trim()
      if (id === '' || seen.has(id)) continue
      seen.add(id)
      out.push(id)
      if (out.length >= MAX_COMPARE) return out
    }
  }
  return out
}

export function compareIds(search: ModelsSearch): string[] {
  return normalizeCompare(search.compare)
}

export function withCompareIds(ids: readonly string[]): string {
  return normalizeCompare([...ids]).join(',')
}

/**
 * 路由 `validateSearch`。返回值永远合法，页面组件无需再判空；非法值在
 * 这里静默降级，然后由路由组件写回地址栏。
 */
export function validateModelsSearch(
  raw: Record<string, unknown>,
): ModelsSearch {
  const sort: ModelsSort =
    raw.sort === 'name' ||
    raw.sort === 'latency' ||
    raw.sort === 'tps' ||
    raw.sort === 'success' ||
    raw.sort === 'context'
      ? raw.sort
      : 'measured'

  const range: ModelsRange = raw.range === '24h' ? '24h' : '7d'
  const view: ModelsView = raw.view === 'cards' ? 'cards' : 'table'

  return {
    q: toText(raw.q).trim().slice(0, SEARCH_MAX_LENGTH),
    cap: withCapabilities(normalizeCapabilities(raw.cap)),
    vendor: withVendors(normalizeVendors(raw.vendor)),
    context: withContexts(normalizeContexts(raw.context)),
    endpoint: withEndpointTypes(normalizeEndpointTypes(raw.endpoint)),
    measured: toBoolean(raw.measured),
    sort,
    range,
    view,
    compare: withCompareIds(normalizeCompare(raw.compare)),
  }
}

/** 规范化查询串；键序固定，便于与地址栏原始串做等价比较。 */
export function canonicalModelsSearch(search: ModelsSearch): string {
  const params = new URLSearchParams()
  params.set('q', search.q)
  params.set('cap', search.cap)
  params.set('vendor', search.vendor)
  params.set('context', search.context)
  params.set('endpoint', search.endpoint)
  params.set('measured', String(search.measured))
  params.set('sort', search.sort)
  params.set('range', search.range)
  params.set('view', search.view)
  params.set('compare', search.compare)
  return params.toString()
}

/** 原始地址栏与规范化结果不同即需要 replace 写回（含键序/重复键差异）。 */
export function needsModelsUrlRepair(
  rawSearch: string,
  search: ModelsSearch,
): boolean {
  const raw = rawSearch.startsWith('?') ? rawSearch.slice(1) : rawSearch
  return new URLSearchParams(raw).toString() !== canonicalModelsSearch(search)
}
