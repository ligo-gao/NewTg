import { useQuery } from '@tanstack/react-query'

import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { htmlToPlainText, sanitizeHtmlFragment } from './home/custom-html'
import { LanguageSwitch } from './language-switch'
import { isNavEntryVisible, isSafePublicHref } from './nav-visibility'
import { SiteBrand } from './public-header'

interface FooterLink {
  key: string
  label: string
  href: string
  external: boolean
}

function isExternal(href: string): boolean {
  return /^https?:\/\//i.test(href)
}

/**
 * F-09-37 footer. Rendered only inside `PublicLayout`; console routes use
 * `_console` and therefore never mount it (TC-F-09-37-04) — there is no
 * feature-flag branch to get wrong.
 *
 * Columns: brand, operator links (`site.footer_html`, sanitized), resources,
 * language. When the operator HTML is empty (or nothing survives sanitizing)
 * the links column falls back to the default group instead of leaving a blank
 * column.
 */
export function PublicFooter() {
  const { t } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const isLoggedIn = me.isSuccess
  const site = status.data?.site
  const title = site?.title?.trim() || 'NewTG'
  const year = new Date().getFullYear()
  const docsUrl = (site?.docs_url ?? '').trim()
  const statusPageUrl = (site?.status_page_url ?? '').trim()
  // Same `enabled && (!require_auth || isLoggedIn) && docs_url` rule as the
  // header: a hidden nav entry must not reappear in the footer.
  const docsVisible =
    isNavEntryVisible(status.data?.nav, 'docs', isLoggedIn, docsUrl) &&
    isSafePublicHref(docsUrl)
  // Status-page URLs never render as links unless they pass the same scheme
  // rule (defence in depth; the server validates on write).
  const safeStatusPageUrl = isSafePublicHref(statusPageUrl) ? statusPageUrl : ''

  const operatorLinks = sanitizeHtmlFragment(site?.footer_html)

  const defaultLinks: FooterLink[] = [
    ...(docsVisible
      ? [{ key: 'docs', label: t('public.footer.docs'), href: docsUrl, external: isExternal(docsUrl) }]
      : []),
    ...(safeStatusPageUrl !== ''
      ? [
          {
            key: 'status',
            label: t('public.footer.status'),
            href: safeStatusPageUrl,
            external: isExternal(statusPageUrl),
          },
        ]
      : []),
    { key: 'about', label: t('public.nav.about'), href: '/about', external: false },
    {
      key: 'terms',
      label: t('public.footer.terms'),
      href: '/legal/terms',
      external: false,
    },
    {
      key: 'privacy',
      label: t('public.footer.privacy'),
      href: '/legal/privacy',
      external: false,
    },
  ]

  const resourceLinks: FooterLink[] = [
    ...(docsVisible
      ? [{ key: 'docs', label: t('public.footer.docs'), href: docsUrl, external: isExternal(docsUrl) }]
      : []),
    ...(safeStatusPageUrl !== ''
      ? [
          {
            key: 'status',
            label: t('public.footer.status'),
            href: safeStatusPageUrl,
            external: isExternal(statusPageUrl),
          },
        ]
      : []),
    {
      key: 'opensource',
      label: t('public.footer.opensource'),
      href: '/about#opensource',
      external: false,
    },
    { key: 'terms', label: t('public.footer.terms'), href: '/legal/terms', external: false },
    {
      key: 'privacy',
      label: t('public.footer.privacy'),
      href: '/legal/privacy',
      external: false,
    },
  ]

  const aboutSummary = htmlToPlainText(site?.about_html) || t('public.footer.tagline')

  // F-09-37 异常流程: a status outage with no cached config degrades to the
  // minimal copyright line instead of guessing links or columns.
  if (status.isError && site === undefined) {
    return (
      <footer role="contentinfo" className="border-t border-border-hairline bg-surface-page">
        <div className="mx-auto w-full max-w-[1200px] px-4 py-6 text-xs text-ink-3 md:px-6">
          <p>{t('public.footer.copyright', { year, site: title })}</p>
        </div>
      </footer>
    )
  }

  return (
    <footer role="contentinfo" className="border-t border-border-hairline bg-surface-page">
      <div className="mx-auto w-full max-w-[1200px] px-4 py-10 md:px-6">
        <div className="grid grid-cols-1 gap-8 md:grid-cols-2 lg:grid-cols-4">
          <div>
            <SiteBrand site={site} />
            <p className="mt-3 max-w-[280px] text-[13px] leading-5 text-ink-2">
              {aboutSummary}
            </p>
          </div>

          <div>
            <h2 className="text-[13px] font-medium leading-[18px] text-ink-2">
              {t('public.footer.links')}
            </h2>
            {operatorLinks !== '' ? (
              // Server-sanitized operator HTML; the client allowlist above ran
              // again and already forced external rel/target.
              <div
                className="mt-3 space-y-3 text-[13px] leading-5 text-ink-2 [&_a]:text-ink-2 [&_a:hover]:text-ink-1 [&_a:hover]:underline [&_p]:m-0"
                dangerouslySetInnerHTML={{ __html: operatorLinks }}
              />
            ) : (
              <FooterLinkList links={defaultLinks} />
            )}
          </div>

          <div>
            <h2 className="text-[13px] font-medium leading-[18px] text-ink-2">
              {t('public.footer.resources')}
            </h2>
            <FooterLinkList links={resourceLinks} />
          </div>

          <div>
            <h2 className="text-[13px] font-medium leading-[18px] text-ink-2">
              {t('public.language.label')}
            </h2>
            <div className="mt-3">
              <LanguageSwitch variant="footer" />
            </div>
          </div>
        </div>

        <div className="mt-8 flex flex-col gap-2 border-t border-border-hairline pt-6 text-xs text-ink-3 md:flex-row md:items-center md:justify-between">
          <p>
            {t('public.footer.copyright', { year, site: title })}
            {'. '}
            {t('public.footer.all_rights')}
          </p>
          {site?.filing_no?.trim() ? (
            <a
              href="https://beian.miit.gov.cn/"
              target="_blank"
              rel="noopener noreferrer"
              className="text-ink-3 hover:text-ink-1 hover:underline"
            >
              {site.filing_no.trim()}
            </a>
          ) : null}
        </div>
      </div>
    </footer>
  )
}

function FooterLinkList({ links }: { links: readonly FooterLink[] }) {
  const { t } = useI18n()
  if (links.length === 0) return null
  return (
    <ul className="mt-3 space-y-3 text-[13px] leading-5">
      {links.map((link) => (
        <li key={link.key}>
          <a
            href={link.href}
            target={link.external ? '_blank' : undefined}
            rel={link.external ? 'noopener noreferrer' : undefined}
            aria-label={
              link.external ? `${link.label} (${t('public.nav.external')})` : undefined
            }
            className="inline-flex items-center gap-1 text-ink-2 hover:text-ink-1 hover:underline"
          >
            {link.label}
          </a>
        </li>
      ))}
    </ul>
  )
}
