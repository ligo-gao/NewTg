/**
 * 通知模板页的 URL search 契约。
 *
 * 筛选状态全部进 URL（可分享、可后退）：模板 Tab 用 `category`（逗号分隔的
 * 多选）、`channel`、`locale`、`modified`、`page`；投递记录 Tab 用 `d_` 前缀，
 * 两个 Tab 的筛选互不干扰。parse 时逐项对冻结集合做白名单归一，非法值直接
 * 丢弃，绝不把未知 key 送进后端（后端会 400）。
 */
import { NOTIFICATION_CATEGORY_KEYS, isNotificationCategory } from './registry'
import {
  DELIVERY_STATUSES,
  isDeliveryStatus,
  isNotificationLocale,
  type DeliveryStatus,
  type NotificationDeliveryChannel,
  type NotificationLocale,
  type NotificationTemplateChannel,
} from './types'

export type NotificationTemplatesTab = 'templates' | 'deliveries'

export interface NotificationTemplatesSearch {
  tab?: NotificationTemplatesTab
  /** 逗号分隔的 category 多选（契约只接受单值，多选在前端过滤）。 */
  category?: string
  channel?: NotificationTemplateChannel
  locale?: NotificationLocale
  modified?: boolean
  page?: number

  d_category?: string
  d_channel?: NotificationDeliveryChannel
  d_status?: DeliveryStatus
  d_page?: number
}

function positiveInt(value: unknown): number | undefined {
  const parsed = typeof value === 'number' ? value : Number(value)
  if (!Number.isInteger(parsed) || parsed < 1) return undefined
  return parsed
}

function templateChannel(value: unknown): NotificationTemplateChannel | undefined {
  return value === 'email' || value === 'inapp' ? value : undefined
}

function deliveryChannel(value: unknown): NotificationDeliveryChannel | undefined {
  return value === 'email' || value === 'webhook' ? value : undefined
}

/** 逗号分隔 → 去重后的合法 category 列表，保持注册表顺序。 */
export function categoriesFromSearch(value: string | undefined): string[] {
  if (value === undefined) return []
  const selected = new Set(value.split(',').filter(isNotificationCategory))
  return NOTIFICATION_CATEGORY_KEYS.filter((key) => selected.has(key))
}

export function categoriesToSearch(categories: readonly string[]): string | undefined {
  const ordered = NOTIFICATION_CATEGORY_KEYS.filter((key) => categories.includes(key))
  return ordered.length === 0 ? undefined : ordered.join(',')
}

export function parseNotificationTemplatesSearch(
  raw: Record<string, unknown>,
): NotificationTemplatesSearch {
  const search: NotificationTemplatesSearch = {}

  if (raw.tab === 'deliveries') search.tab = 'deliveries'
  if (typeof raw.category === 'string') {
    const categories = categoriesToSearch(categoriesFromSearch(raw.category))
    if (categories !== undefined) search.category = categories
  }
  const channel = templateChannel(raw.channel)
  if (channel !== undefined) search.channel = channel
  if (isNotificationLocale(raw.locale)) search.locale = raw.locale
  if (raw.modified === true || raw.modified === 'true') search.modified = true
  const page = positiveInt(raw.page)
  if (page !== undefined) search.page = page

  if (isNotificationCategory(raw.d_category)) search.d_category = raw.d_category
  const delivery = deliveryChannel(raw.d_channel)
  if (delivery !== undefined) search.d_channel = delivery
  if (isDeliveryStatus(raw.d_status)) search.d_status = raw.d_status
  const deliveryPage = positiveInt(raw.d_page)
  if (deliveryPage !== undefined) search.d_page = deliveryPage

  return search
}

/** 状态筛选下拉的合法值（API 口径，queued 已由服务端映射为 pending）。 */
export const DELIVERY_STATUS_FILTERS = DELIVERY_STATUSES
