/**
 * 站内通知（inbox）传输层（A5 契约，见 `api/schema.d.ts`）。
 *
 * - `GET /api/notifications` 恒为当前登录用户，服务端不支持按 user_id 查询；
 * - `POST /api/notifications/{id}/read` 幂等，重复调用仍返回 204；
 * - 契约没有批量已读端点，也不返回未读总数（`total` 是全部通知数），
 *   所以徽章 / 「全部已读」只能在前端按页聚合。这里集中放路径与分页常量，
 *   避免铃铛、面板各自写错 page_size。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'

export type UserNotification = components['schemas']['UserNotification']
export type UserNotificationList = components['schemas']['UserNotificationList']
export type UserNotificationListQuery = NonNullable<
  operations['listUserNotifications']['parameters']['query']
>

/** 面板每页条数：与服务端默认 20 一致，分页语义直观。 */
export const NOTIFICATION_PAGE_SIZE = 20
/**
 * 徽章的一次性探测窗口。取服务端上限 100：未读数只能从 item.read_at
 * 推导，窗口越大漏计越少；超过窗口的未读会显示为 99+，而不是谎报精确值。
 */
export const NOTIFICATION_COUNT_PAGE_SIZE = 100
/** 徽章封顶展示；精确数仍由 aria-live 文本给出（同样封顶，避免误导）。 */
export const NOTIFICATION_BADGE_CAP = 99
/** 「全部已读」逐条遍历时的页数保护，防止异常 total 造成无限循环。 */
const MAX_PAGES = 1_000

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

/** 幂等标记本人一条通知已读；他人/不存在统一 404，由 ApiError 表达。 */
export async function markUserNotificationRead(id: number): Promise<void> {
  await apiClient.POST('/api/notifications/{id}/read', {
    params: { path: { id } },
  })
}

/**
 * 汇总全部未读 id。服务端无批量端点，也没有 `read=false` 过滤参数，因此
 * 只能翻完全部分页后逐条 POST。通知量级由公告广播决定（低频），但这里
 * 仍加页数上限，异常数据下宁可少标也不能卡住面板。
 */
export async function collectUnreadNotificationIds(
  signal?: AbortSignal,
): Promise<number[]> {
  const ids: number[] = []
  let page = 1
  let seen = 0
  let total = 0

  do {
    const data = await listUserNotifications(
      { p: page, page_size: NOTIFICATION_COUNT_PAGE_SIZE },
      signal,
    )
    total = data.total
    seen += data.items.length
    for (const item of data.items) {
      if (item.read_at === null || item.read_at === undefined) ids.push(item.id)
    }
    // 空页说明服务端 total 与实际行数不一致；继续翻只会重复空请求。
    if (data.items.length === 0) break
    page += 1
  } while (seen < total && page <= MAX_PAGES)

  return ids
}
