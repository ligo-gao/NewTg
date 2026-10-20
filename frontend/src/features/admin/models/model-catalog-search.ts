/**
 * `/admin/models` 的 URL 查询参数与权限判定。
 *
 * 这个模块刻意只依赖类型，不 import 页面/API 客户端：路由文件的
 * `validateSearch` 与 `beforeLoad` 会随入口 chunk 执行，把整页（两个
 * 表格、三个抽屉、MSW 不参与的生产代码）拖进首屏会破坏 web-00 §7.2 的
 * 体积预算（与 `features/admin/redemption-codes/redemption-search.ts`
 * 同一理由）。
 *
 * 双 Tab 的筛选各自独立：所有键按 Tab 加 `mv_` / `vv_` 前缀。切换 Tab
 * 只改 `tab`，另一侧的筛选原样保留，服务端的两次查询因而互不干扰
 * （D1d 任务书 §1「Tab 切换保留各自筛选」）。
 */

export type ModelCatalogTab = 'models' | 'vendors'

export const MODEL_SQUARE_STATES = [
  'visible',
  'partial',
  'unavailable',
  'hidden',
] as const
export type ModelSquareState = (typeof MODEL_SQUARE_STATES)[number]

export const MODEL_SORTS = [
  'model_name',
  '-model_name',
  'status',
  '-status',
  'updated_at',
  '-updated_at',
  'created_at',
  '-created_at',
] as const
export type ModelSort = (typeof MODEL_SORTS)[number]

export const VENDOR_SORTS = [
  'name',
  '-name',
  'model_count',
  '-model_count',
  'updated_at',
  '-updated_at',
  'created_at',
  '-created_at',
] as const
export type VendorSort = (typeof VENDOR_SORTS)[number]

export type CatalogStatusFilter = 'enabled' | 'disabled'
export type VendorAttachedFilter = 'attached' | 'unattached'

export interface ModelCatalogSearch {
  tab: ModelCatalogTab
  // models Tab
  mv_p: number
  mv_page_size: number
  mv_keyword?: string
  mv_vendor_id?: number
  mv_status?: CatalogStatusFilter
  mv_square_state?: ModelSquareState
  mv_sort: ModelSort
  // vendors Tab
  vv_p: number
  vv_page_size: number
  vv_keyword?: string
  vv_status?: CatalogStatusFilter
  vv_attached?: VendorAttachedFilter
  vv_sort: VendorSort
}

export const MODEL_PAGE_SIZES = [10, 20, 50] as const

export const DEFAULT_MODEL_CATALOG_SEARCH: ModelCatalogSearch = {
  tab: 'models',
  mv_p: 1,
  mv_page_size: 20,
  mv_sort: 'model_name',
  vv_p: 1,
  vv_page_size: 20,
  vv_sort: 'name',
}

function positiveInt(value: unknown): number | undefined {
  const parsed = typeof value === 'string' ? Number(value) : value
  if (typeof parsed !== 'number' || !Number.isInteger(parsed) || parsed < 1) {
    return undefined
  }
  return parsed
}

/**
 * `page_size` 只接受组件暴露的档位（10/20/50）。地址栏里可能是任意整数
 * （服务端上限 100，且 37 这类值不会出现在下拉选项里，`<select>` 会显示成
 * 空白），因此非档位值一律回落到默认值，保证发出的请求与控件状态一致。
 */
function pageSizeOf(value: unknown): number {
  const parsed = positiveInt(value)
  if (parsed === undefined) return DEFAULT_MODEL_CATALOG_SEARCH.mv_page_size
  return (MODEL_PAGE_SIZES as readonly number[]).includes(parsed)
    ? parsed
    : DEFAULT_MODEL_CATALOG_SEARCH.mv_page_size
}

function oneOf<T extends string>(
  value: unknown,
  allowed: readonly T[],
): T | undefined {
  return typeof value === 'string' && (allowed as readonly string[]).includes(value)
    ? (value as T)
    : undefined
}

function keywordOf(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined
  // 关键字上限与服务端 ILIKE 参数的 128 rune 校验对齐，避免把注定 400
  // 的值发出去（web-00 §5.5：地址栏永远不是可信输入）。
  const trimmed = value.trim().slice(0, 128)
  return trimmed === '' ? undefined : trimmed
}

/**
 * 地址栏归一：非法枚举、非整数、负数与超长关键字都在这里降级为默认值，
 * 页面拿到的 search 始终是可发给服务端的形状。
 */
export function normalizeModelCatalogSearch(
  raw: Record<string, unknown>,
): ModelCatalogSearch {
  const search: ModelCatalogSearch = { ...DEFAULT_MODEL_CATALOG_SEARCH }

  if (raw.tab === 'vendors') search.tab = 'vendors'

  search.mv_p = positiveInt(raw.mv_p) ?? 1
  search.mv_page_size = pageSizeOf(raw.mv_page_size)
  const mvKeyword = keywordOf(raw.mv_keyword)
  if (mvKeyword !== undefined) search.mv_keyword = mvKeyword
  const mvVendor = positiveInt(raw.mv_vendor_id)
  if (mvVendor !== undefined) search.mv_vendor_id = mvVendor
  const mvStatus = oneOf(raw.mv_status, ['enabled', 'disabled'] as const)
  if (mvStatus !== undefined) search.mv_status = mvStatus
  const mvSquare = oneOf(raw.mv_square_state, MODEL_SQUARE_STATES)
  if (mvSquare !== undefined) search.mv_square_state = mvSquare
  search.mv_sort = oneOf(raw.mv_sort, MODEL_SORTS) ?? 'model_name'

  search.vv_p = positiveInt(raw.vv_p) ?? 1
  search.vv_page_size = pageSizeOf(raw.vv_page_size)
  const vvKeyword = keywordOf(raw.vv_keyword)
  if (vvKeyword !== undefined) search.vv_keyword = vvKeyword
  const vvStatus = oneOf(raw.vv_status, ['enabled', 'disabled'] as const)
  if (vvStatus !== undefined) search.vv_status = vvStatus
  const vvAttached = oneOf(raw.vv_attached, ['attached', 'unattached'] as const)
  if (vvAttached !== undefined) search.vv_attached = vvAttached
  search.vv_sort = oneOf(raw.vv_sort, VENDOR_SORTS) ?? 'name'

  return search
}

export function hasModelFilters(search: ModelCatalogSearch): boolean {
  return (
    search.mv_keyword !== undefined ||
    search.mv_vendor_id !== undefined ||
    search.mv_status !== undefined ||
    search.mv_square_state !== undefined ||
    search.mv_sort !== 'model_name'
  )
}

export function hasVendorFilters(search: ModelCatalogSearch): boolean {
  return (
    search.vv_keyword !== undefined ||
    search.vv_status !== undefined ||
    search.vv_attached !== undefined ||
    search.vv_sort !== 'name'
  )
}

/**
 * 权限键与 `backend/internal/rbac/catalog.go` 逐字对齐。`*` 代表
 * ADMIN_PERMS_FULL（数据库 NULL 在 /api/me 上投影为 `["*"]`）。
 *
 * 前端判定只是 UX 层，服务端仍逐请求强制（web-00 §9.1）。
 */
export function hasPermission(
  permissions: readonly string[] | undefined,
  key: string,
): boolean {
  return (
    permissions?.some(
      (permission) => permission === '*' || permission === key,
    ) ?? false
  )
}

export function canReadCatalog(
  permissions: readonly string[] | undefined,
): boolean {
  return hasPermission(permissions, 'channels:read')
}

export function canManageCatalog(
  permissions: readonly string[] | undefined,
): boolean {
  return hasPermission(permissions, 'channels:manage')
}

export function canManageCatalogSensitive(
  permissions: readonly string[] | undefined,
): boolean {
  return hasPermission(permissions, 'channels:manage_sensitive')
}

export function canManagePricing(
  permissions: readonly string[] | undefined,
): boolean {
  return hasPermission(permissions, 'pricing:manage')
}
