import { createFileRoute } from '@tanstack/react-router'

import { ErrorStatusPage } from '@/app/error-status-page'

export interface ForbiddenSearch {
  redirect?: string
}

function validateForbiddenSearch(
  search: Record<string, unknown>,
): ForbiddenSearch {
  return typeof search.redirect === 'string' ? { redirect: search.redirect } : {}
}

/**
 * 403 only serves "signed in but unauthorized" (modules-09 A-2). The session
 * probe inside `ErrorStatusPage` sends the user to `/sign-in` when `/me` turns
 * 401 while this page is mounted; anonymous deep links never land here (the
 * console guards redirect them straight to sign-in).
 */
export const Route = createFileRoute('/_errors/403')({
  validateSearch: validateForbiddenSearch,
  component: ForbiddenRoute,
})

function ForbiddenRoute() {
  const { redirect } = Route.useSearch()
  return (
    <ErrorStatusPage
      status={403}
      redirect={redirect}
      onSessionExpired={(redirectTo) => {
        // 会话已死，整页导航到登录页（错误页语境下整页导航比 SPA 跳转更干净）。
        window.location.assign(
          `/sign-in?redirect=${encodeURIComponent(redirectTo)}`,
        )
      }}
    />
  )
}
