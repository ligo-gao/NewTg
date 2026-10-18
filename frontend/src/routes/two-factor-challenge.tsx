import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { TwoFactorChallengePage } from '@/features/auth/TwoFactorChallengePage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface ChallengeSearch {
  ticket?: string
  redirect?: string
}

function validateChallengeSearch(search: Record<string, unknown>): ChallengeSearch {
  const result: ChallengeSearch = {}
  if (typeof search.ticket === 'string') result.ticket = search.ticket
  if (typeof search.redirect === 'string') result.redirect = search.redirect
  return result
}

export const Route = createFileRoute('/two-factor-challenge')({
  validateSearch: validateChallengeSearch,
  loader: () => loadDictionary('en', 'auth'),
  component: ChallengeRoute,
})

function ChallengeRoute() {
  const { ticket, redirect } = Route.useSearch()
  return (
    <TwoFactorChallengePage
      ticket={ticket}
      redirect={redirect}
      onNavigate={navigateTo}
    />
  )
}
