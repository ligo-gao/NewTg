import { useQueryClient } from '@tanstack/react-query'

import {
  PUBLIC_STATUS_QUERY_KEY,
  type PublicNav,
  type PublicNavEntry,
  type PublicStatus,
} from '@/lib/public-status'

/**
 * F-09-34 navigation visibility (modules-09 C3).
 *
 * Single source of truth for the six public nav entries. The header, the
 * mobile drawer and the route guard helpers below all consume this module, so
 * "is this entry visible" cannot drift between them.
 */
export type PublicNavKey =
  | 'home'
  | 'console'
  | 'pricing'
  | 'rankings'
  | 'docs'
  | 'about'

export interface PublicNavItem {
  key: PublicNavKey
  href: string
  /** True only for `nav.docs` when the target is an absolute http(s) URL. */
  external: boolean
}

export const PUBLIC_NAV_ORDER: readonly PublicNavKey[] = [
  'home',
  'console',
  'pricing',
  'rankings',
  'docs',
  'about',
]

export const PUBLIC_NAV_HREFS: Record<PublicNavKey, string> = {
  home: '/',
  console: '/dashboard',
  pricing: '/pricing',
  rankings: '/rankings',
  docs: '/docs',
  about: '/about',
}

/**
 * Missing keys keep the documented defaults (F-09-34 异常流程: enabled=true,
 * require_auth=false). `console` is the exception — its `require_auth` is
 * locked to true server-side, so a missing value must not expose the console
 * entry to guests.
 */
export function navEntryOf(
  nav: PublicNav | undefined,
  key: PublicNavKey,
): Required<PublicNavEntry> {
  const entry = nav?.[key]
  return {
    enabled: entry?.enabled ?? true,
    require_auth: entry?.require_auth ?? key === 'console',
  }
}

/**
 * Absolute `http(s)` targets open in a new tab; a same-site relative path (for
 * example `/guide`) is navigated in place (F-09-38 分支流程, allowed by the
 * backend URL validation).
 */
export function isExternalHref(href: string): boolean {
  return /^https?:\/\//i.test(href)
}

/**
 * Client-side mirror of the server URL rule (F-09-38 数据字段表: reject
 * `javascript:`/`data:` and protocol-relative targets). The server validates
 * on write, but a stale or hand-edited row must never become an `<a href>` or
 * `window.location` target in a visitor's browser.
 */
export function isSafePublicHref(href: string | undefined): boolean {
  const value = (href ?? '').trim()
  if (isExternalHref(value)) return true
  return value.startsWith('/') && !value.startsWith('//') && !value.startsWith('/\\')
}

/**
 * `enabled && (!require_auth || isLoggedIn)` — the frozen derived rule from
 * F-09-34. `nav.docs` additionally requires a non-empty `site.docs_url`
 * (logical AND, F-09-38). A `require_auth` entry is silently *not rendered*
 * rather than rendered-then-redirected; no lock icon leaks its existence.
 */
export function isNavEntryVisible(
  nav: PublicNav | undefined,
  key: PublicNavKey,
  isLoggedIn: boolean,
  docsUrl?: string,
): boolean {
  const entry = navEntryOf(nav, key)
  if (!entry.enabled) return false
  if (entry.require_auth && !isLoggedIn) return false
  if (key === 'docs' && !isSafePublicHref(docsUrl)) return false
  return true
}

/** Visible entries in render order; never leaves an empty slot. */
export function visibleNavItems(
  nav: PublicNav | undefined,
  isLoggedIn: boolean,
  docsUrl?: string,
): PublicNavItem[] {
  const items: PublicNavItem[] = []
  for (const key of PUBLIC_NAV_ORDER) {
    if (!isNavEntryVisible(nav, key, isLoggedIn, docsUrl)) continue
    const href = key === 'docs' ? (docsUrl ?? '').trim() : PUBLIC_NAV_HREFS[key]
    items.push({ key, href, external: key === 'docs' && isExternalHref(href) })
  }
  return items
}

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
 * Returns the redirect target for a disabled/`require_auth` public route, or
 * `null` when the current visitor may stay. Routing itself is wired in the
 * next wave (pricing/rankings routes do not exist yet); this pure helper is
 * what those route guards will call from `beforeLoad`.
 *
 * - `enabled=false` for the section → silent redirect to `/` (F-09-35:
 *   disabled public pages are not a 401 and no sign-in prompt is rendered).
 * - `require_auth=true` and guest → `/sign-in?redirect=<pathname+search>`.
 * - Anything else → allow.
 */
export function publicRouteGuardRedirect(
  pathname: string,
  search: string,
  nav: PublicNav | undefined,
  isLoggedIn: boolean,
): string | null {
  const key = sectionForPath(pathname)
  if (key === null) return null
  const entry = navEntryOf(nav, key)
  if (!entry.enabled) return '/'
  if (entry.require_auth && !isLoggedIn) return signInHref(pathname, search)
  return null
}

/** Maps a public pathname to its nav section; unknown paths are unguarded. */
export function sectionForPath(pathname: string): PublicNavKey | null {
  if (pathname === '/') return 'home'
  if (pathname === '/pricing' || pathname.startsWith('/pricing/')) return 'pricing'
  if (pathname === '/rankings' || pathname.startsWith('/rankings/')) return 'rankings'
  if (pathname === '/docs') return 'docs'
  if (pathname === '/about' || pathname.startsWith('/about/')) return 'about'
  return null
}
