import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { AuditLogsPage } from '@/features/audit/audit-logs-page'
import {
  validateAuditLogSearch,
  type AuditLogSearch,
} from '@/features/audit/search'

/**
 * Admin entry point for the operation log (F-08-22). It reuses the existing
 * modules-05 page component — the admin route only changes where the page is
 * mounted, never its internals.
 */
export const Route = createFileRoute('/_console/admin/system/audit-logs')({
  validateSearch: validateAuditLogSearch,
  loader: () => loadDictionary('en', 'logs'),
  component: AuditLogsRoute,
})

function AuditLogsRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <AuditLogsPage
      search={search as AuditLogSearch}
      onSearchChange={(next) => {
        void navigate({ search: next, replace: true })
      }}
    />
  )
}
