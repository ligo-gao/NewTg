/**
 * `/pricing` 与 `/pricing/$modelId` 的访问守卫（modules-09 F-09-14）。
 *
 * 与 `_console` 的会话闸门不同，定价是**可选公开内容**：
 * - `nav.pricing.enabled=false` → 静默回首页（不是 403，不提示）；
 * - `require_auth=true` 且未登录 → `/sign-in?redirect=<含 search 的完整路径>`；
 * - `/api/status` 不可用 → 采用安全默认（enabled=true / require_auth=false）
 *   继续渲染，由页面显示可关闭的配置提示条。公开页面不能因为配置探测失败
 *   而整页不可用（web-00 §4.4）。
 */

import type { QueryClient } from '@tanstack/react-query'

import { navEntryOf, signInHref } from '@/features/public/nav-visibility'
import { fetchPublicStatus } from '@/lib/public-status'
import { fetchMe } from '@/lib/session'

export async function pricingAccessRedirect(
  queryClient: QueryClient,
  pathname: string,
  search: string,
): Promise<string | null> {
  const status = await fetchPublicStatus(queryClient)
  if (status === null) return null

  const entry = navEntryOf(status.nav, 'pricing')
  if (!entry.enabled) return '/'
  if (!entry.require_auth) return null

  // 只有 require_auth 场景才需要会话探测：公开页默认不发多余请求。
  // 会话接口故障按「未登录」处理——把用户送到登录页是可恢复的，
  // 而放行受限内容不是。
  let me = null
  try {
    me = await fetchMe(queryClient)
  } catch {
    me = null
  }
  return me === null ? signInHref(pathname, search) : null
}
