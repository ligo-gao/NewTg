import { useDocsVisible } from '@/features/public/nav-visibility'
import { usePlaygroundEnabled } from '@/features/playground/visibility'
import type { Me } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { NavIcon } from './nav-icon'
import { activeNavItem, visibleNavGroups } from './nav-config'

function cn(...values: Array<string | false | null | undefined>): string {
  return values.filter(Boolean).join(' ')
}

export interface AppSidebarProps {
  me: Me | null | undefined
  pathname: string
  collapsed: boolean
  onToggleCollapsed: () => void
  /** Test/embedding override for the site-level Playground switch. */
  playgroundEnabled?: boolean
  /** Test/embedding override for the site-level Docs target. */
  docsVisible?: boolean
}

export function AppSidebar({
  me,
  pathname,
  collapsed,
  onToggleCollapsed,
  playgroundEnabled,
  docsVisible,
}: AppSidebarProps) {
  const { t } = useI18n()
  const sitePlaygroundEnabled = usePlaygroundEnabled()
  const siteDocsVisible = useDocsVisible()
  const groups = visibleNavGroups(me, {
    playgroundEnabled: playgroundEnabled ?? sitePlaygroundEnabled,
    docsVisible: docsVisible ?? siteDocsVisible,
  })
  const active = activeNavItem(pathname)
  // Only the settings tab route owns the drill-in (it is the sole route that
  // mounts `SettingsShell` into the slot). Standalone pages such as
  // `/admin/settings/announcements` keep the root navigation — otherwise the
  // slot would stay empty and the user would be stranded on "Back to console".
  const settingsMode = pathname.replace(/\/+$/, '') === '/admin/settings'

  return (
    <aside
      className={cn(
        'flex shrink-0 border-border-hairline bg-surface-card transition-[width]',
        // < 1024px: a horizontal, always-visible strip
        'w-full flex-row items-center gap-1 overflow-x-auto border-b px-2 py-1.5',
        // ≥ 1024px: the existing rail
        'min-[1024px]:h-full min-[1024px]:flex-col min-[1024px]:items-stretch min-[1024px]:overflow-visible',
        'min-[1024px]:border-b-0 min-[1024px]:border-r min-[1024px]:px-2.5 min-[1024px]:py-0',
        collapsed ? 'min-[1024px]:w-16 min-[1024px]:px-2' : 'min-[1024px]:w-60',
      )}
      aria-label={t('common.app.title')}
    >
      {settingsMode ? (
        <div
          className={cn(
            // < 1024px: one row inside the strip — back link, then the nav.
            'flex min-h-0 flex-1 flex-row items-center gap-1 overflow-x-auto overflow-y-hidden',
            // ≥ 1024px: the original vertical drill-in column.
            'min-[1024px]:mt-1 min-[1024px]:flex-col min-[1024px]:items-stretch',
            'min-[1024px]:overflow-visible min-[1024px]:pb-2',
          )}
        >
          <a
            href="/dashboard"
            title={t('settings.shell.back')}
            className={cn(
              'flex min-h-[34px] shrink-0 items-center gap-2 rounded-lg px-2 text-meta leading-5 text-ink-2 transition-colors hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              collapsed &&
                'min-[1024px]:justify-center min-[1024px]:px-0',
            )}
          >
            <span aria-hidden="true" className="shrink-0 text-body leading-none">
              ←
            </span>
            <span
              className={cn(
                'whitespace-nowrap min-[1024px]:min-w-0 min-[1024px]:flex-1 min-[1024px]:truncate',
                collapsed && 'min-[1024px]:hidden',
              )}
            >
              {t('settings.shell.back')}
            </span>
          </a>
          <div
            id="console-settings-nav-slot"
            data-testid="settings-nav-slot"
            data-collapsed={collapsed ? 'true' : 'false'}
            className="group min-h-0 min-w-0 flex-1 overflow-x-auto overflow-y-hidden min-[1024px]:overflow-x-hidden min-[1024px]:overflow-y-auto"
          />
        </div>
      ) : (
        <nav
          className="flex flex-1 flex-row items-center gap-1 overflow-x-auto overflow-y-hidden min-[1024px]:mt-1 min-[1024px]:flex-col min-[1024px]:items-stretch min-[1024px]:gap-0 min-[1024px]:overflow-y-auto min-[1024px]:pb-2 min-[1024px]:pt-1"
          aria-label={t('common.app.title')}
        >
          {groups.map((group) => (
            <div
              key={group.id}
              className="min-[1024px]:mt-3 min-[1024px]:first:mt-0"
            >
              {collapsed ? null : (
                <div className="hidden px-2 pb-1 text-badge font-semibold uppercase leading-[18px] tracking-[0.02em] text-ink-2 min-[1024px]:block">
                  {t(group.labelKey)}
                </div>
              )}
              <div className="flex flex-row gap-0.5 min-[1024px]:flex-col">
                {group.items.map((item) => {
                  const current = active?.id === item.id
                  return (
                    <a
                      key={item.id}
                      href={item.to}
                      title={t(item.labelKey)}
                      aria-current={current ? 'page' : undefined}
                      className={cn(
                        'flex min-h-[34px] items-center gap-2 whitespace-nowrap rounded-lg px-2 text-meta leading-5 transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                        current
                          ? 'bg-brand-subtle font-semibold text-brand'
                          : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                        collapsed &&
                          'min-[1024px]:justify-center min-[1024px]:px-0',
                      )}
                    >
                      <span className="hidden shrink-0 min-[1024px]:inline-flex">
                        <NavIcon
                          name={item.icon}
                          className="size-[17px] shrink-0"
                        />
                      </span>
                      <span
                        className={cn(
                          'whitespace-nowrap min-[1024px]:min-w-0 min-[1024px]:flex-1 min-[1024px]:truncate',
                          collapsed && 'min-[1024px]:hidden',
                        )}
                      >
                        {t(item.labelKey)}
                      </span>
                      {collapsed || item.badge === undefined ? null : (
                        <span className="inline-flex h-[18px] min-w-[18px] items-center justify-center rounded-full bg-surface-subtle px-1 text-badge text-ink-3">
                          {item.badge}
                        </span>
                      )}
                    </a>
                  )
                })}
              </div>
            </div>
          ))}
        </nav>
      )}

      <div
        className={cn(
          'mt-2 hidden items-center gap-2 border-t border-border-hairline pt-3 min-[1024px]:flex',
          collapsed ? 'justify-center' : 'px-1.5',
        )}
      >
        {collapsed ? null : (
          <span className="min-w-0 flex-1 truncate text-badge text-ink-3">
            {t('common.console.label')}
          </span>
        )}
        <button
          type="button"
          data-testid="sidebar-collapse"
          onClick={onToggleCollapsed}
          aria-label={
            collapsed
              ? t('common.console.expand')
              : t('common.console.collapse')
          }
          className="grid size-[30px] shrink-0 place-items-center rounded-lg border border-border-hairline bg-surface-card text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <NavIcon name="panel" className="size-4" />
        </button>
      </div>
    </aside>
  )
}
