import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import {
  readOptionalSiteConfig,
  ServiceUnavailablePage,
} from '@/app/error-status-page'
import { usePublicStatus } from '@/lib/public-status'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { isSafePublicHref } from '@/features/public/nav-visibility'

export interface ServiceUnavailableSearch {
  redirect?: string
}

function validateServiceUnavailableSearch(
  search: Record<string, unknown>,
): ServiceUnavailableSearch {
  return typeof search.redirect === 'string' ? { redirect: search.redirect } : {}
}

/**
 * F-09-13 `/503`（依赖就绪失败 / 维护窗口）。
 *
 * `site.status_page_url` 仍属【假设】字段：后端未下发时按钮必须隐藏，不能
 * 指向可能不存在的页面。`?redirect=` 存在时「重试」重发原请求，否则整页
 * reload —— 503 的语义本身就是「服务会恢复，稍后再来」，reload 是有效的
 * 手动重试。
 */
export const Route = createFileRoute('/_errors/503')({
  validateSearch: validateServiceUnavailableSearch,
  loader: () => loadRouteDictionary('public'),
  component: ServiceUnavailableRoute,
})

function ServiceUnavailableRoute() {
  const { redirect } = Route.useSearch()
  const status = usePublicStatus()
  const config = readOptionalSiteConfig(status.data)
  // 同登录回跳口径清洗，避免 `?redirect=//evil.com` 成为开放跳转。
  const target =
    redirect === undefined || redirect === ''
      ? undefined
      : sanitizeAuthRedirect(redirect)

  // Same client-side scheme rule as the public footer: a stored
  // `javascript:`/protocol-relative URL must never become a link target.
  const statusPageUrl = isSafePublicHref(config.statusPageUrl)
    ? config.statusPageUrl
    : undefined

  return (
    <ServiceUnavailablePage
      statusPageUrl={statusPageUrl}
      onRetry={() => {
        if (target !== undefined) {
          window.location.assign(target)
          return
        }
        window.location.reload()
      }}
    />
  )
}
