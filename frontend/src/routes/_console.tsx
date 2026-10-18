import { createFileRoute, Outlet } from '@tanstack/react-router'

import { useI18n } from '@/shared/lib/i18n-context'

const NAV_ITEMS = [
  { href: '/dashboard', key: 'nav.dashboard' },
  { href: '/profile', key: 'nav.profile' },
  { href: '/security', key: 'nav.security' },
  { href: '/admin/users', key: 'nav.users' },
  { href: '/admin/users/groups', key: 'nav.groups' },
] as const

function ConsoleLayout() {
  const { t } = useI18n()

  return (
    <div className="min-h-screen bg-surface-page text-ink-1">
      <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
        <div className="flex items-center gap-6">
          {/* `common` is the always-loaded domain, unlike `auth.shell.brand`. */}
          <p className="text-sm font-semibold">{t('app.title')}</p>
          <nav aria-label={t('app.title')} className="flex flex-wrap gap-4 text-[13px]">
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
      <main id="main" className="p-6">
        <Outlet />
      </main>
    </div>
  )
}

export const Route = createFileRoute('/_console')({
  component: ConsoleLayout,
})
