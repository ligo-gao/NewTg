import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { ProfilePage } from '@/features/account/ProfilePage'

export const Route = createFileRoute('/_console/profile')({
  loader: () => loadDictionary('en', 'account'),
  component: ProfilePage,
})
