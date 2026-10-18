import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { SignInPage } from '@/features/auth/SignInPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface SignInSearch {
  redirect?: string
}

function validateSignInSearch(search: Record<string, unknown>): SignInSearch {
  return typeof search.redirect === 'string' ? { redirect: search.redirect } : {}
}

export const Route = createFileRoute('/sign-in')({
  validateSearch: validateSignInSearch,
  // Load the domain chunk before first render so `t()` never flashes key names.
  loader: () => loadDictionary('en', 'auth'),
  component: SignInRoute,
})

function SignInRoute() {
  const { redirect } = Route.useSearch()
  return <SignInPage redirect={redirect} onNavigate={navigateTo} />
}
