import type { QueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { clearLanguageChoice } from '@/lib/language-preference'

/**
 * Revokes the current server session, then drops every cached identity so the
 * next page cannot render the previous user's data, and finally sends the
 * browser home.
 *
 * The caller acts on `false` only: a failed revoke leaves the HttpOnly cookie
 * (and therefore the session) alive, so pretending the user signed out would
 * be a lie. `navigate` is injected so the full-page browser hop can be
 * observed without loading a real document.
 */
export async function performSignOut(
  client: QueryClient,
  navigate: (href: string) => void = (href) => window.location.assign(href),
): Promise<boolean> {
  try {
    await apiClient.POST('/api/logout')
  } catch {
    return false
  }
  // 会话已经结束：这次登录期间的显式选择/待同步标记属于该账号，不能
  // 跟着下一个匿名访客继续存在。语言本身保留，匿名页面仍显示用户选过的值。
  clearLanguageChoice()
  client.clear()
  navigate('/')
  return true
}
