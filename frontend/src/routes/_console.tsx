import { createFileRoute, Link, Outlet } from '@tanstack/react-router'

import { useI18n } from '@/shared/lib/i18n-context'

function ConsoleLayout() {
  const { t } = useI18n()

  return (
    <div className="min-h-screen bg-surface-page text-ink-1">
      <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
        <p className="text-sm font-semibold">NewTG console</p>
        <nav aria-label="Console" className="mt-2 flex gap-4 text-sm">
          <Link to="/dashboard" className="text-ink-2 hover:text-ink-1">
            {t('common.nav.dashboard')}
          </Link>
          <Link to="/admin/channels" className="text-ink-2 hover:text-ink-1">
            {t('common.nav.channels')}
          </Link>
        </nav>
      </header>
      <main id="main" className="p-6">
        <p className="text-sm text-ink-2">M0 scaffold</p>
        <Outlet />
      </main>
    </div>
  )
}

export const Route = createFileRoute('/_console')({
  component: ConsoleLayout,
})
