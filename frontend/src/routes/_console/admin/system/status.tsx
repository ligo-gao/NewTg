import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { SystemStatusPage } from '@/features/admin/system/system-status-page'

export const Route = createFileRoute('/_console/admin/system/status')({
  loader: () => loadDictionary('en', 'settings'),
  component: SystemStatusPage,
})
