import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { useTheme, type ThemeMode } from '@/app/providers/theme'
import { NotificationBell } from '@/features/notifications/notification-bell'
import type { Me } from '@/lib/session'
import { useLanguageSelection } from '@/lib/use-language-selection'
import { useI18n } from '@/shared/lib/i18n-context'
import { LanguageSwitch } from '@/shared/ui/language-switch'
import { useToastStore } from '@/shared/ui/toast'

import { NavIcon } from './nav-icon'
import { performSignOut } from './sign-out'

export interface AppTopbarProps {
  title: string
  me?: Me | null
  onOpenCommandPalette: () => void
  /**
   * A restricted (temporary-password) session cannot reach the notification
   * endpoints, and each blocked request would bounce the user back to the
   * change-password screen. Hide the bell while the session is restricted.
   */
  showNotifications?: boolean
}

function isApplePlatform(): boolean {
  if (typeof navigator === 'undefined') return false
  const data = navigator as Navigator & {
    userAgentData?: { platform?: string }
  }
  const platform = data.userAgentData?.platform ?? navigator.platform
  return /mac|iphone|ipad|ipod/i.test(platform)
}

function isThemeMode(value: string): value is ThemeMode {
  return value === 'light' || value === 'dark' || value === 'system'
}

/**
 * Console workspace header. Marketing navigation lives on the public site, so
 * the bar only carries workspace actions: title, search, back-to-site, theme,
 * language, notifications, and the profile avatar.
 */
export function AppTopbar({
  title,
  me,
  onOpenCommandPalette,
  showNotifications = true,
}: AppTopbarProps) {
  const { t } = useI18n()
  const { mode, setMode } = useTheme()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const signedIn = me !== null && me !== undefined
  const language = useLanguageSelection(signedIn)
  const shortcut = isApplePlatform()
    ? t('common.command.shortcut_mac')
    : t('common.command.shortcut_other')
  const initials = me?.username?.trim().slice(0, 1).toUpperCase() || 'A'
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    if (!menuOpen) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      setMenuOpen(false)
      triggerRef.current?.focus()
    }
    const onPointerDown = (event: PointerEvent) => {
      if (menuRef.current?.contains(event.target as Node)) return
      setMenuOpen(false)
    }
    document.addEventListener('keydown', onKeyDown)
    document.addEventListener('pointerdown', onPointerDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.removeEventListener('pointerdown', onPointerDown)
    }
  }, [menuOpen])

  async function signOut(): Promise<void> {
    setMenuOpen(false)
    if (await performSignOut(queryClient)) return
    push({ tone: 'error', message: t('common.nav.sign_out_failed') })
  }

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-border-hairline bg-surface-card px-5">
      <div className="min-w-0">
        <p className="text-badge leading-[14px] text-ink-3">
          {t('common.console.label')}
        </p>
        <p className="truncate text-meta font-semibold leading-[18px] text-ink-1">
          {title}
        </p>
      </div>

      <div className="ml-auto flex items-center gap-1.5">
        <a
          href="/"
          className="inline-flex h-8 shrink-0 items-center rounded-lg px-2.5 text-badge text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('common.error_page.home')}
        </a>

        <button
          type="button"
          data-testid="command-palette-trigger"
          aria-keyshortcuts="Meta+K Control+K"
          onClick={onOpenCommandPalette}
          className="inline-flex h-8 items-center gap-2 rounded-lg border border-border-default bg-surface-subtle px-2.5 text-badge text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <NavIcon name="search" className="size-4" />
          <span className="hidden sm:inline">{t('common.command.open')}</span>
          <kbd className="rounded border border-border-hairline bg-surface-card px-1 font-mono text-badge text-ink-3">
            {shortcut}
          </kbd>
        </button>

        <select
          aria-label={t('common.theme.label')}
          value={mode}
          onChange={(event) => {
            const next = event.target.value
            if (isThemeMode(next)) setMode(next)
          }}
          className="h-8 max-w-[110px] shrink-0 cursor-pointer rounded-lg border border-border-default bg-surface-card px-2 text-badge text-ink-2 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <option value="system">{t('common.theme.system')}</option>
          <option value="light">{t('common.theme.light')}</option>
          <option value="dark">{t('common.theme.dark')}</option>
        </select>

        <LanguageSwitch
          label={t('common.language.label')}
          onChange={language.onChange}
        />

        {showNotifications ? (
          <NotificationBell signedIn={signedIn} />
        ) : null}

        <div ref={menuRef} className="relative">
          <button
            ref={triggerRef}
            type="button"
            aria-label={t('common.nav.account_menu')}
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen((open) => !open)}
            className="grid size-[30px] shrink-0 place-items-center rounded-full bg-brand-subtle text-badge font-bold text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {initials}
          </button>
          {menuOpen ? (
            <div
              role="menu"
              aria-label={t('common.nav.account_menu')}
              className="absolute right-0 top-full z-20 mt-2 w-44 overflow-hidden rounded-lg border border-border-hairline bg-surface-card py-1 shadow-lg"
            >
              <a
                role="menuitem"
                href="/profile"
                className="block px-3 py-2 text-badge text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:bg-surface-hover focus-visible:outline-none"
              >
                {t('common.nav.profile')}
              </a>
              <div className="my-1 border-t border-border-hairline" />
              <button
                type="button"
                role="menuitem"
                onClick={() => void signOut()}
                className="block w-full px-3 py-2 text-left text-badge text-danger hover:bg-danger-bg focus-visible:bg-danger-bg focus-visible:outline-none"
              >
                {t('common.nav.sign_out')}
              </button>
            </div>
          ) : null}
        </div>
      </div>
    </header>
  )
}
