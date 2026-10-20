/**
 * 公告管理页的 URL search 契约。
 *
 * 筛选与分页全部进 URL（可分享、可后退）：`status` / `category` / `p` /
 * `page_size`。parse 时逐项对冻结集合做白名单归一——服务端对未知
 * status/category 直接 400，前端绝不把自由文本透传过去。
 */
import type {
  AnnouncementCategory,
  AnnouncementDisplayStatus,
} from '@/features/announcements/api'

export const ANNOUNCEMENT_STATUS_FILTERS: readonly AnnouncementDisplayStatus[] = [
  'draft',
  'published',
  'expired',
  'offline',
]

export const ANNOUNCEMENT_CATEGORIES: readonly AnnouncementCategory[] = [
  'notice',
  'maintenance',
  'release',
]

export const ANNOUNCEMENT_PAGE_SIZES = [10, 20, 50] as const
export type AnnouncementPageSize = (typeof ANNOUNCEMENT_PAGE_SIZES)[number]
export const DEFAULT_ANNOUNCEMENT_PAGE_SIZE: AnnouncementPageSize = 20

export interface AnnouncementsSearch {
  status?: AnnouncementDisplayStatus
  category?: AnnouncementCategory
  p?: number
  page_size?: AnnouncementPageSize
}

function isStatus(value: unknown): value is AnnouncementDisplayStatus {
  return (
    typeof value === 'string' &&
    (ANNOUNCEMENT_STATUS_FILTERS as readonly string[]).includes(value)
  )
}

function isCategory(value: unknown): value is AnnouncementCategory {
  return (
    typeof value === 'string' &&
    (ANNOUNCEMENT_CATEGORIES as readonly string[]).includes(value)
  )
}

function isPageSize(value: unknown): value is AnnouncementPageSize {
  return (
    typeof value === 'number' &&
    (ANNOUNCEMENT_PAGE_SIZES as readonly number[]).includes(value)
  )
}

function positiveInt(value: unknown): number | undefined {
  const parsed = typeof value === 'number' ? value : Number(value)
  if (!Number.isInteger(parsed) || parsed < 1) return undefined
  return parsed
}

export function parseAnnouncementsSearch(
  raw: Record<string, unknown>,
): AnnouncementsSearch {
  const search: AnnouncementsSearch = {}
  if (isStatus(raw.status)) search.status = raw.status
  if (isCategory(raw.category)) search.category = raw.category
  const page = positiveInt(raw.p)
  if (page !== undefined) search.p = page
  // URL 的 search 值可能是字符串；只接受 UI 下拉里真实存在的档位。
  const pageSize =
    typeof raw.page_size === 'string' ? Number(raw.page_size) : raw.page_size
  if (isPageSize(pageSize)) search.page_size = pageSize
  return search
}
