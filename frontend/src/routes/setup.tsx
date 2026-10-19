import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SetupWizard } from '@/features/auth/SetupWizard'

function navigateTo(destination: string): void {
  window.location.assign(destination)
}

export const Route = createFileRoute('/setup')({
  loader: () => loadRouteDictionary('auth'),
  component: () => <SetupWizard onNavigate={navigateTo} />,
})
