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
}

export function AppSidebar({
  me,
  pathname,
  collapsed,
  onToggleCollapsed,
  playgroundEnabled,
}: AppSidebarProps) {
  const { t } = useI18n()
  const sitePlaygroundEnabled = usePlaygroundEnabled()
  const groups = visibleNavGroups(me, {
    playgroundEnabled: playgroundEnabled ?? sitePlaygroundEnabled,
  })
  const active = activeNavItem(pathname)
  // On a settings route the sidebar drills into the settings sections (rendered
  // into the slot by `SettingsShell`) instead of showing the root navigation.
  const settingsMode =
    pathname === '/admin/settings' || pathname.startsWith('/admin/settings/')

  return (
    <aside
      className={cn(
        'sticky top-0 flex h-screen shrink-0 flex-col border-r border-border-hairline bg-surface-card transition-[width] duration-150',
        collapsed ? 'w-16 px-2' : 'w-60 px-2.5',
      )}
      aria-label={t('common.app.title')}
    >
      <div
        className={cn(
          'flex items-center gap-2.5 py-3',
          collapsed ? 'justify-center px-0' : 'px-1.5',
        )}
      >
        <span className="grid size-8 shrink-0 place-items-center rounded-[9px] bg-brand text-[10px] font-bold tracking-[-0.02em] text-white">
          NTG
        </span>
        {collapsed ? null : (
          <div className="min-w-0">
            <div className="truncate text-[15px] font-bold leading-5 tracking-[-0.01em] text-ink-1">
              {t('common.app.title')}
            </div>
            <div className="truncate text-[11px] leading-4 text-ink-3">
              AI API Gateway
            </div>
          </div>
        )}
      </div>

      {settingsMode ? (
        <div className="mt-1 flex min-h-0 flex-1 flex-col gap-1 pb-2">
          <a
            href="/dashboard"
            title={t('settings.shell.back')}
            className={cn(
              'flex min-h-[34px] items-center gap-2 rounded-lg px-2 text-[13px] leading-5 text-ink-2 transition-colors hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              collapsed && 'justify-center px-0',
            )}
          >
            <span aria-hidden="true" className="shrink-0 text-[15px] leading-none">
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
          className="mt-1 flex-1 overflow-y-auto pb-2"
          aria-label={t('common.app.title')}
        >
          {groups.map((group) => (
            <div key={group.id} className="mt-3 first:mt-1">
              {collapsed ? null : (
                <div className="px-2 pb-1 text-[10.5px] font-semibold uppercase leading-4 tracking-[0.04em] text-ink-3">
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
                        'flex min-h-[34px] items-center gap-2 rounded-lg px-2 text-[13px] leading-5 transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
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
                        <span className="inline-flex h-[18px] min-w-[18px] items-center justify-center rounded-full bg-surface-subtle px-1 text-[10px] text-ink-3">
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
          <span className="min-w-0 flex-1 truncate text-[10.5px] text-ink-3">
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
