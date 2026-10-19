import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SecurityPage } from '@/features/account/SecurityPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface SecuritySearch {
  force?: string
}

function validateSecuritySearch(search: Record<string, unknown>): SecuritySearch {
  return typeof search.force === 'string' ? { force: search.force } : {}
}

export const Route = createFileRoute('/_console/security')({
  validateSearch: validateSecuritySearch,
  // The page reuses auth copy for password fields and the 2FA dialog.
  loader: () =>
    Promise.all([loadRouteDictionary('account'), loadRouteDictionary('auth')]),
  component: SecurityRoute,
})

function SecurityRoute() {
  const { force } = Route.useSearch()
  return (
    <SecurityPage forcePasswordChange={force === 'password'} onNavigate={navigateTo} />
  )
}
