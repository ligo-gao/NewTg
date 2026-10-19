import { createFileRoute } from '@tanstack/react-router'
import { useEffect } from 'react'

import { loadRouteDictionary } from '@/app/providers/i18n'
import type { PublicNav } from '@/lib/public-status'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useQuery } from '@tanstack/react-query'
import { navEntryOf, signInHref } from '@/features/public/nav-visibility'
import { PublicLayout } from '@/features/public/public-layout'
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
 */
export function resolveDocsTarget(raw: string | undefined): DocsTarget {
  const value = (raw ?? '').trim()
  if (/^https?:\/\//i.test(value)) return { kind: 'external', href: value }
  if (value.startsWith('/') && !value.startsWith('//')) {
    return { kind: 'internal', href: value }
  }
  return { kind: 'unavailable' }
}

/** True when `nav.docs.require_auth=true` keeps a guest out of `/docs`. */
export function docsNeedsSignIn(
  nav: PublicNav | undefined,
  isLoggedIn: boolean,
): boolean {
  const entry = navEntryOf(nav, 'docs')
  return entry.enabled && entry.require_auth && !isLoggedIn
}

export const Route = createFileRoute('/docs')({
  loader: () => loadRouteDictionary('public'),
  component: DocsRoute,
})

export function DocsRoute() {
  const { t } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const target = resolveDocsTarget(status.data?.site?.docs_url)
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

  return (
    <PublicLayout activePath="/docs">
      {statusUnknown ? (
        <div className="flex flex-col items-start gap-3">
          <h1 className="text-xl font-semibold text-ink-1">
            {t('public.status.unavailable')}
          </h1>
          <p className="text-sm text-ink-2">{t('public.status.unavailable_hint')}</p>
          <button
            type="button"
            onClick={() => void status.refetch()}
            className="text-sm text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.status.retry')}
          </button>
        </div>
      ) : target.kind === 'unavailable' && !needsSignIn ? (
        <div className="flex flex-col items-start gap-3">
          <h1 className="text-xl font-semibold text-ink-1">
            {t('public.docs.unavailable')}
          </h1>
          <p className="text-sm text-ink-2">{t('public.docs.unavailable_hint')}</p>
          <a
            href="/"
            className="text-sm text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.docs.back_home')}
          </a>
        </div>
      ) : (
        // Brief transient state while the replace() above takes effect; no
        // fake docs content is ever rendered.
        <p className="text-sm text-ink-3">{t('public.docs.redirecting')}</p>
      )}
    </PublicLayout>
  )
}
