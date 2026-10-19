import type { ReactNode } from 'react'

import { usePublicStatus } from '@/lib/public-status'
import { Tag } from '@/shared/ui/tag'
import { useI18n } from '@/shared/lib/i18n-context'

export interface OssCredit {
  name: string
  license: string
  url: string
}

/**
 * Startup values for `site.oss_credits` from modules-09 F-09-07. The backend
 * does not expose the key yet; keeping the list as an explicit default means a
 * deployer can replace it in one place, and the section is never silently
 * empty (compliance visibility).
 */
export const DEFAULT_OSS_CREDITS: readonly OssCredit[] = [
  { name: 'Go', license: 'BSD-3-Clause', url: 'https://go.dev/' },
  { name: 'Gin', license: 'MIT', url: 'https://github.com/gin-gonic/gin' },
  { name: 'GORM', license: 'MIT', url: 'https://github.com/go-gorm/gorm' },
  { name: 'PostgreSQL', license: 'PostgreSQL License', url: 'https://www.postgresql.org/' },
  { name: 'Redis', license: 'RSALv2/SSPLv1', url: 'https://redis.io/' },
  { name: 'React', license: 'MIT', url: 'https://react.dev/' },
  { name: 'TypeScript', license: 'Apache-2.0', url: 'https://www.typescriptlang.org/' },
  { name: 'Tailwind CSS', license: 'MIT', url: 'https://tailwindcss.com/' },
  { name: 'Hugeicons', license: 'See project site', url: 'https://hugeicons.com/' },
  { name: 'Prometheus', license: 'Apache-2.0', url: 'https://prometheus.io/' },
]

const TECH_STACK = [
  { key: 'backend', items: 'Go · Gin · GORM' },
  { key: 'frontend', items: 'React 19 · TypeScript · Tailwind CSS' },
  { key: 'data', items: 'PostgreSQL · Redis' },
  { key: 'observability', items: 'Prometheus' },
] as const

const CAPABILITIES = ['gateway', 'protocols', 'billing', 'security'] as const

export interface AboutPageProps {
  /** Override while `site.oss_credits` is not part of `GET /api/status`. */
  credits?: readonly OssCredit[]
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
 * status endpoint does not provide `site.about_html`, `site.docs_url` or
 * `site.oss_credits`, so the rich-text block is omitted (there is no raw-HTML
 * path), the docs block appears only when a URL is supplied, and the
 * attribution table falls back to the documented startup list. Stats come from
 * `GET /api/status` and disappear when the probe fails — the page never shows
 * a cached or invented count.
 */
export function AboutPage({
  credits = DEFAULT_OSS_CREDITS,
  docsUrl,
}: AboutPageProps) {
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

      {languageCount !== undefined ? (
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

      <section aria-labelledby="about-oss" className="flex flex-col gap-4">
        <h2 id="about-oss" className="text-base font-semibold text-ink-1">
          {t('public.about.oss.title')}
        </h2>
        {credits.length === 0 ? (
          <p className="text-sm text-ink-2">{t('public.about.oss.empty')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table
              aria-label={t('public.about.oss.title')}
              className="w-full min-w-[520px] border-collapse text-sm"
            >
              <thead>
                <tr className="border-b border-border-hairline bg-surface-subtle text-left text-xs text-ink-2">
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('public.about.oss.component')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('public.about.oss.license')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('public.about.oss.link')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {credits.map((credit) => (
                  <tr
                    key={credit.name}
                    className="border-b border-border-hairline last:border-0"
                  >
                    <th scope="row" className="px-3 py-3 text-left font-medium text-ink-1">
                      {credit.name}
                    </th>
                    <td className="px-3 py-3">
                      <Tag variant="neutral">{credit.license}</Tag>
                    </td>
                    <td className="px-3 py-3">
                      <ExternalLink href={credit.url} label={credit.name}>
                        {credit.name}
                      </ExternalLink>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="text-xs text-ink-3">{t('public.about.oss.note')}</p>
      </section>
    </article>
  )
}
