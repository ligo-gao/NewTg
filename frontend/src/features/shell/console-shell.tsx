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

import { AppSidebar } from './app-sidebar'
import { AppTopbar } from './app-topbar'
import {
  CommandPalette,
  useCommandPaletteShortcut,
} from './command-palette'
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

/**
 * Console shell: a slim global top bar, a navigation rail that folds into a
 * horizontal strip below 1024px, and the routed page capped at the same grid
 * as the public pages.
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
  const [collapsed, setCollapsed] = useState(readCollapsed)
  const [commandPaletteOpen, setCommandPaletteOpen] = useState(false)
  const navigate = useNavigate()

  useCommandPaletteShortcut(() => setCommandPaletteOpen(true))

  const active = activeNavItem(pathname)
  const title = active ? t(active.labelKey) : t('common.nav.dashboard')
  // A restricted (temporary-password) session may not call the notification or
  // announcement endpoints. Mounting those widgets would answer 403 and bounce
  // the user straight back here, so the only page they can complete becomes
  // unreachable. The change-password form needs neither.
  const restricted = isForcedPasswordChangePath(pathname, searchStr)

  return (
    <div className="flex h-screen flex-col bg-surface-page text-ink-1">
      <AppTopbar
        title={title}
        me={meQuery.data}
        showNotifications={!restricted}
        onOpenCommandPalette={() => setCommandPaletteOpen(true)}
      />
      <div className="flex min-h-0 flex-1 flex-col min-[1024px]:flex-row">
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
        <main
          id="main"
          className="min-h-0 min-w-0 flex-1 overflow-y-auto p-4 min-[1024px]:p-6 min-[1280px]:px-10 min-[1280px]:py-7"
        >
          <div className="mx-auto w-full max-w-[1520px]">
            <Outlet />
          </div>
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
