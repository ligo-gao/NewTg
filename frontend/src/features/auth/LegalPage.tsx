import { useI18n } from '@/shared/lib/i18n-context'

import { LEGAL_LAST_UPDATED } from './legal-meta'

export interface LegalPageProps {
  doc: 'terms' | 'privacy'
}

interface Section {
  id: string
  headingKey: string
  bodyKey: string
}

const SECTIONS: Record<LegalPageProps['doc'], Section[]> = {
  terms: [
    {
      id: 'acceptance',
      headingKey: 'auth.legal.terms.acceptance_heading',
      bodyKey: 'auth.legal.terms.acceptance_body',
    },
    {
      id: 'accounts',
      headingKey: 'auth.legal.terms.accounts_heading',
      bodyKey: 'auth.legal.terms.accounts_body',
    },
    {
      id: 'use',
      headingKey: 'auth.legal.terms.use_heading',
      bodyKey: 'auth.legal.terms.use_body',
    },
  ],
  privacy: [
    {
      id: 'collection',
      headingKey: 'auth.legal.privacy.collection_heading',
      bodyKey: 'auth.legal.privacy.collection_body',
    },
    {
      id: 'use',
      headingKey: 'auth.legal.privacy.use_heading',
      bodyKey: 'auth.legal.privacy.use_body',
    },
    {
      id: 'retention',
      headingKey: 'auth.legal.privacy.retention_heading',
      bodyKey: 'auth.legal.privacy.retention_body',
    },
  ],
}

/**
 * F-01-21 legal document page. Content is rendered from the typed i18n key
 * table rather than Markdown/HTML: there is no raw-HTML path, so the
 * `SanitizeMarkdown` requirement (security-04 §2.5) is satisfied structurally.
 */
export function LegalPage({ doc }: LegalPageProps) {
  const { t } = useI18n()
  const sections = SECTIONS[doc]
  const title =
    doc === 'terms' ? t('auth.legal.terms_title') : t('auth.legal.privacy_title')

  return (
    <div className="min-h-screen bg-surface-card text-ink-1">
      <header className="border-b border-border-hairline px-6 py-4">
        <div className="mx-auto flex max-w-[960px] items-center justify-between">
          <span className="text-body font-semibold">{t('auth.shell.brand')}</span>
          <a className="text-meta text-ink-2 hover:text-ink-1" href="/">
            {t('auth.legal.back')}
          </a>
        </div>
      </header>

      <main id="main" className="mx-auto flex max-w-[960px] gap-10 px-6 py-10">
        <article className="max-w-[720px] flex-1">
          <h1 className="text-page-title leading-9">{title}</h1>
          <p className="mt-1 font-mono text-meta text-ink-3">
            {t('auth.legal.last_updated', { date: LEGAL_LAST_UPDATED })}
          </p>
          <div className="mt-8 flex flex-col gap-6">
            {sections.map((section) => (
              <section key={section.id} aria-labelledby={`legal-${section.id}`}>
                <h2
                  id={`legal-${section.id}`}
                  tabIndex={-1}
                  className="text-section leading-7"
                >
                  {t(section.headingKey)}
                </h2>
                <p className="mt-2 text-body leading-6 text-ink-2">{t(section.bodyKey)}</p>
              </section>
            ))}
          </div>
        </article>

        <nav aria-label={t('auth.legal.toc')} className="hidden w-[200px] shrink-0 xl:block">
          <p className="text-badge uppercase tracking-wide text-ink-3">
            {t('auth.legal.toc')}
          </p>
          <ul className="mt-3 flex flex-col gap-2 text-meta">
            {sections.map((section) => (
              <li key={section.id}>
                <a className="text-ink-2 hover:text-brand" href={`#legal-${section.id}`}>
                  {t(section.headingKey)}
                </a>
              </li>
            ))}
          </ul>
        </nav>
      </main>
    </div>
  )
}
