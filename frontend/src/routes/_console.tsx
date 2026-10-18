import { createFileRoute, Outlet } from '@tanstack/react-router'

import { useI18n } from '@/shared/lib/i18n-context'

// Keys carry the `common.` module prefix because the dictionary flattener
// already restores the module namespace (`{module}.{feature}.{element}`).
const NAV_ITEMS = [
  { href: '/dashboard', key: 'common.nav.dashboard' },
  { href: '/profile', key: 'common.nav.profile' },
  { href: '/security', key: 'common.nav.security' },
  { href: '/admin/users', key: 'common.nav.users' },
  { href: '/admin/users/groups', key: 'common.nav.groups' },
] as const

/** Extracted so the layout header can be rendered in isolation by tests. */
export function ConsoleHeader() {
  const { t } = useI18n()

  return (
    <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
      <div className="flex items-center gap-6">
        <p className="text-sm font-semibold">{t('common.app.title')}</p>
        <nav
          aria-label={t('common.app.title')}
          className="flex flex-wrap gap-4 text-[13px]"
        >
          {NAV_ITEMS.map((item) => (
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

export const Route = createFileRoute('/_console')({
  component: ConsoleLayout,
})
