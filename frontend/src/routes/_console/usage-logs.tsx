import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { UsageLogsPage } from '@/features/logs/usage-logs-page'
import {
  validateUsageLogSearch,
  type UsageLogSearch,
} from '@/features/logs/search'

export const Route = createFileRoute('/_console/usage-logs')({
  validateSearch: validateUsageLogSearch,
  loader: () => loadRouteDictionary('logs'),
  component: UsageLogsRoute,
})

function UsageLogsRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <UsageLogsPage
      search={search as UsageLogSearch}
      onSearchChange={(next) => {
        // Replace keeps filter typing out of the browser's back stack while
        // still making the current view refresh/share safe.
        void navigate({ search: next, replace: true })
      }}
    />
  )
}
