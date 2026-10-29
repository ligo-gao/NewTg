import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { TwoFactorEnrollmentPage } from '@/features/auth/TwoFactorEnrollmentPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface EnrollSearch {
  ticket?: string
  redirect?: string
}

function validateEnrollSearch(search: Record<string, unknown>): EnrollSearch {
  const result: EnrollSearch = {}
  if (typeof search.ticket === 'string') result.ticket = search.ticket
  if (typeof search.redirect === 'string') result.redirect = search.redirect
  return result
}

/**
 * R-04: unbound administrators are sent here after sign-in. The page drives
 * the ticket-authenticated enrollment API and finishes with a full document
 * navigation, so the fresh session cookie is used by every later request.
 */
export const Route = createFileRoute('/two-factor-enroll')({
  validateSearch: validateEnrollSearch,
  loader: () => loadRouteDictionary('auth'),
  component: EnrollRoute,
})

function EnrollRoute() {
  const { ticket, redirect } = Route.useSearch()
  return (
    <TwoFactorEnrollmentPage
      ticket={ticket}
      redirect={redirect}
      onNavigate={navigateTo}
    />
  )
}
