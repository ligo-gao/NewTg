import { createFileRoute } from '@tanstack/react-router'

import { ServerErrorPage } from '@/app/error-status-page'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'

export interface ServerErrorSearch {
  redirect?: string
}

function validateServerErrorSearch(
  search: Record<string, unknown>,
): ServerErrorSearch {
  return typeof search.redirect === 'string' ? { redirect: search.redirect } : {}
}

/**
 * F-09-12 `/500`. `?redirect=` carries the path of the request that failed so
 * 「重试」 can re-send it; without it there is no original request and the retry
 * button is hidden (modules-09 按钮表). `request_id` never travels in the URL
 * (web-00 §8.4); route-level render errors reach this page through
 * `ErrorFallback`, which reads it from the error object instead.
 */
export const Route = createFileRoute('/_errors/500')({
  validateSearch: validateServerErrorSearch,
  component: ServerErrorRoute,
})

function ServerErrorRoute() {
  const { redirect } = Route.useSearch()
  // 回跳目标是 URL 参数，必须按登录回跳同一口径清洗，防止站外跳转。
  const target = redirect === undefined || redirect === '' ? undefined : sanitizeAuthRedirect(redirect)
  return (
    <ServerErrorPage
      retryable={target !== undefined}
      onRetry={
        target !== undefined
          ? () => {
              // 整页导航重发原请求；错误页本身处于坏掉的 SPA 语境。
              window.location.assign(target)
            }
          : undefined
      }
    />
  )
}
