import { useQuery } from '@tanstack/react-query'
import {
  Outlet,
  useNavigate,
  useRouterState,
} from '@tanstack/react-router'
import { useState } from 'react'

import { isForcedPasswordChangePath } from '@/app/providers/auth-failures'
import { AnnouncementPopup } from '@/features/shell/announcement-popup'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { useMediaQuery } from '@/shared/lib/use-media-query'

import { AppSidebar } from './app-sidebar'
import { AppTopbar } from './app-topbar'
import {
  CommandPalette,
  useCommandPaletteShortcut,
} from './command-palette'
import { NavIcon } from './nav-icon'
import { activeNavItem } from './nav-config'

export const SIDEBAR_STORAGE_KEY = 'newtg.console.sidebar'

/** Stored preference wins; otherwise narrow viewports start collapsed. */
export function readCollapsed(): boolean {
  try {
    if (typeof window === 'undefined') return false
    const stored = window.localStorage.getItem(SIDEBAR_STORAGE_KEY)
    if (stored === 'collapsed') return true
    if (stored === 'expanded') return false
    return (
      typeof window.matchMedia === 'function' &&
      window.matchMedia('(max-width: 1279px)').matches
    )
  } catch {
    return false
  }
}

export function writeCollapsed(collapsed: boolean): void {
  try {
    window.localStorage.setItem(
      SIDEBAR_STORAGE_KEY,
      collapsed ? 'collapsed' : 'expanded',
    )
  } catch {
    // Storage can be unavailable in a private window; the toggle still works.
  }
}

export function ConsoleWideScreenGate() {
  const { t } = useI18n()
  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-page p-6">
      <div className="w-full max-w-md rounded-xl border border-border-hairline bg-surface-card p-7 text-center">
        <div className="mx-auto mb-4 grid size-11 place-items-center rounded-xl bg-brand-subtle text-brand">
          <NavIcon name="panel" className="size-5" />
        </div>
        <h1 className="text-lg font-semibold text-ink-1">
          {t('common.console.gate.title')}
        </h1>
        <p className="mt-2 text-sm leading-6 text-ink-2">
          {t('common.console.gate.body')}
        </p>
      </div>
    </div>
  )
}

/**
 * Console shell: a persistent left navigation, a slim global top bar, and the
 * routed page. The console is wide-screen only; below 1024px it renders the
 * gate instead of a degraded mobile layout.
 */
export function ConsoleShell() {
  const { t } = useI18n()
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  })
  const searchStr = useRouterState({
    select: (state) => state.location.searchStr,
  })
  const meQuery = useQuery(meQueryOptions())
  const wide = useMediaQuery('(min-width: 1024px)')
  const [collapsed, setCollapsed] = useState(readCollapsed)
  const [commandPaletteOpen, setCommandPaletteOpen] = useState(false)
  const navigate = useNavigate()

  useCommandPaletteShortcut(() => setCommandPaletteOpen(true))

  if (!wide) return <ConsoleWideScreenGate />

  const active = activeNavItem(pathname)
  const title = active ? t(active.labelKey) : t('common.nav.dashboard')
  // A restricted (temporary-password) session may not call the notification or
  // announcement endpoints. Mounting those widgets would answer 403 and bounce
  // the user straight back here, so the only page they can complete becomes
  // unreachable. The change-password form needs neither.
  const restricted = isForcedPasswordChangePath(pathname, searchStr)

  return (
    <div className="flex h-screen bg-surface-page text-ink-1">
      <AppSidebar
        me={meQuery.data}
        pathname={pathname}
        collapsed={collapsed}
        onToggleCollapsed={() => {
          setCollapsed((current) => {
            const next = !current
            writeCollapsed(next)
            return next
          })
        }}
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <AppTopbar
          title={title}
          me={meQuery.data}
          showNotifications={!restricted}
          onOpenCommandPalette={() => setCommandPaletteOpen(true)}
        />
        <main id="main" className="min-h-0 flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>

      {restricted ? null : <AnnouncementPopup />}
      <CommandPalette
        open={commandPaletteOpen}
        onOpenChange={setCommandPaletteOpen}
        onNavigate={(to) => {
          void navigate({ to })
        }}
      />
    </div>
  )
}
