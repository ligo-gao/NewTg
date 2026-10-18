import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { GroupListPage } from '@/features/admin/users/GroupListPage'

export const Route = createFileRoute('/_console/admin/users/groups')({
  loader: () => loadDictionary('en', 'admin'),
  component: GroupListPage,
})
