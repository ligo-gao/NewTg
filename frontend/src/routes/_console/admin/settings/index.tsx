import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SettingsShell } from '@/features/admin/settings/settings-shell'
import { isSettingsTab, type SettingsTab } from '@/features/admin/settings/sections'

export interface SettingsSearch {
  tab?: SettingsTab
}

export const Route = createFileRoute('/_console/admin/settings/')({
  validateSearch: (search: Record<string, unknown>): SettingsSearch =>
    isSettingsTab(search.tab) ? { tab: search.tab } : {},
  // `/admin/settings` is a canonical alias for the first region; redirecting
  // (rather than silently rendering Site) keeps the URL shareable and makes
  // the active tab unambiguous.
  beforeLoad: ({ search }) => {
    if (search.tab === undefined) {
      throw redirect({ to: '/admin/settings', search: { tab: 'site' }, replace: true })
    }
  },
  loader: () => loadRouteDictionary('settings'),
  component: SettingsRoute,
})

function SettingsRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <SettingsShell
      tab={search.tab ?? 'site'}
      onTabChange={(tab) => {
        void navigate({ search: { tab }, replace: true })
      }}
    />
  )
}
