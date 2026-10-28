import { useQueryClient } from '@tanstack/react-query'

import {
  NAV_COPY_KEY,
  PUBLIC_NAV_HREFS,
  PUBLIC_NAV_ORDER,
  isExternalHref,
  isNavEntryVisible,
  isSafePublicHref,
  navEntryOf,
  sectionForPath,
  visibleNavItems,
} from '@newtg/public-shell'
import type { PublicNavKey, PublicNavItem, PublicNavSwitches } from '@newtg/public-shell'

import {
  PUBLIC_STATUS_QUERY_KEY,
  type PublicStatus,
} from '@/lib/public-status'

/**
 * SPA side of the public navigation.
 *
 * The rules themselves live in `@newtg/public-shell` (`nav.ts`) because the
 * marketing site renders the same header and must resolve the same list. This
 * module only adds what is SPA-specific: the cached status reader and the
 * sign-in hand-off the router uses.
 */
export {
  NAV_COPY_KEY,
  PUBLIC_NAV_HREFS,
  PUBLIC_NAV_ORDER,
  isExternalHref,
  isNavEntryVisible,
  isSafePublicHref,
  navEntryOf,
  sectionForPath,
  visibleNavItems,
}
export type { PublicNavKey, PublicNavItem }

/**
 * Console-side Docs visibility (spec §4): the Help → Docs entry exists only
 * when a docs target is configured. Reads the `['public','status']` snapshot
 * the root guard already wrote — no extra request — and fails hidden while the
 * cache is absent, because an unconfigured target is the default.
 */
export function useDocsVisible(): boolean {
  const status = useQueryClient().getQueryData<PublicStatus>(PUBLIC_STATUS_QUERY_KEY)
  return status === undefined
    ? false
    : isNavEntryVisible(status.nav, 'docs', true, status.site?.docs_url)
}

/**
 * Sign-in hand-off used by both the header CTA and the direct-link guard.
 *
 * The full `pathname + search` is percent-encoded into `?redirect=` so the
 * login page can return the visitor to exactly where they were aiming. The
 * consuming page still runs `sanitizeAuthRedirect` before navigating —
 * encoding here is transport, not validation.
 */
export function signInHref(pathname: string, search = ''): string {
  return `/sign-in?redirect=${encodeURIComponent(`${pathname}${search}`)}`
}

/** Console entry for the avatar menu (always `/dashboard`, F-09-35). */
export function currentSearch(): string {
  return typeof location === 'undefined' ? '' : location.search
}

/**
 * Direct-link guard helper (F-09-34/F-09-35).
 *
 * Returns the redirect target for a disabled or auth-gated public route, or
 * `null` when the current visitor may stay.
 *
 * - `enabled=false` for the section → silent redirect to `/` (F-09-35:
 *   disabled public pages are not a 401 and no sign-in prompt is rendered).
 * - `require_auth=true` and guest → `/sign-in?redirect=<pathname+search>`.
 * - Anything else → allow.
 */
export function publicRouteGuardRedirect(
  pathname: string,
  search: string,
  nav: PublicNavSwitches | undefined,
  signedIn: boolean,
): string | null {
  const key = sectionForPath(pathname)
  if (key === null) return null
  const entry = navEntryOf(nav, key)
  if (!entry.enabled) return '/'
  if (entry.require_auth && !signedIn) return signInHref(pathname, search)
  return null
}
