import { isUuid, normalizeRequestId } from '@/features/logs/search'

import type {
  WalletTransactionFilterDir,
  WalletTransactionStatus,
  WalletTransactionType,
} from './types'

/**
 * `/wallet` 的页面级 Tab 与流水筛选的 URL 形状（R-M3-01/R-M3-07）。
 *
 * 默认 Tab 是 `topup`：只有 `?tab=transactions` 会被序列化，默认视图保持
 * 干净链接。流水筛选与分页全部写回 URL，刷新/分享可复现（F-03-02 主流程 2）。
 */

export type WalletTab = 'topup' | 'transactions'

export const WALLET_TRANSACTION_TYPES: readonly WalletTransactionType[] = [
  'topup',
  'redemption',
  'consume_pre',
  'consume',
  'release',
  'reversal',
  'admin_adjustment',
  'bonus',
  'affiliate_transfer',
  'checkin_bonus',
  'subscription_purchase',
]

export const WALLET_TRANSACTION_STATUSES: readonly WalletTransactionStatus[] = [
  'hold',
  'settled',
  'released',
  'expired',
  'confirmed',
]

/** UI 只暴露 10/20/50（R-M3-10）；API 的 1..100 契约仍由后端守卫。 */
export const WALLET_PAGE_SIZES = [10, 20, 50] as const

export const MAX_WALLET_RANGE_DAYS = 90
const MAX_WALLET_RANGE_MS = MAX_WALLET_RANGE_DAYS * 24 * 60 * 60 * 1000

export interface WalletTransactionsSearch {
  /**
   * 只有 `transactions` 会出现在 URL；缺省/非法值都落到 `topup`。
   * 它不在类型里用 `tab?: WalletTab`，避免把默认值写进链接。
   */
  tab?: 'transactions'
  type?: WalletTransactionType
  status?: WalletTransactionStatus
  dir?: WalletTransactionFilterDir
  /** `datetime-local` 原始值；提交时转 RFC3339。 */
  from?: string
  to?: string
  min?: number
  max?: number
  ref?: string
  p: number
  page_size: number
}

export const DEFAULT_WALLET_SEARCH: WalletTransactionsSearch = {
  p: 1,
  page_size: 20,
}

function asTrimmedString(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined
  const trimmed = value.trim()
  return trimmed === '' ? undefined : trimmed
}

function asPositiveInteger(value: unknown): number | undefined {
  const parsed =
    typeof value === 'number'
      ? value
      : typeof value === 'string' && value.trim() !== ''
        ? Number(value)
        : Number.NaN
  if (!Number.isInteger(parsed) || parsed < 1) return undefined
  return parsed
}

/** 非负整数（金额区间）；小数/负数由界面行内报错拦截。 */
function asNonNegativeInteger(value: unknown): number | undefined {
  const parsed =
    typeof value === 'number'
      ? value
      : typeof value === 'string' && value.trim() !== ''
        ? Number(value)
        : Number.NaN
  if (!Number.isInteger(parsed) || parsed < 0) return undefined
  return parsed
}

/**
 * 归一化 route search：未知键丢弃、非法枚举回落默认。
 *
 * `from`/`to` 原样保留，便于界面在保留下拉/输入内容的同时给出字段级错误；
 * 真正发请求前由 `validateWalletRange` 拦截（设计：跨度 >90 天不发请求）。
 */
export function validateWalletSearch(
  search: Record<string, unknown>,
): WalletTransactionsSearch {
  const result: WalletTransactionsSearch = {
    p: 1,
    page_size: DEFAULT_WALLET_SEARCH.page_size,
  }

  if (search.tab === 'transactions') result.tab = 'transactions'

  const type = asTrimmedString(search.type)
  if (
    type !== undefined &&
    (WALLET_TRANSACTION_TYPES as readonly string[]).includes(type)
  ) {
    result.type = type as WalletTransactionType
  }

  const status = asTrimmedString(search.status)
  if (
    status !== undefined &&
    (WALLET_TRANSACTION_STATUSES as readonly string[]).includes(status)
  ) {
    result.status = status as WalletTransactionStatus
  }

  const dir = asTrimmedString(search.dir)
  if (dir === 'in' || dir === 'out') result.dir = dir

  const from = asTrimmedString(search.from)
  if (from !== undefined) result.from = from
  const to = asTrimmedString(search.to)
  if (to !== undefined) result.to = to

  const min = asNonNegativeInteger(search.min)
  if (min !== undefined) result.min = min
  const max = asNonNegativeInteger(search.max)
  if (max !== undefined) result.max = max

  const ref = asTrimmedString(search.ref)
  if (ref !== undefined && ref.length <= 64) result.ref = ref

  const page = asPositiveInteger(search.p)
  if (page !== undefined) result.p = page

  const pageSize = asPositiveInteger(search.page_size)
  if (pageSize !== undefined && (WALLET_PAGE_SIZES as readonly number[]).includes(pageSize)) {
    result.page_size = pageSize
  }
  return result
}

export type WalletRangeError = 'invalid' | 'order' | 'too_long' | null

/** 完全客户端的时间跨度校验：不合法时界面不发请求（后端 422 仍是兜底）。 */
export function validateWalletRange(
  from: string | undefined,
  to: string | undefined,
): WalletRangeError {
  // 空串是 datetime-local 的「未选择」，与 undefined 同义。
  const normalizedFrom = from === '' ? undefined : from
  const normalizedTo = to === '' ? undefined : to
  const fromMs =
    normalizedFrom === undefined ? undefined : Date.parse(normalizedFrom)
  const toMs = normalizedTo === undefined ? undefined : Date.parse(normalizedTo)
  if (fromMs !== undefined && Number.isNaN(fromMs)) return 'invalid'
  if (toMs !== undefined && Number.isNaN(toMs)) return 'invalid'
  if (fromMs !== undefined && toMs !== undefined) {
    if (fromMs > toMs) return 'order'
    if (toMs - fromMs > MAX_WALLET_RANGE_MS) return 'too_long'
  }
  return null
}

export type WalletAmountError = 'invalid' | 'order' | null

/**
 * 金额区间草稿校验：空串合法；非法输入不提交。min>max 在失焦时行内报错。
 */
export function validateWalletAmountRange(
  minDraft: string,
  maxDraft: string,
): WalletAmountError {
  const parse = (raw: string): number | 'invalid' | null => {
    const trimmed = raw.trim()
    if (trimmed === '') return null
    if (!/^\d+$/.test(trimmed)) return 'invalid'
    return Number(trimmed)
  }
  const min = parse(minDraft)
  const max = parse(maxDraft)
  if (min === 'invalid' || max === 'invalid') return 'invalid'
  if (min !== null && max !== null && min > max) return 'order'
  return null
}

/** `datetime-local` → ISO（RFC3339）；空串/非法值返回 undefined。 */
export function datetimeLocalToIso(value: string | undefined): string | undefined {
  if (value === undefined || value === '') return undefined
  const ms = Date.parse(value)
  if (Number.isNaN(ms)) return undefined
  return new Date(ms).toISOString()
}

/**
 * ref 归一（R-M3-07）：UUID 形状与 logs 页复用同一判定，去连字符后发送，
 * 后端仍会做第二次归一；非 UUID 关联号保持原样精确匹配。
 */
export function walletRefForQuery(ref: string): string {
  return isUuid(ref) ? normalizeRequestId(ref) : ref
}

export interface WalletTransactionsQuery {
  p: number
  page_size: number
  type?: WalletTransactionType
  status?: WalletTransactionStatus
  dir?: WalletTransactionFilterDir
  from?: string
  to?: string
  min?: number
  max?: number
  ref?: string
}

/** 构造 GET /api/wallet/transactions 的 query 参数。 */
export function buildWalletTransactionsQuery(
  search: WalletTransactionsSearch,
): WalletTransactionsQuery {
  const query: WalletTransactionsQuery = {
    p: search.p,
    page_size: search.page_size,
  }
  if (search.type !== undefined) query.type = search.type
  if (search.status !== undefined) query.status = search.status
  if (search.dir !== undefined) query.dir = search.dir
  const from = datetimeLocalToIso(search.from)
  if (from !== undefined) query.from = from
  const to = datetimeLocalToIso(search.to)
  if (to !== undefined) query.to = to
  if (search.min !== undefined) query.min = search.min
  if (search.max !== undefined) query.max = search.max
  if (search.ref !== undefined && search.ref !== '') {
    query.ref = walletRefForQuery(search.ref)
  }
  return query
}

/** 是否存在任何生效筛选（用于区分两种空态）。 */
export function hasWalletFilters(search: WalletTransactionsSearch): boolean {
  return (
    search.type !== undefined ||
    search.status !== undefined ||
    search.dir !== undefined ||
    search.from !== undefined ||
    search.to !== undefined ||
    search.min !== undefined ||
    search.max !== undefined ||
    search.ref !== undefined
  )
}

/** 除分页外的筛选重置（保留当前 page_size）。 */
export function clearWalletFilters(
  search: WalletTransactionsSearch,
): WalletTransactionsSearch {
  return {
    tab: search.tab,
    p: 1,
    page_size: search.page_size,
  }
}
