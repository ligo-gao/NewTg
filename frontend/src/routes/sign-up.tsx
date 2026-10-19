import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SignUpPage } from '@/features/auth/SignUpPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface SignUpSearch {
  redirect?: string
  aff?: string
}

function validateSignUpSearch(search: Record<string, unknown>): SignUpSearch {
  const result: SignUpSearch = {}
  if (typeof search.redirect === 'string') result.redirect = search.redirect
  if (typeof search.aff === 'string') result.aff = search.aff
  return result
}

export const Route = createFileRoute('/sign-up')({
  validateSearch: validateSignUpSearch,
  loader: () => loadRouteDictionary('auth'),
  component: SignUpRoute,
})

function SignUpRoute() {
  const { redirect, aff } = Route.useSearch()
  return <SignUpPage redirect={redirect} aff={aff} onNavigate={navigateTo} />
}
