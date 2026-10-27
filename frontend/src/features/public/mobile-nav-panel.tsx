import { useEffect } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import type { PublicNavItem } from './nav-visibility'

export interface MobileNavPanelProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  items: readonly PublicNavItem[]
  activePath?: string
  labels: Record<PublicNavItem['key'], string>
  /** Guests need a sign-in hand-off here: the header's is hidden below 1024. */
  isLoggedIn: boolean
  /** `/sign-in?redirect=…` computed by the header for the current path. */
  signInTarget: string
}

/**
 * Returns focus to the hamburger that owns the panel. The panel deliberately
 * has no dialog semantics: it is a plain conditional block, not a modal, so
 * the only focus contract is "give focus back when the panel closes".
 */
function focusMenuTrigger(): void {
  document
    .querySelector<HTMLElement>('[aria-controls="mobile-menu"]')
    ?.focus()
}

/**
 * F-09-34 collapsed-header navigation (below 1024px). Marketing parity: a
 * plain block under the fixed header with stacked links and a full-width
 * primary CTA. Escape and choosing a link both close it and restore focus to
 * the hamburger.
 */
export function MobileNavPanel({
  open,
  onOpenChange,
  items,
  activePath,
  labels,
  isLoggedIn,
  signInTarget,
}: MobileNavPanelProps) {
  const { t } = useI18n()

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      onOpenChange(false)
      focusMenuTrigger()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [open, onOpenChange])

  if (!open) return null

  function close() {
    onOpenChange(false)
    focusMenuTrigger()
  }

  return (
    <nav
      id="mobile-menu"
      aria-label={t('public.nav.primary')}
      className="mobile-nav-panel"
    >
      <div className="wrap mobile-nav-inner">
        {items.map((item) => {
          const active = activePath === item.href
          return (
            <a
              key={item.key}
              href={item.href}
              aria-current={active ? 'page' : undefined}
              target={item.external ? '_blank' : undefined}
              rel={item.external ? 'noopener noreferrer' : undefined}
              aria-label={
                item.external
                  ? `${labels[item.key]} (${t('public.nav.external')})`
                  : undefined
              }
              onClick={close}
              className="mobile-nav-link"
            >
              {labels[item.key]}
            </a>
          )
        })}
        {!isLoggedIn ? (
          <a href={signInTarget} onClick={close} className="mobile-nav-link">
            {t('public.nav.sign_in')}
          </a>
        ) : null}
      </div>
    </nav>
  )
}
