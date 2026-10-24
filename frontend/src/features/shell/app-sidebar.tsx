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
        'flex h-full shrink-0 flex-col border-r border-border-hairline bg-surface-card transition-[width] duration-150',
        collapsed ? 'w-16 px-2' : 'w-60 px-2.5',
      )}
      aria-label={t('common.app.title')}
    >
      {settingsMode ? (
        <div className="mt-1 flex min-h-0 flex-1 flex-col gap-1 pb-2">
          <a
            href="/dashboard"
            title={t('settings.shell.back')}
            className={cn(
              'flex min-h-[34px] items-center gap-2 rounded-lg px-2 text-meta leading-5 text-ink-2 transition-colors hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              collapsed && 'justify-center px-0',
            )}
          >
            <span aria-hidden="true" className="shrink-0 text-body leading-none">
              ←
            </span>
            {collapsed ? null : (
              <span className="min-w-0 flex-1 truncate">
                {t('settings.shell.back')}
              </span>
            )}
          </a>
          <div
            id="console-settings-nav-slot"
            data-testid="settings-nav-slot"
            data-collapsed={collapsed ? 'true' : 'false'}
            className="group flex-1 min-h-0 overflow-y-auto overflow-x-hidden"
          />
        </div>
      ) : (
        <nav
          className="mt-1 flex-1 overflow-y-auto pb-2 pt-1"
          aria-label={t('common.app.title')}
        >
          {groups.map((group) => (
            <div key={group.id} className="mt-3 first:mt-0">
              {collapsed ? null : (
                <div className="px-2 pb-1 text-badge font-semibold uppercase leading-[18px] tracking-[0.02em] text-ink-2">
                  {t(group.labelKey)}
                </div>
              )}
              <div className="flex flex-col gap-0.5">
                {group.items.map((item) => {
                  const current = active?.id === item.id
                  return (
                    <a
                      key={item.id}
                      href={item.to}
                      title={t(item.labelKey)}
                      aria-current={current ? 'page' : undefined}
                      className={cn(
                        'flex min-h-[34px] items-center gap-2 rounded-lg px-2 text-meta leading-5 transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                        current
                          ? 'bg-brand-subtle font-semibold text-brand'
                          : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                        collapsed && 'justify-center px-0',
                      )}
                    >
                      <NavIcon
                        name={item.icon}
                        className="size-[17px] shrink-0"
                      />
                      {collapsed ? null : (
                        <span className="min-w-0 flex-1 truncate">
                          {t(item.labelKey)}
                        </span>
                      )}
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
          'mt-2 flex items-center gap-2 border-t border-border-hairline pt-3',
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
