/**
 * 兑换码页的 URL 查询参数与权限判定。
 *
 * 这个模块刻意只依赖类型，不 import 页面/API 客户端：路由文件的
 * `validateSearch` 与 `beforeLoad` 会随入口 chunk 执行，把整页（表格、
 * 抽屉、openapi 客户端）拖进首屏会破坏 web-00 §7.2 的体积预算
 * （与 `features/admin/plugins/tabs.ts` 同一理由）。
 */

/** 与服务端 `RedemptionCodeStatusFilter` 对齐；`expired` 是虚拟态。 */
export type RedemptionStatusFilter = 'unused' | 'disabled' | 'used' | 'expired'

const STATUS_FILTERS: readonly RedemptionStatusFilter[] = [
  'unused',
  'disabled',
  'used',
  'expired',
]

/** 契约 `page_size` 的冻结取值（UI §7.2 的「每页 20 ▾」）。 */
export type RedemptionPageSize = 10 | 20 | 50

export const REDEMPTION_PAGE_SIZES: readonly RedemptionPageSize[] = [10, 20, 50]

export const DEFAULT_REDEMPTION_PAGE_SIZE: RedemptionPageSize = 20

export interface RedemptionSearch {
  /** 页码，1 基。 */
  p: number
  /**
   * 每页条数。放进 URL 与 `p` 同理：刷新/分享链接后分页形状保持一致。
   * 省略即默认 20，因此最常见的链接不会被 `?size=20` 污染。
   */
  size?: RedemptionPageSize
  status?: RedemptionStatusFilter
  batch?: string
  min?: number
  max?: number
}

function positiveInt(value: unknown): number | undefined {
  const parsed = typeof value === 'string' ? Number(value) : value
  if (typeof parsed !== 'number' || !Number.isInteger(parsed) || parsed < 1) {
    return undefined
  }
  return parsed
}

function nonNegativeInt(value: unknown): number | undefined {
  const parsed = typeof value === 'string' ? Number(value) : value
  if (typeof parsed !== 'number' || !Number.isInteger(parsed) || parsed < 0) {
    return undefined
  }
  return parsed
}

function pageSize(value: unknown): RedemptionPageSize | undefined {
  const parsed = typeof value === 'string' ? Number(value) : value
  if (typeof parsed !== 'number' || !Number.isInteger(parsed)) return undefined
  return REDEMPTION_PAGE_SIZES.find((size) => size === parsed)
}

/**
 * 地址栏永远不是可信输入：非法枚举、非整数、负数与超长批次名在这里降级，
 * 页面拿到的 search 始终是可发给服务端的形状（web-00 §5.5）。批次名上限
 * 取契约的 `batch_name` maxLength 20，避免把注定 400 的值发出去。
 *
 * `page_size` 只接受契约下拉的三个白名单值：契约本身允许 1–100，但页面
 * 只提供 10/20/50，其他值一律降级为「省略」（渲染时取默认 20），否则
 * 手工改 URL 会得到一个下拉框里不存在的选中值。
 */
export function normalizeRedemptionSearch(
  raw: Record<string, unknown>,
): RedemptionSearch {
  const search: RedemptionSearch = { p: positiveInt(raw.p) ?? 1 }

  const size = pageSize(raw.size)
  if (size !== undefined) search.size = size

  if (
    typeof raw.status === 'string' &&
    (STATUS_FILTERS as readonly string[]).includes(raw.status)
  ) {
    search.status = raw.status as RedemptionStatusFilter
  }

  if (typeof raw.batch === 'string') {
    const batch = raw.batch.trim().slice(0, 20)
    if (batch !== '') search.batch = batch
  }

  const min = nonNegativeInt(raw.min)
  const max = nonNegativeInt(raw.max)
  if (min !== undefined) search.min = min
  if (max !== undefined) search.max = max

  return search
}

export function hasRedemptionFilters(search: RedemptionSearch): boolean {
  return (
    search.status !== undefined ||
    search.batch !== undefined ||
    search.min !== undefined ||
    search.max !== undefined
  )
}

/**
 * 写操作权限：入口与路由守卫都用 `redemption:manage`（modules-07 既有权限
 * 键，本批不新增）。服务端仍逐请求强制；这里的隐藏只是 UX（web-00 §9.1）。
 */
export function hasRedemptionManage(
  permissions: readonly string[] | undefined,
): boolean {
  return (
    permissions?.some(
      (permission) => permission === '*' || permission === 'redemption:manage',
    ) ?? false
  )
}
