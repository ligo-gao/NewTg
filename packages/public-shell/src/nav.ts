import type { ShellCopyKey } from './copy'

/**
 * The public navigation, resolved in one place for both apps.
 *
 * The header markup already lives here (`ShellHeader`), but until now each app
 * built its own item list: the SPA filtered `/api/status` switches, the
 * marketing site carried a hard-coded array. Same component, two answers — a
 * guest saw the console entry on `/` but not on `/models`. Everything that
 * decides *which* entries exist, in what order, with which href, lives here so
 * that difference cannot come back.
 */

export type PublicNavKey =
  | 'home'
  | 'console'
  | 'models'
  | 'pricing'
  | 'rankings'
  | 'docs'
  | 'about'

/** One resolved entry: label is the host's job, the rest is decided here. */
export interface PublicNavItem {
  key: PublicNavKey
  href: string
  /** True only for `docs` when the operator configured an absolute http(s) URL. */
  external: boolean
}

/** The switch pair as `GET /api/status` reports it; both fields are optional. */
export interface PublicNavEntryState {
  enabled?: boolean
  require_auth?: boolean
}

export type PublicNavSwitches = Partial<Record<PublicNavKey, PublicNavEntryState>>

// 品牌标就是首页入口；头部不再单列 home。
// 排序：控制台在首位，其后是模型库、定价、排行、关于；文档最后。
export const PUBLIC_NAV_ORDER = [
  'console',
  'models',
  'pricing',
  'rankings',
  'about',
  'docs',
] as const

export const PUBLIC_NAV_HREFS: Record<PublicNavKey, string> = {
  home: '/',
  console: '/dashboard',
  models: '/models',
  pricing: '/pricing',
  rankings: '/rankings',
  docs: '/docs',
  about: '/about',
}

/** Key → shell dictionary entry, so both apps label the same list the same way. */
export const NAV_COPY_KEY: Record<PublicNavKey, ShellCopyKey> = {
  home: 'nav.home',
  console: 'nav.console',
  models: 'nav.models',
  pricing: 'nav.pricing',
  rankings: 'nav.rankings',
  docs: 'nav.docs',
  about: 'nav.about',
}

/**
 * Missing keys keep the documented defaults (F-09-34 异常流程: enabled=true,
 * require_auth=false). `console` is the exception — its `require_auth` is
 * locked to true server-side, so a missing value must not be read as "public".
 */
export function navEntryOf(
  nav: PublicNavSwitches | undefined,
  key: PublicNavKey,
): Required<PublicNavEntryState> {
  const entry = nav?.[key]
  return {
    enabled: entry?.enabled ?? true,
    require_auth: entry?.require_auth ?? key === 'console',
  }
}

/**
 * Absolute `http(s)` targets open in a new tab; a same-site relative path (for
 * example `/guide`) is navigated in place (F-09-38 分支流程).
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
 * `enabled && (!require_auth || signedIn)` — the frozen derived rule from
 * F-09-34 — with one product decision on top:
 *
 * 控制台入口对访客也显示（2026-10-28，以首页导航为准）。受保护的是
 * `/dashboard` 本身，由 `_console` 守卫在进场时把访客送去登录；入口本身
 * 只是路标，首页四个公开页看到同一份菜单比提前隐藏更有用。
 *
 * `nav.docs` additionally requires a safe, non-empty `site.docs_url`
 * (logical AND, F-09-38).
 */
export function isNavEntryVisible(
  nav: PublicNavSwitches | undefined,
  key: PublicNavKey,
  signedIn: boolean,
  docsUrl?: string,
): boolean {
  const entry = navEntryOf(nav, key)
  if (!entry.enabled) return false
  if (key !== 'console' && entry.require_auth && !signedIn) return false
  if (key === 'docs' && !isSafePublicHref(docsUrl)) return false
  return true
}

/** Visible entries in render order; never leaves an empty slot. */
export function visibleNavItems(
  nav: PublicNavSwitches | undefined,
  signedIn: boolean,
  docsUrl?: string,
): PublicNavItem[] {
  const items: PublicNavItem[] = []
  for (const key of PUBLIC_NAV_ORDER) {
    if (!isNavEntryVisible(nav, key, signedIn, docsUrl)) continue
    const href = key === 'docs' ? (docsUrl ?? '').trim() : PUBLIC_NAV_HREFS[key]
    items.push({ key, href, external: key === 'docs' && isExternalHref(href) })
  }
  return items
}

/**
 * Maps a *public* pathname to the nav entry it belongs to. Both apps pass an
 * unprefixed path (the marketing site strips its `/en/` prefix first), so a
 * model detail page highlights 定价 and `/announcements/<id>` highlights
 * nothing. Console paths stay `null`: they render the console layout, never
 * this header.
 */
export function sectionForPath(pathname: string): PublicNavKey | null {
  const path = pathname.replace(/\/+$/, '') || '/'
  if (path === '/') return 'home'
  if (path === '/models' || path.startsWith('/models/')) return 'models'
  if (path === '/pricing' || path.startsWith('/pricing/')) return 'pricing'
  if (path === '/rankings' || path.startsWith('/rankings/')) return 'rankings'
  if (path === '/docs') return 'docs'
  if (path === '/about' || path.startsWith('/about/')) return 'about'
  return null
}
