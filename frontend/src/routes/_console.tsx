import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Outlet, redirect } from '@tanstack/react-router'

import { fetchMe, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

// Keys carry the `common.` module prefix because the dictionary flattener
// already restores the module namespace (`{module}.{feature}.{element}`).
// `admin` items are hidden from non-admin sessions (§9.3 role guard); the
// server still enforces RBAC on every request — this is UX only (§9.1).
const NAV_ITEMS = [
  { href: '/dashboard', key: 'common.nav.dashboard' },
  { href: '/profile', key: 'common.nav.profile' },
  { href: '/security', key: 'common.nav.security' },
  // M2 channel management; `/admin/channels` sits under the admin layout guard.
  { href: '/admin/channels', key: 'common.nav.channels', admin: true },
  { href: '/admin/users', key: 'common.nav.users', admin: true },
  { href: '/admin/users/groups', key: 'common.nav.groups', admin: true },
] as const

/** Extracted so the layout header can be rendered in isolation by tests. */
export function ConsoleHeader() {
  const { t } = useI18n()
  // The console guard has already seeded `['me']`; this read is served from
  // the cache (a direct `/dashboard` navigation still triggers one probe).
  const meQuery = useQuery(meQueryOptions())

  return (
    <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
      <div className="flex items-center gap-6">
        <p className="text-sm font-semibold">{t('common.app.title')}</p>
        <nav
          aria-label={t('common.app.title')}
          className="flex flex-wrap gap-4 text-[13px]"
        >
          {NAV_ITEMS.filter(
            (item) => !('admin' in item) || meQuery.data?.role === 'admin',
          ).map((item) => (
            <a
              key={item.href}
              href={item.href}
              className="text-ink-2 hover:text-ink-1"
            >
              {t(item.key)}
            </a>
          ))}
        </nav>
      </div>
    </header>
  )
}

export function ConsoleLayout() {
  return (
    <div className="min-h-screen bg-surface-page text-ink-1">
      <ConsoleHeader />
      <main id="main" className="p-6">
        <Outlet />
      </main>
    </div>
  )
}

/**
 * Session layer of the guard chain (web-00 §9.3 row 1). A missing session
 * redirects to `/sign-in` with the origin path in `?redirect=`; no intermediate
 * 401 page is rendered (§9.4). The path is sanitized again by
 * `sanitizeAuthRedirect` when the sign-in flow consumes it.
 */
export const Route = createFileRoute('/_console')({
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (me === null) {
      throw redirect({ to: '/sign-in', search: { redirect: location.href } })
    }
    return { me }
  },
  component: ConsoleLayout,
})
