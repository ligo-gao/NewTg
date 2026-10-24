import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

export interface AuthPageShellProps {
  title: string
  subtitle?: ReactNode
  children: ReactNode
  /** The setup wizard is wider than the centered auth card (modules-01 §0.10). */
  variant?: 'auth' | 'setup'
}

/**
 * Shared chrome for every anonymous account page (modules-01 §1 wireframe):
 * thin brand header, one centered card, legal footer. The card is deliberately
 * the only "brand breathing" surface in the console.
 */
export function AuthPageShell({
  title,
  subtitle,
  children,
  variant = 'auth',
}: AuthPageShellProps) {
  const { t } = useI18n()
  const maxWidth = variant === 'setup' ? 'max-w-[560px]' : 'max-w-[420px]'

  return (
    <div className="flex min-h-screen flex-col bg-surface-subtle text-ink-1">
      <header className="flex h-14 items-center border-b border-border-hairline bg-surface-card px-6">
        <span className="text-body font-semibold">{t('auth.shell.brand')}</span>
      </header>

      <main id="main" className="flex flex-1 items-center justify-center px-4 py-10">
        <div
          className={`w-full ${maxWidth} rounded-xl border border-border-hairline bg-surface-card p-6`}
        >
          <h1 className="text-auth-title leading-[30px]">{title}</h1>
          {subtitle ? <div className="mt-1 text-meta text-ink-2">{subtitle}</div> : null}
          <div className="mt-6">{children}</div>
        </div>
      </main>

      <footer className="flex justify-center gap-4 pb-6 text-meta text-ink-3">
        <a className="hover:text-ink-1" href="/legal/terms">
          {t('auth.shell.terms')}
        </a>
        <a className="hover:text-ink-1" href="/legal/privacy">
          {t('auth.shell.privacy')}
        </a>
      </footer>
    </div>
  )
}
