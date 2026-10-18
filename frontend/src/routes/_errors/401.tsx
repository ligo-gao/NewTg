import { createFileRoute } from '@tanstack/react-router'

import { ErrorStatusPage } from '@/app/error-status-page'

export interface UnauthorizedSearch {
  redirect?: string
}

function validateUnauthorizedSearch(
  search: Record<string, unknown>,
): UnauthorizedSearch {
  return typeof search.redirect === 'string' ? { redirect: search.redirect } : {}
}

/**
 * Runtime-session-expiry landing (web-00 §8.3/§9.4). The guard-driven
 * "not signed in" case never renders this page; it redirects straight to
 * `/sign-in`.
 */
export const Route = createFileRoute('/_errors/401')({
  validateSearch: validateUnauthorizedSearch,
  component: UnauthorizedRoute,
})

function UnauthorizedRoute() {
  const { redirect } = Route.useSearch()
  return <ErrorStatusPage status={401} redirect={redirect} />
}
