import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { GroupListPage } from '@/features/admin/users/GroupListPage'

export const Route = createFileRoute('/_console/admin/users/groups')({
  loader: () => loadRouteDictionary('admin'),
  component: GroupListPage,
})
