import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'

import type { PublicNav } from '@/lib/public-status'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { navEntryOf, signInHref } from '@/features/public/nav-visibility'
import { useI18n } from '@/shared/lib/i18n-context'

export type DocsTarget =
  | { kind: 'external'; href: string }
  | { kind: 'internal'; href: string }
  | { kind: 'unavailable' }

/**
 * F-09-38 `/docs` ruling: the route is a redirect, never a built-out page.
 * Only absolute `http(s)` URLs and single-slash site-relative paths are
 * accepted — `javascript:`/`data:` and protocol-relative `//host` land on the
 * unavailable state, matching the server-side URL validation.
 *
 * The page lives outside `routes/docs.tsx` on purpose: TanStack Router cannot
 * code-split an exported route component, and keeping it in the route file
 * would put `PublicLayout` (and therefore `home.css`) back into the eager
 * entry bundle, defeating the route-level CSS split.
 */
export function resolveDocsTarget(raw: string | undefined): DocsTarget {
  const value = (raw ?? '').trim()
  if (/^https?:\/\//i.test(value)) return { kind: 'external', href: value }
  if (value.startsWith('/') && !value.startsWith('//')) {
    return { kind: 'internal', href: value }
  }
  return { kind: 'unavailable' }
}

/** True when `nav.docs.enabled=false` hides the entry from every surface. */
export function docsDisabled(nav: PublicNav | undefined): boolean {
  return !navEntryOf(nav, 'docs').enabled
}

/** True when `nav.docs.require_auth=true` keeps a guest out of `/docs`. */
export function docsNeedsSignIn(
  nav: PublicNav | undefined,
  isLoggedIn: boolean,
): boolean {
  const entry = navEntryOf(nav, 'docs')
  return entry.enabled && entry.require_auth && !isLoggedIn
}

export function DocsPage() {
  const { t } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  // `nav.docs.enabled=false` is a site-wide "docs do not exist" verdict. It
  // must match `publicRouteGuardRedirect` (disabled public pages never
  // redirect, they fall back to `/`): redirecting to `site.docs_url` while the
  // entry is hidden would keep exposing a page the operator switched off.
  const disabled = status.data !== undefined && docsDisabled(status.data.nav)
  const target: DocsTarget = disabled
    ? { kind: 'unavailable' }
    : resolveDocsTarget(status.data?.site?.docs_url)
  const navigableHref = target.kind === 'unavailable' ? null : target.href
  const needsSignIn =
    status.data !== undefined && docsNeedsSignIn(status.data.nav, me.isSuccess)
  // A status outage is not the same answer as "docs not configured": the
  // former is retryable, the latter is a permanent disabled state.
  const statusUnknown = status.isError && status.data === undefined

  useEffect(() => {
    if (needsSignIn) {
      window.location.replace(signInHref('/docs'))
      return
    }
    // `unavailable` renders the disabled state instead of navigating.
    if (navigableHref !== null) window.location.replace(navigableHref)
  }, [needsSignIn, navigableHref])

  return statusUnknown ? (
    <div className="flex flex-col items-start gap-3">
      <h1 className="text-public-title text-ink-1">
        {t('public.status.unavailable')}
      </h1>
      <p className="text-public-body text-ink-2">{t('public.status.unavailable_hint')}</p>
      <button
        type="button"
        onClick={() => void status.refetch()}
        className="text-public-body text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.status.retry')}
      </button>
    </div>
  ) : target.kind === 'unavailable' && !needsSignIn ? (
    <div className="flex flex-col items-start gap-3">
      <h1 className="text-public-title text-ink-1">
        {t('public.docs.unavailable')}
      </h1>
      <p className="text-public-body text-ink-2">{t('public.docs.unavailable_hint')}</p>
      <a
        href="/"
        className="text-public-body text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.docs.back_home')}
      </a>
    </div>
  ) : (
    // Brief transient state while the replace() above takes effect; no
    // fake docs content is ever rendered.
    <p className="text-public-body text-ink-3">{t('public.docs.redirecting')}</p>
  )
}
