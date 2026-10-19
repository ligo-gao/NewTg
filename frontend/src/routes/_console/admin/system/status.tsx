import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SystemStatusPage } from '@/features/admin/system/system-status-page'

export const Route = createFileRoute('/_console/admin/system/status')({
  loader: () => loadRouteDictionary('settings'),
  component: SystemStatusPage,
})
