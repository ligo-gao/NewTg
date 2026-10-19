import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'

import { ErrorStatusPage } from '@/app/error-status-page'
import { loadRouteDictionary } from '@/app/providers/i18n'
import { takeUnauthorizedRequestId } from '@/app/providers/auth-failures'

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
  // The shared error shell now renders the public chrome, whose copy lives in
  // the `public` domain; the error strings themselves stay in `common`.
  loader: () => loadRouteDictionary('public'),
  component: UnauthorizedRoute,
})

function UnauthorizedRoute() {
  const { redirect } = Route.useSearch()
  // A runtime 401 is detected deep inside a query, where the failing error
  // carries `request_id`. `reportServerError` stashes it in sessionStorage
  // (never the URL, §8.4); this page consumes and clears it exactly once.
  const [requestId] = useState(() => takeUnauthorizedRequestId())
  return <ErrorStatusPage status={401} redirect={redirect} requestId={requestId} />
}
