import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import type { PublicSite } from '@/lib/public-status'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { LanguageSwitch } from './language-switch'
import { MobileNavDrawer } from './mobile-nav-drawer'
import {
  currentSearch,
  signInHref,
  visibleNavItems,
  type PublicNavItem,
  type PublicNavKey,
} from './nav-visibility'
import { Menu01Icon } from './public-icons'

const DEFAULT_SITE_TITLE = 'NewTG'

/** Only site-relative paths or absolute http(s) URLs may become an `<img src>`. */
function safeLogoUrl(raw: string | undefined): string | null {
  const value = (raw ?? '').trim()
  if (value === '') return null
  if (value.startsWith('/') && !value.startsWith('//')) return value
  if (/^https?:\/\//i.test(value)) return value
  return null
}

export interface SiteBrandProps {
  site?: PublicSite
  /** Render the wordmark; the footer brand column does, the console does not. */
  showTitle?: boolean
  className?: string
}

/**
 * F-09-34 brand block: logo when configured, otherwise a first-letter tile, in
 * both cases followed by `site.title`. `nav.home.enabled=false` hides the Home
 * *entry* only — the logo always links back to `/` (F-09-34 special semantics).
 * The tile is `aria-hidden` because the adjacent title already names the link.
 */
/**
 * F-09-34 brand mark. Three upstream lanes converge on one output node; the
 * copper node is the gateway's only piece of ornament, and it is also the
 * mark used in the footer. Operator logos still win when configured.
 */
function RelayMark() {
  return (
    <svg className="brand-mark" viewBox="0 0 28 28" fill="none" aria-hidden="true">
      <path d="M2 5h5l5 9" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M2 14h10" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      <path d="M2 23h5l5-9" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M12 14h8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      <circle className="mark-dot" cx="22" cy="14" r="2.2" />
    </svg>
  )
}

export function SiteBrand({ site, showTitle = true, className }: SiteBrandProps) {
  const title = site?.title?.trim() || DEFAULT_SITE_TITLE
  const logo = safeLogoUrl(site?.logo_url)

  return (
    <a
      href="/"
      className={['brand', className].filter(Boolean).join(' ')}
    >
      {logo !== null ? (
        // Intrinsic size is set so the header does not reflow when the image
        // arrives (F-09-06 CLS budget).
        <img src={logo} alt="" width={30} height={30} className="brand-logo" />
      ) : (
        <RelayMark />
      )}
      {showTitle ? <span className="brand-name">{title}</span> : null}
    </a>
  )
}

function navLabels(t: (key: string) => string): Record<PublicNavKey, string> {
  return {
    home: t('public.nav.home'),
    console: t('public.nav.console'),
    pricing: t('public.nav.pricing'),
    rankings: t('public.nav.rankings'),
    docs: t('public.nav.docs'),
    about: t('public.nav.about'),
  }
}

/**
 * Desktop inline nav. When every `nav.*` switch is off (a legal configuration)
 * this returns an empty list and the header simply has no navigation block —
 * no empty slot, matching F-09-34 "关掉最后一项" ruling.
 */
function PrimaryNav({
  items,
  activePath,
  labels,
}: {
  items: readonly PublicNavItem[]
  activePath?: string
  labels: Record<PublicNavKey, string>
}) {
  const { t } = useI18n()
  if (items.length === 0) return null

  return (
    <nav aria-label={t('public.nav.primary')} className="nav">
      {items.map((item) => {
        const active = activePath === item.href
        return (
          <a
            key={item.key}
            href={item.href}
            aria-current={active ? 'page' : undefined}
            target={item.external ? '_blank' : undefined}
            rel={item.external ? 'noopener noreferrer' : undefined}
            aria-label={
              item.external
                ? `${labels[item.key]} (${t('public.nav.external')})`
                : undefined
            }
            className="nav-link"
          >
            {labels[item.key]}
            {item.external ? <ArrowUpRightGlyph /> : null}
          </a>
        )
      })}
    </nav>
  )
}

function ArrowUpRightGlyph() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" className="size-3.5" fill="none" stroke="currentColor" strokeWidth="1.5">
      <path d="M5 11 11 5M6 5h5v5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

interface AuthClusterProps {
  isLoggedIn: boolean
  signupEnabled: boolean
  signInTarget: string
  activePath: string
  /**
   * `header` keeps the desktop-only visibility classes; `drawer` renders the
   * same cluster inside the mobile sheet where those classes would hide it.
   */
  presentation?: 'header' | 'drawer'
}

/**
 * Session-aware right cluster (F-09-34/35): guests get Sign in / Sign up,
 * signed-in visitors get the avatar menu with Console as the first item.
 * There is deliberately no "you are signed in" banner.
 */
function AuthCluster({
  isLoggedIn,
  signupEnabled,
  signInTarget,
  activePath,
  presentation = 'header',
}: AuthClusterProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const inHeader = presentation === 'header'
  // Desktop hides these controls behind the hamburger; the drawer shows them.
  const guestLinkClass = inHeader ? 'hidden md:inline-flex' : 'inline-flex'
  const avatarWrapperClass = inHeader ? 'relative hidden md:block' : 'relative'
  const avatarMenuClass = 'avatar-menu'

  // Avatar menu keyboard contract: Escape closes and returns focus to the
  // trigger. Pointer users close it by choosing an item (links navigate away).
  useEffect(() => {
    if (!menuOpen) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      setMenuOpen(false)
      menuRef.current?.querySelector<HTMLElement>('button')?.focus()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [menuOpen])

  async function signOut() {
    try {
      await apiClient.POST('/api/logout')
    } finally {
      // A failed logout must still drop every cached identity: keeping the
      // avatar or another user's cached data on a public page is worse than a
      // server-side session that is already gone.
      queryClient.removeQueries({ queryKey: ME_QUERY_KEY })
      queryClient.clear()
      window.location.assign('/')
    }
  }

  if (!isLoggedIn) {
    return (
      <div className="flex items-center gap-2">
        <a
          href={signInTarget}
          className={`sign-in ${guestLinkClass}`}
        >
          {t('public.nav.sign_in')}
        </a>
        {signupEnabled ? (
          <a
            href="/sign-up"
            className={`btn btn-sm ${guestLinkClass}`}
          >
            {t('public.nav.sign_up')}
          </a>
        ) : null}
      </div>
    )
  }

  return (
    <div ref={menuRef} className={avatarWrapperClass}>
      <button
        type="button"
        aria-label={t('public.nav.account')}
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        onClick={() => setMenuOpen((open) => !open)}
        className="avatar-btn"
      >
        <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
          <circle cx="8" cy="5.5" r="2.5" />
          <path d="M3.5 13c.8-2 2.5-3 4.5-3s3.7 1 4.5 3" strokeLinecap="round" />
        </svg>
              </button>
      {menuOpen ? (
        <div
          role="menu"
          className={avatarMenuClass}
        >
          <a
            role="menuitem"
            href="/dashboard"
            aria-current={activePath === '/dashboard' ? 'page' : undefined}
          >
            {t('public.nav.console')}
          </a>
          <a
            role="menuitem"
            href="/profile"
          >
            {t('public.nav.profile')}
          </a>
          <div className="avatar-rule" />
          <button
            type="button"
            role="menuitem"
            onClick={() => void signOut()}
          >
            {t('public.nav.sign_out')}
          </button>
        </div>
      ) : null}
    </div>
  )
}

export interface PublicHeaderProps {
  /** Current route pathname; drives `aria-current`. */
  activePath?: string
}

/**
 * F-09-34 public header, shared by every public page.
 *
 * `/api/status` is `no-store` and shared through one React Query entry, so the
 * header, footer and page sections observe the same snapshot (R-PUB-06).
 * `/api/me` is probed in parallel; a 401 is the guest answer and is never
 * surfaced (web-00 §9.2). When status is still loading the header renders the
 * same-height brand and up to three nav placeholders, preventing CLS.
 */
export function PublicHeader({ activePath }: PublicHeaderProps) {
  const { t } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const [drawerOpen, setDrawerOpen] = useState(false)

  const site = status.data?.site
  const isLoggedIn = me.isSuccess
  const path = activePath ?? '/'
  const signInTarget = signInHref(path, currentSearch())
  // F-09-34 异常流程: with a failed status and no cached snapshot the shell must
  // not guess operator switches — render brand + session cluster only, with
  // every nav entry hidden.
  const degraded = status.isError && status.data === undefined
  const items = degraded
    ? []
    : visibleNavItems(status.data?.nav, isLoggedIn, site?.docs_url)
  const labels = navLabels(t)
  const signupEnabled = status.data?.auth.signup_enabled !== false

  return (
    <header className="site-header">
      <div className="wrap header-row">
        <SiteBrand site={site} />
        {status.isPending ? (
          <div aria-hidden="true" className="hidden items-center gap-3 md:flex">
            <span className="nav-skeleton w-12" />
            <span className="nav-skeleton w-16" />
            <span className="nav-skeleton w-14" />
          </div>
        ) : (
          <PrimaryNav items={items} activePath={activePath} labels={labels} />
        )}
        <div className="header-tools">
          <LanguageSwitch className="home-lang" />
          <AuthCluster
            isLoggedIn={isLoggedIn}
            signupEnabled={signupEnabled}
            signInTarget={signInTarget}
            activePath={path}
          />
          <button
            type="button"
            aria-label={t('public.nav.open_menu')}
            aria-expanded={drawerOpen}
            onClick={() => setDrawerOpen(true)}
            className="menu-btn"
          >
            <Menu01Icon size={20} strokeWidth={1.5} />
          </button>
        </div>
      </div>
      <MobileNavDrawer
        open={drawerOpen}
        onOpenChange={setDrawerOpen}
        items={items}
        activePath={activePath}
        labels={labels}
        footer={
          <div className="flex flex-col gap-4">
            <LanguageSwitch variant="footer" />
            <AuthCluster
              isLoggedIn={isLoggedIn}
              signupEnabled={signupEnabled}
              signInTarget={signInTarget}
              activePath={path}
              presentation="drawer"
            />
          </div>
        }
      />
    </header>
  )
}
