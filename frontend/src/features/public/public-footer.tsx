import {
  FRIEND_LINKS,
  ShellFooter,
  formatShellCopy,
  type ShellFooterColumn,
  type ShellFooterLink,
} from '@newtg/public-shell'
import { useQuery } from '@tanstack/react-query'

import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useLanguageSelection } from '@/lib/use-language-selection'
import { useI18n, type Locale } from '@/shared/lib/i18n-context'

import { htmlToPlainText, sanitizeHtmlFragment } from './home/custom-html'
import {
  NAV_COPY_KEY,
  PUBLIC_NAV_HREFS,
  isNavEntryVisible,
  isSafePublicHref,
} from './nav-visibility'
import { siteBrand } from './public-header'
import { shellCopy, shellLocaleOptions } from './shell-copy'

function isExternal(href: string): boolean {
  return /^https?:\/\//i.test(href)
}

/** Product column order, mirroring the approved footer demo's PRODUCT_ORDER. */
const PRODUCT_KEYS = ['models', 'pricing', 'rankings', 'about', 'console'] as const

/**
 * The OpenAI-compatible base URL shown in the footer, copied verbatim from
 * `demos/unified-footer/index.html`. A private address (`localhost`,
 * `127.0.0.1`, `[::1]`) is not an endpoint a visitor can call, so it renders
 * nothing rather than a copyable dead end.
 */
export function apiEndpoint(serverAddress: string | undefined): string {
  const raw = (serverAddress ?? '').trim().replace(/\/+$/, '')
  if (!/^https?:\/\//i.test(raw)) return ''
  if (/^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(:\d+)?$/i.test(raw)) return ''
  return `${raw}/v1`
}

/**
 * F-09-37 footer. Rendered only inside `PublicLayout`; console routes use
 * `_console` and therefore never mount it (TC-F-09-37-04).
 *
 * The shell package owns the markup, styling and copy; this component supplies
 * the operator's switches: the product column (`site.footer_html`, sanitized,
 * or the default links), the resources column and the language control.
 */
export function PublicFooter() {
  const { locale, setLocale, availableLocales } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const isLoggedIn = me.isSuccess
  const { onChange } = useLanguageSelection(isLoggedIn)

  const copy = shellCopy(locale)
  const site = status.data?.site
  const title = site?.title?.trim() || 'NEWTG'
  const brand = siteBrand(site)
  const year = new Date().getFullYear()
  const docsUrl = (site?.docs_url ?? '').trim()
  const statusPageUrl = (site?.status_page_url ?? '').trim()
  const nav = status.data?.nav
  // Same `enabled && (!require_auth || isLoggedIn) && docs_url` rule as the
  // header: a hidden nav entry must not reappear in the footer.
  const docsVisible =
    isNavEntryVisible(nav, 'docs', isLoggedIn, docsUrl) && isSafePublicHref(docsUrl)
  // Status-page URLs never render as links unless they pass the same scheme
  // rule (defence in depth; the server validates on write).
  const safeStatusPageUrl = isSafePublicHref(statusPageUrl) ? statusPageUrl : ''

  const operatorLinks = sanitizeHtmlFragment(site?.footer_html)

  const productLinks: ShellFooterLink[] = PRODUCT_KEYS.filter((key) =>
    isNavEntryVisible(nav, key, isLoggedIn),
  ).map((key) => ({
    label: copy[NAV_COPY_KEY[key]],
    href: PUBLIC_NAV_HREFS[key],
  }))

  const resourceLinks: ShellFooterLink[] = [
    ...(docsVisible
      ? [
          {
            label: copy['nav.docs'],
            href: docsUrl,
            external: isExternal(docsUrl),
          },
        ]
      : []),
    ...(safeStatusPageUrl !== ''
      ? [
          {
            label: copy['footer.status'],
            href: safeStatusPageUrl,
            external: isExternal(safeStatusPageUrl),
          },
        ]
      : []),
    { label: copy['footer.terms'], href: '/legal/terms' },
    { label: copy['footer.privacy'], href: '/legal/privacy' },
  ]

  const columns: ShellFooterColumn[] = []
  if (operatorLinks !== '' || productLinks.length > 0) {
    columns.push({
      key: 'product',
      title: copy['footer.product'],
      // Operator HTML wins over the default links; it never renders both.
      html: operatorLinks !== '' ? operatorLinks : undefined,
      links: productLinks,
    })
  }
  columns.push({ key: 'resources', title: copy['footer.resources'], links: resourceLinks })
  columns.push({ key: 'friends', title: copy['footer.friends'], links: FRIEND_LINKS })

  const copyright = `${formatShellCopy(copy['footer.copyright'], { year, site: title })} ${copy['footer.all_rights']}`
  const filingNo = site?.filing_no?.trim()

  // F-09-37 异常流程: a status outage with no cached config degrades to the
  // minimal copyright line instead of guessing links or columns.
  const degraded = status.isError && site === undefined
  // A private/loopback address (or no address at all) yields '', so the strip
  // is simply omitted.
  const endpoint = apiEndpoint(site?.server_address)

  function changeLocale(next: string) {
    const value = next as Locale
    setLocale?.(value)
    onChange?.(value)
  }

  return (
    <ShellFooter
      copy={copy}
      brand={brand}
      statement={
        degraded ? undefined : htmlToPlainText(site?.about_html) || copy['footer.tagline']
      }
      endpoint={degraded || endpoint === '' ? undefined : endpoint}
      columns={degraded ? [] : columns}
      locale={
        degraded
          ? undefined
          : {
              value: locale,
              options: shellLocaleOptions(availableLocales),
              onChange: changeLocale,
            }
      }
      copyright={copyright}
      filing={filingNo ? { label: filingNo, href: 'https://beian.miit.gov.cn/' } : null}
    />
  )
}
