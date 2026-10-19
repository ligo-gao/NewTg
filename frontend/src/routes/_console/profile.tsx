import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { ProfilePage } from '@/features/account/ProfilePage'

export const Route = createFileRoute('/_console/profile')({
  loader: () => loadRouteDictionary('account'),
  component: ProfilePage,
})
