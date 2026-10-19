import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

export interface PublicLayoutProps {
  /** Current pathname; drives `aria-current` on the nav. */
  activePath?: string
  children: ReactNode
}

const NAV_ITEMS = [
  { href: '/', key: 'public.nav.home' },
  { href: '/about', key: 'public.nav.about' },
] as const

/**
 * Minimal public shell for the M6b pages.
 *
 * The full public header (F-09-34: site title, pricing/docs switches, language
 * picker, session-aware CTA, mobile hamburger) belongs to M7 together with the
 * `site.*` configuration surface; this shell only wires what exists today so
 * visitors can move between the public pages that ship. Links are plain `<a>`
 * like the console header, which keeps every public page usable without a
 * router context.
 */
export function PublicLayout({ activePath, children }: PublicLayoutProps) {
  const { t } = useI18n()

  return (
    <div className="flex min-h-screen flex-col bg-surface-page text-ink-1">
      <header className="border-b border-border-hairline bg-surface-card">
        <div className="mx-auto flex w-full max-w-[1120px] items-center gap-6 px-6 py-3">
          <a href="/" className="text-sm font-semibold text-ink-1">
            {t('public.nav.brand')}
          </a>
          <nav aria-label={t('public.nav.primary')} className="flex items-center gap-4 text-[13px]">
            {NAV_ITEMS.map((item) => {
              const active = activePath === item.href
              return (
                <a
                  key={item.href}
                  href={item.href}
                  aria-current={active ? 'page' : undefined}
                  className={active ? 'font-medium text-ink-1' : 'text-ink-2 hover:text-ink-1'}
                >
                  {t(item.key)}
                </a>
              )
            })}
          </nav>
          <a
            href="/sign-in"
            className="ml-auto text-[13px] text-ink-2 hover:text-ink-1"
          >
            {t('public.nav.sign_in')}
          </a>
        </div>
      </header>

      <main id="main" className="mx-auto w-full max-w-[1120px] flex-1 px-6 py-10">
        {children}
      </main>

      <footer className="border-t border-border-hairline px-6 py-4">
        <p className="mx-auto max-w-[1120px] text-xs text-ink-3">
          {t('public.footer.copyright', { year: new Date().getFullYear() })}
        </p>
      </footer>
    </div>
  )
}
