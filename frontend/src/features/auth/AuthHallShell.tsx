import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import { AuthFlowMap } from './AuthFlowMap'

export interface AuthHallShellProps {
  title: string
  subtitle?: ReactNode
  /** The setup wizard needs a wider reading column than the entry forms. */
  variant?: 'auth' | 'setup'
  children: ReactNode
}

/**
 * The auth "hall" (see demos/sign-in-redesign): the left pane is the brand
 * statement plus the routing map, the right pane is the form column. The map
 * is decorative, so the statement stays a paragraph and the form title is the
 * page's only heading.
 *
 * Every anonymous account page uses this shell so the whole entry flow speaks
 * one visual language.
 */
function BrandMark() {
  return (
    <svg
      className="auth-hall-logo-mark"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d="M3 6h3.5c2 0 3.5 1.2 4.5 3l2 6c1 1.8 2.5 3 4.5 3H21" />
      <path d="M3 18h3.5c2 0 3.5-1.2 4.5-3l2-6c1-1.8 2.5-3 4.5-3H21" />
    </svg>
  )
}

export function AuthHallShell({
  title,
  subtitle,
  variant = 'auth',
  children,
}: AuthHallShellProps) {
  const { t } = useI18n()

  return (
    <main id="main" className="auth-hall">
      <section className="auth-hall-brand">
        <div className="auth-hall-top">
          <a className="auth-hall-logo" href="/">
            <BrandMark />
            <span className="auth-hall-logo-name">{t('auth.shell.brand')}</span>
          </a>
        </div>

        <div className="auth-hall-statement">
          <p className="auth-hall-headline">
            <span>{t('auth.sign_in.statement_first')}</span>
            <span>{t('auth.sign_in.statement_second')}</span>
          </p>
          <p className="auth-hall-lead">{t('auth.sign_in.lead')}</p>
        </div>

        <AuthFlowMap />
      </section>

      <section className="auth-hall-pane" aria-labelledby="auth-hall-title">
        <div className="auth-hall-pane-inner" data-variant={variant}>
          <div className="auth-hall-form">
            <h1 id="auth-hall-title" className="auth-hall-title">
              {title}
            </h1>
            {subtitle !== undefined ? (
              <p className="auth-hall-subtitle">{subtitle}</p>
            ) : null}
            {children}
          </div>

          <footer className="auth-hall-legal">
            <a href="/legal/terms">{t('auth.shell.terms')}</a>
            <a href="/legal/privacy">{t('auth.shell.privacy')}</a>
          </footer>
        </div>
      </section>
    </main>
  )
}
