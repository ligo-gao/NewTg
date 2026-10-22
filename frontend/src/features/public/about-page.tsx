import type { ReactNode } from 'react'

import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'

const TECH_STACK = [
  { key: 'backend', items: 'Go · Gin · GORM' },
  { key: 'frontend', items: 'React 19 · TypeScript · Tailwind CSS' },
  { key: 'data', items: 'PostgreSQL · Redis' },
  { key: 'observability', items: 'Prometheus' },
] as const

const CAPABILITIES = ['gateway', 'protocols', 'billing', 'security'] as const

export interface AboutPageProps {
  /** `site.docs_url`; when absent the whole docs block stays hidden. */
  docsUrl?: string
}

function trimTrailingSlash(url: string): string {
  return url.endsWith('/') ? url.slice(0, -1) : url
}

function ExternalLink({
  href,
  label,
  children,
}: {
  href: string
  label: string
  children: ReactNode
}) {
  const { t } = useI18n()
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={`${label} — ${t('public.about.external')}`}
      className="inline-flex items-center gap-1 text-brand hover:underline"
    >
      {children}
      <svg
        aria-hidden="true"
        viewBox="0 0 16 16"
        className="size-3.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      >
        <path d="M6 3h7v7M13 3 5 11" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    </a>
  )
}

/**
 * F-09-07 about page.
 *
 * Data policy per modules-09 §3.7: only real values render. The current public
 * status endpoint does not provide `site.about_html` or `site.docs_url`, so the
 * rich-text block is omitted (there is no raw-HTML path) and the docs block
 * appears only when a URL is supplied. Stats come from `GET /api/status` and
 * disappear when the probe fails — the page never shows a cached or invented
 * count.
 */
export function AboutPage({ docsUrl }: AboutPageProps) {
  const { t } = useI18n()
  const status = usePublicStatus()
  const languageCount = status.data?.languages.length

  return (
    <article className="mx-auto flex max-w-[880px] flex-col gap-12">
      <header className="flex flex-col gap-2">
        <h1 className="text-[20px] font-semibold leading-7 text-ink-1">
          {t('public.about.title', { site: t('public.about.site_name') })}
        </h1>
        <p className="text-sm text-ink-2">{t('public.about.tagline')}</p>
      </header>

      {/* 六态表的加载态：统计条先占位，技术栈等静态正文随首屏直出。 */}
      {status.isPending ? (
        <div
          aria-busy="true"
          data-testid="about-stats-skeleton"
          className="h-[76px] rounded-xl border border-border-hairline bg-surface-card"
        />
      ) : languageCount !== undefined ? (
        <dl className="flex flex-wrap gap-10 rounded-xl border border-border-hairline bg-surface-card px-5 py-4">
          <div className="flex flex-col gap-1">
            <dt className="text-xs leading-[18px] text-ink-2">
              {t('public.about.stats.languages')}
            </dt>
            <dd className="text-2xl font-semibold leading-8 tabular-nums text-ink-1">
              {languageCount}
            </dd>
          </div>
        </dl>
      ) : null}

      <section aria-labelledby="about-capabilities" className="flex flex-col gap-4">
        <h2 id="about-capabilities" className="text-base font-semibold text-ink-1">
          {t('public.about.capabilities')}
        </h2>
        <ul className="grid grid-cols-1 gap-4 md:grid-cols-2">
          {CAPABILITIES.map((key) => (
            <li
              key={key}
              className="rounded-xl border border-border-hairline bg-surface-card p-4"
            >
              <h3 className="text-sm font-semibold text-ink-1">
                {t(`public.about.feature.${key}.title`)}
              </h3>
              <p className="mt-1 text-sm text-ink-2">
                {t(`public.about.feature.${key}.body`)}
              </p>
            </li>
          ))}
        </ul>
      </section>

      <section aria-labelledby="about-techstack" className="flex flex-col gap-4">
        <h2 id="about-techstack" className="text-base font-semibold text-ink-1">
          {t('public.about.techstack')}
        </h2>
        <dl className="flex flex-col gap-2">
          {TECH_STACK.map((group) => (
            <div key={group.key} className="flex flex-col gap-1 sm:flex-row sm:gap-4">
              <dt className="w-24 shrink-0 text-[13px] leading-5 text-ink-2">
                {t(`public.about.tech.${group.key}`)}
              </dt>
              <dd className="text-sm leading-[22px] text-ink-1">{group.items}</dd>
            </div>
          ))}
        </dl>
      </section>

      {docsUrl !== undefined && docsUrl !== '' ? (
        <section aria-labelledby="about-docs" className="flex flex-col gap-4">
          <h2 id="about-docs" className="text-base font-semibold text-ink-1">
            {t('public.about.docs')}
          </h2>
          <div className="flex flex-wrap gap-6 text-sm">
            <ExternalLink href={docsUrl} label={t('public.about.docs_home')}>
              {t('public.about.docs_home')}
            </ExternalLink>
            <ExternalLink
              href={`${trimTrailingSlash(docsUrl)}/guide`}
              label={t('public.about.integration_guide')}
            >
              {t('public.about.integration_guide')}
            </ExternalLink>
            <ExternalLink
              href={`${trimTrailingSlash(docsUrl)}/api`}
              label={t('public.about.api_reference')}
            >
              {t('public.about.api_reference')}
            </ExternalLink>
          </div>
        </section>
      ) : null}
    </article>
  )
}
