import type { QueryClient } from '@tanstack/react-query'

import type {
  UserNotificationList,
  UserNotificationUnreadCount,
} from './api'

/**
 * 通知缓存统一前缀。铃铛的未读计数、面板列表共享该前缀，单条已读/全部
 * 已读时一次前缀失效就能让两边都以服务端为准。
 */
export const NOTIFICATIONS_QUERY_PREFIX = ['notifications'] as const

export function notificationsListQueryKey(page: number, pageSize: number) {
  return [...NOTIFICATIONS_QUERY_PREFIX, 'list', page, pageSize] as const
}

/**
 * 未读计数缓存键。与列表前缀不同（第二段是 `unread-count`），因此更新列表
 * 的 `setQueriesData` 必须显式收窄到 `'list'` 子前缀，绝不能误把计数对象
 * 当成列表对象去读 `.items`。
 */
export const notificationsUnreadCountQueryKey = [
  ...NOTIFICATIONS_QUERY_PREFIX,
  'unread-count',
] as const

/** 列表缓存子前缀：单条已读的本地乐观更新只应命中分页结果。 */
const NOTIFICATIONS_LIST_QUERY_PREFIX = [
  ...NOTIFICATIONS_QUERY_PREFIX,
  'list',
] as const

/**
 * 在已缓存的分页结果里把一条标记为已读（本地乐观更新），并同步把未读计数
 * 减一（不依赖下一次服务端刷新）。
 *
 * `read_at` 用当前时间占位：UI 只判断 null/非 null，具体时刻下次
 * invalidate 后由服务端值覆盖；这样徽章与列表能在请求成功时立即一致。
 */
export function applyNotificationRead(
  queryClient: QueryClient,
  id: number,
  now: string = new Date().toISOString(),
): void {
  queryClient.setQueriesData<UserNotificationList>(
    { queryKey: NOTIFICATIONS_LIST_QUERY_PREFIX },
    (previous) => {
      if (previous === undefined) return previous
      let changed = false
      const items = previous.items.map((item) => {
        // Schema 的 `read_at?` 在未读时可能省略，不能只判 null。
        if (
          item.id !== id ||
          (item.read_at !== null && item.read_at !== undefined)
        ) {
          return item
        }
        changed = true
        return { ...item, read_at: now }
      })
      return changed ? { ...previous, items } : previous
    },
  )
  queryClient.setQueryData<UserNotificationUnreadCount | undefined>(
    notificationsUnreadCountQueryKey,
    (previous) =>
      previous === undefined
        ? previous
        : { unread: Math.max(0, previous.unread - 1) },
  )
}

/** 「全部已读」成功后未读计数归零（列表由随后 invalidate 刷新）。 */
export function applyAllNotificationsRead(queryClient: QueryClient): void {
  queryClient.setQueryData<UserNotificationUnreadCount | undefined>(
    notificationsUnreadCountQueryKey,
    (previous) => (previous === undefined ? previous : { unread: 0 }),
  )
}
