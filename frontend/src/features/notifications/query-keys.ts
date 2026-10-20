import type { QueryClient } from '@tanstack/react-query'

import type { UserNotificationList } from './api'

/**
 * 通知缓存统一前缀。铃铛的未读探测与面板列表共享同一个前缀，标记已读时
 * 一次 `setQueriesData` 就能把两边都改掉，随后再 invalidate 以服务端为准。
 */
export const NOTIFICATIONS_QUERY_PREFIX = ['notifications'] as const

export function notificationsListQueryKey(page: number, pageSize: number) {
  return [...NOTIFICATIONS_QUERY_PREFIX, 'list', page, pageSize] as const
}

/**
 * 在已缓存的分页结果里把一条标记为已读（本地乐观更新）。
 *
 * `read_at` 用当前时间占位：UI 只判断 null/非 null，具体时刻下次
 * invalidate 后由服务端值覆盖；这样徽章能在请求成功前立即递减。
 */
export function applyNotificationRead(
  queryClient: QueryClient,
  id: number,
  now: string = new Date().toISOString(),
): void {
  queryClient.setQueriesData<UserNotificationList>(
    { queryKey: NOTIFICATIONS_QUERY_PREFIX },
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
}
