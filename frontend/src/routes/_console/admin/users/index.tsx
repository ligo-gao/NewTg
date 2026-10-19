import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { UserListPage } from '@/features/admin/users/UserListPage'

export const Route = createFileRoute('/_console/admin/users/')({
  loader: () => Promise.all([loadRouteDictionary('admin'), loadRouteDictionary('auth')]),
  component: UserListPage,
})
