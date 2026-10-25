import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import { AuthFlowMap } from './AuthFlowMap'

export interface AuthHallShellProps {
  title: string
  children: ReactNode
}

/**
 * The sign-in "hall" (see demos/sign-in-redesign): the left pane is the brand
 * statement plus the routing map, the right pane is the form column. The map
 * is decorative, so the statement stays a paragraph and the form title is the
 * page's only heading.
 *
 * Only sign-in uses this shell; the other anonymous pages keep the shared
 * card so a half-migrated auth flow never mixes two visual languages.
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

export function AuthHallShell({ title, children }: AuthHallShellProps) {
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
        <div className="auth-hall-pane-inner">
          <div className="auth-hall-form">
            <h1 id="auth-hall-title" className="auth-hall-title">
              {title}
            </h1>
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
