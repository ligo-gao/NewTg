/**
 * 通知模板 / 投递记录的前端类型。
 *
 * 字段逐字对应 backend/internal/notify/service.go 的 `TemplateView` 与
 * `DeliveryView`（真实 DTO），不采用 UI 设计文档里更理想的契约：
 * - 模板行是 key × channel × locale 的**行**，不是 key 的聚合；
 * - `customized` 是 DB 的 `is_modified`，`updated_at` 可为 null（未落库的
 *   内置默认行）；
 * - 投递记录没有 attempt / next_retry_at / dedupe_key（见 G-NOTIFY-02）。
 */

/** 模板载体。投递表的 `webhook` 不走模板，见 §3.9.2 末的两处枚举差异。 */
export type NotificationTemplateChannel = 'email' | 'inapp'

/** 投递记录渠道。站内通知不落投递记录，故只有 email / webhook。 */
export type NotificationDeliveryChannel = 'email' | 'webhook'

/** 服务端 canonical 7 语，顺序即列表排序与下拉顺序（notify.localeOrder）。 */
export const NOTIFICATION_LOCALES = [
  'en',
  'zh-CN',
  'zh-TW',
  'fr',
  'ru',
  'ja',
  'vi',
] as const

export type NotificationLocale = (typeof NOTIFICATION_LOCALES)[number]

export function isNotificationLocale(value: unknown): value is NotificationLocale {
  return (
    typeof value === 'string' &&
    (NOTIFICATION_LOCALES as readonly string[]).includes(value)
  )
}

export interface NotificationTemplate {
  key: string
  category: string
  channel: NotificationTemplateChannel
  locale: NotificationLocale
  /** email 必填；inapp 恒为空字符串（服务端契约）。 */
  subject: string
  body: string
  cta_text?: string
  cta_url?: string
  enabled: boolean
  /** 对应 DB `is_modified`：是否偏离内置默认文案。 */
  customized: boolean
  /** 乐观锁令牌；未落库的内置默认行是 null。 */
  updated_at: string | null
}

export interface NotificationTemplateListResponse {
  items: NotificationTemplate[]
}

export interface UpdateTemplateInput {
  locale: NotificationLocale
  /**
   * 多渠道 key（billing.balance_low / billing.quota_exhausted）必须显式传；
   * 单渠道 key 服务端可推断，但前端始终传行上的 channel，避免歧义。
   */
  channel: NotificationTemplateChannel
  subject: string
  body: string
  cta_text: string
  cta_url: string
  /** 省略表示保持当前值；前端总是显式发送开关状态。 */
  enabled?: boolean
  /** PUT 的乐观锁令牌，null 表示该行尚未落库。 */
  expect_updated_at: string | null
}

export interface PreviewTemplateInput {
  locale: NotificationLocale
  channel: NotificationTemplateChannel
  /** 覆盖注册表样例值；空对象即全部使用服务端样例。 */
  variables: Record<string, unknown>
}

export interface PreviewTemplateResponse {
  subject: string
  body: string
}

export interface TestSendInput {
  locale: NotificationLocale
  channel: NotificationTemplateChannel
  variables: Record<string, unknown>
  recipient: string
}

/**
 * 202 响应。`status` 是 API 口径（queued → pending）；测试发送只会是
 * sent / failed，但类型按服务端词汇表完整声明。
 */
export interface TestSendResponse {
  id: number
  status: DeliveryStatus
  error: string | null
}

export type DeliveryStatus =
  | 'pending'
  | 'sent'
  | 'failed'
  | 'bounced'
  | 'suppressed'

export const DELIVERY_STATUSES = [
  'pending',
  'sent',
  'failed',
  'bounced',
  'suppressed',
] as const

export function isDeliveryStatus(value: unknown): value is DeliveryStatus {
  return (
    typeof value === 'string' &&
    (DELIVERY_STATUSES as readonly string[]).includes(value)
  )
}

export interface NotificationDelivery {
  id: number
  template_key?: string
  category: string
  channel: NotificationDeliveryChannel
  recipient: string
  locale?: string
  status: DeliveryStatus | 'queued'
  error?: string
  created_at: string
}

export interface DeliveryListResponse {
  items: NotificationDelivery[]
  total: number
  page: number
  page_size: number
}

export interface TemplateListParams {
  /**
   * 单值筛选。分类多选由前端在返回集上过滤：冻结契约只接受一个
   * `category` 参数，重复传参会被 `query.Get` 折叠成首个值。
   */
  category?: string
  channel?: NotificationTemplateChannel
  locale?: NotificationLocale
  modifiedOnly?: boolean
}

export interface DeliveryListParams {
  page: number
  pageSize: number
  category?: string
  channel?: NotificationDeliveryChannel
  status?: DeliveryStatus
}
