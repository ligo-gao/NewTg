/**
 * `/pricing` 的 URL 契约（modules-09 F-09-14/F-09-22）。
 *
 * 为什么 URL 是筛选状态的唯一来源：页面要求刷新、分享、前进/后退都能复现
 * 同一结果集（F-09-15 验收 1、F-09-16 验收 3）。组件内只保留「草稿」
 * （移动端 Sheet、搜索输入框），提交时一律写回这里定义的结构。
 *
 * 逐参数降级（F-09-22）在这里完成，而不是丢给服务端：服务端确实宽容，
 * 但前端仍需用同一套规则修正地址栏，否则分享出去的 URL 会一直携带
 * 非法值并让「已复制链接」在下次打开时再次被降级。
 */

export type PricingSort = 'name' | 'price_asc' | 'price_desc'
export type PricingQuotaType = 'all' | 'token' | 'request'
export type PricingView = 'card' | 'table'
export type PricingTokenUnit = 'M' | 'K'

export interface PricingSearch {
  /** 1 基页码；越界由服务端收敛后前端回写。 */
  p: number
  page_size: number
  /** model_id 子串，服务端上限 128 字符。 */
  search: string
  sort: PricingSort
  /**
   * 逗号分隔的供应商标识。刻意用字符串而不是数组：TanStack Router 的默认
   * 序列化器会把数组写成 JSON（`vendor=["openai"]`），既不符合冻结契约的
   * `vendor=a,b`/重复键形态，也让分享链接难以人工阅读。
   */
  vendor: string
  /** 空串表示不按分组过滤（浏览默认价）。 */
  group: string
  quotaType: PricingQuotaType
  view: PricingView
  /**
   * 展示层开关：只影响前端格式化，绝不作为查询参数发给接口（F-09-19）。
   */
  tokenUnit: PricingTokenUnit
  dualCurrency: boolean
  rechargePrice: boolean
}

export const SEARCH_MAX_LENGTH = 128
export const VENDOR_FILTER_MAX = 50

/** 卡片/表格各自的每页密度（F-09-21）。服务端上限 100。 */
export const PAGE_SIZE_OPTIONS: Record<PricingView, readonly number[]> = {
  card: [24, 48, 96],
  table: [20, 50, 100],
}

export const DEFAULT_PAGE_SIZE: Record<PricingView, number> = {
  card: 24,
  table: 50,
}

function toInteger(value: unknown): number | null {
  if (typeof value === 'number' && Number.isInteger(value)) return value
  if (typeof value === 'string' && /^\d+$/.test(value.trim())) {
    return Number(value)
  }
  return null
}

function toBoolean(value: unknown): boolean {
  return value === true || value === 1 || value === '1' || value === 'true'
}

function toText(value: unknown): string {
  if (typeof value === 'string') return value
  // 默认的 search 解析器会对看起来像 JSON 的字符串做 JSON.parse：
  // `?search=123` 会变成 number。这里统一回文本，避免下游到处判类型。
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return ''
}

/**
 * vendor 既接受逗号分隔也接受重复键（服务端冻结的两种形态）。重复键经
 * 路由解析后可能是数组，这里一并归一。大小写按服务端规则转小写并去重。
 */
export function normalizeVendors(value: unknown): string[] {
  const raw = Array.isArray(value) ? value : [value]
  const seen = new Set<string>()
  const out: string[] = []
  for (const item of raw) {
    if (typeof item !== 'string') continue
    for (const part of item.split(',')) {
      const vendor = part.trim().toLowerCase()
      if (vendor === '' || seen.has(vendor)) continue
      seen.add(vendor)
      out.push(vendor)
      if (out.length >= VENDOR_FILTER_MAX) return out
    }
  }
  return out
}

export function vendorList(search: Pick<PricingSearch, 'vendor'>): string[] {
  return normalizeVendors(search.vendor)
}

export function withVendors(vendors: readonly string[]): string {
  return normalizeVendors([...vendors]).join(',')
}

/**
 * 两种视图的尺寸集合取并集：切换视图时旧尺寸仍是合法的服务端值，
 * 保留它比静默改写更贴近「视图切换保留分页位置」（F-09-17）。
 */
function isAllowedPageSize(value: number): boolean {
  return PAGE_SIZE_OPTIONS.card.includes(value) || PAGE_SIZE_OPTIONS.table.includes(value)
}

/**
 * 路由 `validateSearch`。注意它同时承担「非法值降级」：返回值永远是
 * 合法的 `PricingSearch`，因此页面组件不需要再判空。
 */
export function validatePricingSearch(raw: Record<string, unknown>): PricingSearch {
  const view: PricingView = raw.view === 'table' ? 'table' : 'card'

  const page = toInteger(raw.p)
  const p = page !== null && page >= 1 ? page : 1

  const size = toInteger(raw.page_size)
  const pageSize =
    size !== null && Number.isInteger(size) && size > 0 && size <= 100 &&
    isAllowedPageSize(size)
      ? size
      : DEFAULT_PAGE_SIZE[view]

  const search = toText(raw.search).trim().slice(0, SEARCH_MAX_LENGTH)

  const sort: PricingSort =
    raw.sort === 'price_asc' || raw.sort === 'price_desc' ? raw.sort : 'name'

  const vendor = withVendors(normalizeVendors(raw.vendor))

  const group = toText(raw.group).trim()

  const quotaType: PricingQuotaType =
    raw.quotaType === 'token' || raw.quotaType === 'request'
      ? raw.quotaType
      : 'all'

  return {
    p,
    page_size: pageSize,
    search,
    sort,
    vendor,
    group,
    quotaType,
    view,
    tokenUnit: raw.tokenUnit === 'K' ? 'K' : 'M',
    dualCurrency: toBoolean(raw.dualCurrency),
    rechargePrice: toBoolean(raw.rechargePrice),
  }
}

/** 只把真正会改变结果集的字段送入查询键，避免视图开关触发请求。 */
export function queryParamsOf(search: PricingSearch) {
  return {
    p: search.p,
    pageSize: search.page_size,
    search: search.search,
    sort: search.sort,
    vendors: vendorList(search),
    group: search.group,
    quotaType: search.quotaType,
  }
}

/** 活动筛选计数（移动端「筛选 (N)」徽标与重置按钮）。 */
export function activeFilterCount(search: PricingSearch): number {
  let count = vendorList(search).length
  if (search.search !== '') count += 1
  if (search.group !== '') count += 1
  if (search.quotaType !== 'all') count += 1
  return count
}

/** 「重置」保留展示偏好（view / tokenUnit / 双币种 / 充值价），见 F-09-20。 */
export function clearedFilters(search: PricingSearch): PricingSearch {
  return {
    ...search,
    p: 1,
    search: '',
    vendor: '',
    group: '',
    quotaType: 'all',
  }
}

/**
 * 规范化后的查询串。键序与 `validatePricingSearch` 的返回顺序一致，
 * 便于与路由序列化结果做等价比较。
 */
export function canonicalPricingSearch(search: PricingSearch): string {
  const params = new URLSearchParams()
  params.set('p', String(search.p))
  params.set('page_size', String(search.page_size))
  params.set('search', search.search)
  params.set('sort', search.sort)
  params.set('vendor', search.vendor)
  params.set('group', search.group)
  params.set('quotaType', search.quotaType)
  params.set('view', search.view)
  params.set('tokenUnit', search.tokenUnit)
  params.set('dualCurrency', String(search.dualCurrency))
  params.set('rechargePrice', String(search.rechargePrice))
  return params.toString()
}

/**
 * 原始地址栏与规范化结果是否有差异（F-09-22 口径 2）。
 *
 * 直接比较**原始 search** 而不是逐项白名单：`page_size=30`、`dualCurrency=yes`、
 * `search=%20gpt`、`vendor=OpenAI` 这些都会被 validateSearch 静默降级，
 * 只检查部分键会让地址栏继续显示无效值，分享出去再次降级。
 */
export function needsPricingUrlRepair(
  rawSearch: string,
  search: PricingSearch,
): boolean {
  const raw = rawSearch.startsWith('?') ? rawSearch.slice(1) : rawSearch
  return new URLSearchParams(raw).toString() !== canonicalPricingSearch(search)
}
