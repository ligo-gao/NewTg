import { NotificationBell } from '@/features/notifications/notification-bell'
import type { Me } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { NavIcon } from './nav-icon'

export interface AppTopbarProps {
  title: string
  me?: Me | null
  onOpenCommandPalette: () => void
}

function isApplePlatform(): boolean {
  if (typeof navigator === 'undefined') return false
  const data = navigator as Navigator & {
    userAgentData?: { platform?: string }
  }
  const platform = data.userAgentData?.platform ?? navigator.platform
  return /mac|iphone|ipad|ipod/i.test(platform)
}

export function AppTopbar({ title, me, onOpenCommandPalette }: AppTopbarProps) {
  const { t } = useI18n()
  const shortcut = isApplePlatform()
    ? t('common.command.shortcut_mac')
    : t('common.command.shortcut_other')
  const initials = me?.username?.trim().slice(0, 1).toUpperCase() || 'A'

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-border-hairline bg-surface-card px-5">
      <div className="min-w-0">
        <p className="text-[10.5px] leading-[14px] text-ink-3">
          {t('common.console.label')}
        </p>
        <p className="truncate text-[13px] font-semibold leading-[18px] text-ink-1">
          {title}
        </p>
      </div>

      <div className="ml-auto flex items-center gap-1.5">
        <button
          type="button"
          data-testid="command-palette-trigger"
          aria-keyshortcuts="Meta+K Control+K"
          onClick={onOpenCommandPalette}
          className="inline-flex h-8 items-center gap-2 rounded-lg border border-border-default bg-surface-subtle px-2.5 text-[12px] text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <NavIcon name="search" className="size-4" />
          <span className="hidden sm:inline">{t('common.command.open')}</span>
          <kbd className="rounded border border-border-hairline bg-surface-card px-1 font-mono text-[10px] text-ink-3">
            {shortcut}
          </kbd>
        </button>

        <NotificationBell signedIn={me !== null && me !== undefined} />

        <a
          href="/profile"
          aria-label={t('common.nav.profile')}
          className="grid size-[30px] shrink-0 place-items-center rounded-full bg-brand-subtle text-[12px] font-bold text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {initials}
        </a>
      </div>
    </header>
  )
}
