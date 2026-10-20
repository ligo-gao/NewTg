/** 公告相关的 React Query 缓存键。集中一处，避免管理页与用户弹层各自写错。 */
export const ANNOUNCEMENT_LIST_QUERY_KEY = ['announcements', 'list'] as const
export const ANNOUNCEMENT_ACTIVE_QUERY_KEY = ['announcements', 'active'] as const
