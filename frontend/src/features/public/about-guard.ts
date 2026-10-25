/**
 * `/about` 访问守卫（modules-09 F-09-29/F-09-35），与 pricing/rankings 守卫同构：
 * - `nav.about.enabled=false` → 静默回首页（公开页禁用态，不是 403）；
 * - `require_auth=true` 且未登录 → `/sign-in?redirect=<pathname+search>`；
 * - `/api/status` 不可用 → 安全默认放行（公开页不能因配置探测失败整页不可用）。
 */

import type { QueryClient } from '@tanstack/react-query'

import { navEntryOf, signInHref } from '@/features/public/nav-visibility'
import { fetchPublicStatus } from '@/lib/public-status'
import { fetchMe } from '@/lib/session'

export async function aboutAccessRedirect(
  queryClient: QueryClient,
  pathname: string,
  search: string,
): Promise<string | null> {
  const status = await fetchPublicStatus(queryClient)
  if (status === null) return null

  const entry = navEntryOf(status.nav, 'about')
  if (!entry.enabled) return '/'
  if (!entry.require_auth) return null

  // 只有 require_auth 才探测会话；会话失败按未登录处理（可恢复），
  // 放行受限内容则不可恢复。
  let me = null
  try {
    me = await fetchMe(queryClient)
  } catch {
    me = null
  }
  return me === null ? signInHref(pathname, search) : null
}
