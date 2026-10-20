/**
 * 站内通知（inbox）传输层（A5 契约，见 `api/schema.d.ts`）。
 *
 * - `GET /api/notifications` 恒为当前登录用户，服务端不支持按 user_id 查询；
 * - `GET /api/notifications/unread-count` 返回与列表同口径的未读数（含公告
 *   失效过滤），徽章与「全部已读」可用性都读这一个值，不再按页聚合；
 * - `POST /api/notifications/{id}/read` 幂等，重复调用仍返回 204；
 * - `POST /api/notifications/read-all` 单条 SQL 集合置读，返回本次更新行数；
 *   重复调用 updated=0，已读时刻保持首次值。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'

export type UserNotification = components['schemas']['UserNotification']
export type UserNotificationList = components['schemas']['UserNotificationList']
export type UserNotificationUnreadCount =
  components['schemas']['UserNotificationUnreadCount']
export type UserNotificationReadAllResult =
  components['schemas']['UserNotificationReadAllResult']
export type UserNotificationListQuery = NonNullable<
  operations['listUserNotifications']['parameters']['query']
>

/** 面板每页条数：与服务端默认 20 一致，分页语义直观。 */
export const NOTIFICATION_PAGE_SIZE = 20
/** 徽章封顶展示；精确数仍由 aria-live 文本给出（同样封顶，避免误导）。 */
export const NOTIFICATION_BADGE_CAP = 99

export async function listUserNotifications(
  query: UserNotificationListQuery,
  signal?: AbortSignal,
): Promise<UserNotificationList> {
  return requireData(
    await apiClient.GET('/api/notifications', {
      params: { query },
      signal,
    }),
  )
}

/** 本人未读数；计数与列表同口径（服务端保证），调用方无需再翻页聚合。 */
export async function getUserNotificationUnreadCount(
  signal?: AbortSignal,
): Promise<UserNotificationUnreadCount> {
  return requireData(
    await apiClient.GET('/api/notifications/unread-count', { signal }),
  )
}

/** 幂等标记本人一条通知已读；他人/不存在统一 404，由 ApiError 表达。 */
export async function markUserNotificationRead(id: number): Promise<void> {
  await apiClient.POST('/api/notifications/{id}/read', {
    params: { path: { id } },
  })
}

/**
 * 一次把本人全部可见未读置读。服务端以 `read_at IS NULL` 限定，重复调用
 * 更新 0 行；公告已失效的投递不参与更新。返回本次实际更新条数。
 */
export async function markAllUserNotificationsRead(): Promise<UserNotificationReadAllResult> {
  return requireData(
    await apiClient.POST('/api/notifications/read-all', {}),
  )
}
