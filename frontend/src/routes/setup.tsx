import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { SetupWizard } from '@/features/auth/SetupWizard'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export const Route = createFileRoute('/setup')({
  loader: () => loadDictionary('en', 'auth'),
  component: () => <SetupWizard onNavigate={navigateTo} />,
})
