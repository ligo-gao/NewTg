import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { ResetPasswordPage } from '@/features/auth/ResetPasswordPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export interface ResetSearch {
  token?: string
}

function validateResetSearch(search: Record<string, unknown>): ResetSearch {
  return typeof search.token === 'string' ? { token: search.token } : {}
}

export const Route = createFileRoute('/reset-password')({
  validateSearch: validateResetSearch,
  loader: () => loadDictionary('en', 'auth'),
  component: ResetPasswordRoute,
})

function ResetPasswordRoute() {
  const { token } = Route.useSearch()
  return <ResetPasswordPage token={token} onNavigate={navigateTo} />
}
