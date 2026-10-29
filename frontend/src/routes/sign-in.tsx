import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SignInPage } from '@/features/auth/SignInPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface SignInSearch {
  redirect?: string
  reason?: string
}

function validateSignInSearch(search: Record<string, unknown>): SignInSearch {
  const result: SignInSearch = {}
  if (typeof search.redirect === 'string') result.redirect = search.redirect
  if (typeof search.reason === 'string') result.reason = search.reason
  return result
}

export const Route = createFileRoute('/sign-in')({
  validateSearch: validateSignInSearch,
  // Load the domain chunk before first render so `t()` never flashes key names.
  loader: () => loadRouteDictionary('auth'),
  component: SignInRoute,
})

function SignInRoute() {
  const { redirect, reason } = Route.useSearch()
  return <SignInPage redirect={redirect} reason={reason} onNavigate={navigateTo} />
}
