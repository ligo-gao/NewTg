import { useRef, type JSX, type KeyboardEvent } from 'react'

import { NavIcon, type NavIconKey } from '@/features/shell/nav-icon'
import { useI18n } from '@/shared/lib/i18n-context'

import {
  SETTINGS_TABS,
  tabConfig,
  type SettingsTab,
} from './sections'

export interface SettingsNavProps {
  tab: SettingsTab
  onTabChange: (tab: SettingsTab) => void
  dirtySections?: ReadonlySet<string>
}

interface SettingsNavItem {
  tab: SettingsTab
  icon: NavIconKey
}

interface SettingsNavGroup {
  id: string
  labelKey: string
  items: readonly SettingsNavItem[]
}

/**
 * Drill-in second-level navigation for the settings shell. The groups mirror
 * the main sidebar's visual language; labels and section membership still come
 * from `SETTINGS_TABS_CONFIG`, so a tab can never be reachable here without a
 * registered section. The shell owns the dirty guard: this component only
 * reports the requested tab.
 */
const SETTINGS_NAV_GROUPS: readonly SettingsNavGroup[] = [
  {
    id: 'site',
    labelKey: 'settings.nav_group.site',
    items: [{ tab: 'site', icon: 'settings' }],
  },
  {
    id: 'auth',
    labelKey: 'settings.nav_group.auth',
    items: [{ tab: 'auth', icon: 'shield' }],
  },
  {
    id: 'billing',
    labelKey: 'settings.nav_group.billing',
    items: [
      { tab: 'billing', icon: 'card' },
      { tab: 'payment', icon: 'wallet' },
    ],
  },
  {
    id: 'security',
    labelKey: 'settings.nav_group.security',
    items: [{ tab: 'security', icon: 'shield' }],
  },
  {
    id: 'notify',
    labelKey: 'settings.nav_group.notify',
    items: [{ tab: 'notify', icon: 'activity' }],
  },
  {
    id: 'policy',
    labelKey: 'settings.nav_group.policy',
    items: [{ tab: 'policy', icon: 'sliders' }],
  },
]

function cn(...values: Array<string | false | null | undefined>): string {
  return values.filter(Boolean).join(' ')
}

export function SettingsNav({
  tab,
  onTabChange,
  dirtySections,
}: SettingsNavProps): JSX.Element {
  const { t } = useI18n()
  const itemRefs = useRef<Partial<Record<SettingsTab, HTMLButtonElement | null>>>(
    {},
  )

  function handleKeyDown(event: KeyboardEvent<HTMLElement>): void {
    const { key } = event
    if (
      key !== 'ArrowDown' &&
      key !== 'ArrowUp' &&
      key !== 'Home' &&
      key !== 'End'
    ) {
      return
    }
    event.preventDefault()
    const index = SETTINGS_TABS.indexOf(tab)
    let nextIndex = index
    if (key === 'ArrowDown') nextIndex = (index + 1) % SETTINGS_TABS.length
    if (key === 'ArrowUp') {
      nextIndex = (index - 1 + SETTINGS_TABS.length) % SETTINGS_TABS.length
    }
    if (key === 'Home') nextIndex = 0
    if (key === 'End') nextIndex = SETTINGS_TABS.length - 1
    const next = SETTINGS_TABS[nextIndex]
    if (next === undefined) return
    itemRefs.current[next]?.focus()
    onTabChange(next)
  }

  return (
    <nav
      aria-label={t('settings.shell.tabs_label')}
      className="flex flex-col gap-0.5 pb-2"
      onKeyDown={handleKeyDown}
    >
      {SETTINGS_NAV_GROUPS.map((group, groupIndex) => (
        <div key={group.id} className={groupIndex === 0 ? undefined : 'mt-3'}>
          <div className="px-2 pb-1 text-[10.5px] font-semibold uppercase leading-4 tracking-[0.04em] text-ink-3">
            {t(group.labelKey)}
          </div>
          <div className="flex flex-col gap-0.5">
            {group.items.map((item) => {
              const config = tabConfig(item.tab)
              const current = item.tab === tab
              const dirty =
                dirtySections !== undefined &&
                config.sections.some((section) => dirtySections.has(section.id))
              return (
                <button
                  key={item.tab}
                  ref={(element) => {
                    itemRefs.current[item.tab] = element
                  }}
                  type="button"
                  aria-current={current ? 'page' : undefined}
                  tabIndex={current ? 0 : -1}
                  onClick={() => onTabChange(item.tab)}
                  className={cn(
                    'flex min-h-[34px] w-full items-center gap-2 rounded-lg px-2 text-[13px] leading-5 transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                    current
                      ? 'bg-brand-subtle font-semibold text-brand'
                      : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                  )}
                >
                  <NavIcon name={item.icon} className="size-[17px] shrink-0" />
                  <span className="min-w-0 flex-1 truncate text-left">
                    {t(config.labelKey)}
                  </span>
                  {dirty ? (
                    <span
                      aria-hidden="true"
                      data-testid="settings-nav-dirty"
                      className="inline-block size-1.5 shrink-0 rounded-full bg-warning"
                    />
                  ) : null}
                </button>
              )
            })}
          </div>
        </div>
      ))}
    </nav>
  )
}
