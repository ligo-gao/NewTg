import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { ForgotPasswordPage } from '@/features/auth/ForgotPasswordPage'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export const Route = createFileRoute('/forgot-password')({
  loader: () => loadDictionary('en', 'auth'),
  component: () => <ForgotPasswordPage onNavigate={navigateTo} />,
})
