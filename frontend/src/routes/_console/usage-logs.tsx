import { createFileRoute, Navigate } from '@tanstack/react-router'

import type { UsageLogSearch } from '@/features/logs/search'

/**
 * Legacy route (web-00 §4.3). The canonical IA places both log pages under
 * `/dashboard`; this path only exists so old bookmarks and external links keep
 * working. `search` is inherited and `replace` keeps the redirect out of the
 * history stack.
 */
export const Route = createFileRoute('/_console/usage-logs')({
  component: LegacyUsageLogsRedirect,
})

function LegacyUsageLogsRedirect() {
  return (
    <Navigate
      to="/dashboard/usage-logs"
      // The legacy route has no validator, so `previous` is the raw record the
      // browser carried; the target route normalises it on arrival.
      search={(previous) => previous as UsageLogSearch}
      replace
    />
  )
}
