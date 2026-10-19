import { useQueryClient } from '@tanstack/react-query'

import {
  PUBLIC_STATUS_QUERY_KEY,
  type PublicStatus,
} from '@/lib/public-status'

/**
 * `nav.modules.chat.playground` 的可见性（modules-02 §2.15）。
 *
 * 站点设置缺省为 true：缺键（旧后端/稀疏投影）时一律视为开启，否则一次
 * 版本错配就会把功能藏起来。该键**只控制前端可见性**，不是接口授权——
 * 关闭时 `/pg/*` 仍由会话鉴权、用户分组与余额约束判定。
 */
export function isPlaygroundEnabled(
  status: PublicStatus | null | undefined,
): boolean {
  return status?.nav?.modules?.chat?.playground !== false
}

/**
 * 控制台侧栏用：只读根守卫已经写入的 `['public','status']` 缓存，不再发
 * 请求。这样 ConsoleHeader 在单测里（没有 status 处理器）不会额外打网络，
 * 而真实导航中根守卫已在进入控制台前填好缓存。
 */
export function usePlaygroundEnabled(): boolean {
  const queryClient = useQueryClient()
  const status = queryClient.getQueryData<PublicStatus>(PUBLIC_STATUS_QUERY_KEY)
  return isPlaygroundEnabled(status)
}
