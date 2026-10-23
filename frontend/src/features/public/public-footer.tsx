import { useQuery } from '@tanstack/react-query'

import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { htmlToPlainText, sanitizeHtmlFragment } from './home/custom-html'
import { LanguageSwitch } from '@/shared/ui/language-switch'
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
  const title = site?.title?.trim() || 'NEWTG'
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

  const nav = status.data?.nav
  const productLinks: FooterLink[] = [
    ...(isNavEntryVisible(nav, 'pricing', isLoggedIn)
      ? [{ key: 'pricing', label: t('public.nav.pricing'), href: '/pricing', external: false }]
      : []),
    ...(isNavEntryVisible(nav, 'rankings', isLoggedIn)
      ? [{ key: 'rankings', label: t('public.nav.rankings'), href: '/rankings', external: false }]
      : []),
    ...(isNavEntryVisible(nav, 'about', isLoggedIn)
      ? [{ key: 'about', label: t('public.nav.about'), href: '/about', external: false }]
      : []),
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

  const aboutSummary = htmlToPlainText(site?.about_html) || t('public.footer.tagline')

  // F-09-37 异常流程: a status outage with no cached config degrades to the
  // minimal copyright line instead of guessing links or columns.
  if (status.isError && site === undefined) {
    return (
      <footer role="contentinfo" className="site-footer">
        <div className="wrap footer-bottom">
          <p>{t('public.footer.copyright', { year, site: title })}</p>
        </div>
      </footer>
    )
  }

  return (
    <footer role="contentinfo" className="site-footer">
      <div className="wrap">
        <div className="footer-grid">
          <div>
            <SiteBrand site={site} />
            <p className="footer-tag">{aboutSummary}</p>
          </div>

          {operatorLinks !== '' || productLinks.length > 0 ? (
            <div className="footer-col">
              <h2>{t('public.footer.product')}</h2>
              {operatorLinks !== '' ? (
                // Server-sanitized operator HTML; the client allowlist above ran
                // again and already forced external rel/target.
                <div
                  className="footer-operator"
                  dangerouslySetInnerHTML={{ __html: operatorLinks }}
                />
              ) : (
                <FooterLinkList links={productLinks} />
              )}
            </div>
          ) : null}

          <div className="footer-col">
            <h2>{t('public.footer.resources')}</h2>
            <FooterLinkList links={resourceLinks} />
          </div>

          <div className="footer-col">
            <h2>{t('public.language.label')}</h2>
            <LanguageSwitch
              variant="footer"
              className="home-lang"
              label={t('public.language.label')}
            />
          </div>
        </div>

        <div className="footer-bottom">
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
    <ul>
      {links.map((link) => (
        <li key={link.key}>
          <a
            href={link.href}
            target={link.external ? '_blank' : undefined}
            rel={link.external ? 'noopener noreferrer' : undefined}
            aria-label={
              link.external ? `${link.label} (${t('public.nav.external')})` : undefined
            }
          >
            {link.label}
          </a>
        </li>
      ))}
    </ul>
  )
}
