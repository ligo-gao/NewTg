import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { AuditLogsPage } from '@/features/audit/audit-logs-page'
import {
  validateAuditLogSearch,
  type AuditLogSearch,
} from '@/features/audit/search'

export const Route = createFileRoute('/_console/audit-logs')({
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
