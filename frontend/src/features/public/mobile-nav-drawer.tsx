import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import { MobileSheet } from './mobile-sheet'
import type { PublicNavItem } from './nav-visibility'

export interface MobileNavDrawerProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  items: readonly PublicNavItem[]
  /** Guest / user cluster and the language picker, rendered below the links. */
  footer: ReactNode
  activePath?: string
  labels: Record<PublicNavItem['key'], string>
}

/**
 * F-09-34 mobile drawer (<768px). It reuses `MobileSheet`, which composes the
 * shared Base UI Dialog and therefore inherits the focus trap, Escape/backdrop
 * dismissal, body inertness and focus restore instead of re-implementing a
 * second modal (review rule: one modal implementation). The drawer is not a
 * separate `role="dialog"` — nesting one inside the sheet would break the
 * focus trap, which is why only the sheet owns modal semantics.
 *
 * Nav items are 48px tall on touch (UI design F-09-34) and the current route is
 * marked with `aria-current` exactly like the desktop nav.
 */
export function MobileNavDrawer({
  open,
  onOpenChange,
  items,
  footer,
  activePath,
  labels,
}: MobileNavDrawerProps) {
  const { t } = useI18n()

  return (
    <MobileSheet
      open={open}
      onOpenChange={onOpenChange}
      title={t('public.nav.menu_title')}
      closeLabel={t('public.nav.close')}
    >
      <div className="public-shell drawer-shell">
        <nav aria-label={t('public.nav.primary')}>
          <ul className="flex flex-col">
            {items.map((item) => {
              const active = activePath === item.href
              return (
                <li key={item.key}>
                  <a
                    href={item.href}
                    aria-current={active ? 'page' : undefined}
                    target={item.external ? '_blank' : undefined}
                    rel={item.external ? 'noopener noreferrer' : undefined}
                    aria-label={
                      item.external
                        ? `${labels[item.key]} (${t('public.nav.external')})`
                        : undefined
                    }
                    onClick={() => onOpenChange(false)}
                    className={[
                      'flex min-h-12 items-center rounded-lg px-3 text-[15px]',
                      active
                        ? 'font-medium text-ink-1'
                        : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                    ].join(' ')}
                  >
                    {labels[item.key]}
                  </a>
                </li>
              )
            })}
          </ul>
        </nav>
        <div className="mt-4 border-t border-border-hairline pt-4">{footer}</div>
      </div>
    </MobileSheet>
  )
}
