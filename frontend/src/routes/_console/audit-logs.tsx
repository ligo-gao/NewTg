import { createFileRoute, Navigate } from '@tanstack/react-router'

import type { AuditLogSearch } from '@/features/audit/search'

/**
 * Legacy route (web-00 §4.3). Canonical path is `/dashboard/audit-logs`;
 * `search` is inherited so filters survive the redirect.
 */
export const Route = createFileRoute('/_console/audit-logs')({
  component: LegacyAuditLogsRedirect,
})

function LegacyAuditLogsRedirect() {
  return (
    <Navigate
      to="/dashboard/audit-logs"
      search={(previous) => previous as AuditLogSearch}
      replace
    />
  )
}
