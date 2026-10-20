/** 公告相关的 React Query 缓存键。集中一处，避免管理页与用户弹层各自写错。 */
export const ANNOUNCEMENT_LIST_QUERY_KEY = ['announcements', 'list'] as const
export const ANNOUNCEMENT_ACTIVE_QUERY_KEY = ['announcements', 'active'] as const
/**
 * 管理端详情缓存。保存成功后由调用方 `setQueryData` 写入服务端返回值，
 * 否则 `staleTime: Infinity` 会让抽屉重开时继续用旧正文初始化表单。
 */
export const ANNOUNCEMENT_DETAIL_QUERY_KEY = ['announcements', 'detail'] as const

export function announcementDetailQueryKey(id: string | null) {
  return [...ANNOUNCEMENT_DETAIL_QUERY_KEY, id] as const
}
