import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { UserListPage } from '@/features/admin/users/UserListPage'

export const Route = createFileRoute('/_console/admin/users/')({
  loader: () => Promise.all([loadDictionary('en', 'admin'), loadDictionary('en', 'auth')]),
  component: UserListPage,
})
