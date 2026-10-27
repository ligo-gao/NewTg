import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import { useTheme } from '@/app/providers/theme'
import type { PublicSite } from '@/lib/public-status'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useLanguageSelection } from '@/lib/use-language-selection'
import { useI18n, type Locale } from '@/shared/lib/i18n-context'

import { LANGUAGE_LABELS, LanguageSwitch } from '@/shared/ui/language-switch'
import { MobileNavPanel } from './mobile-nav-panel'
import {
  currentSearch,
  signInHref,
  visibleNavItems,
  type PublicNavItem,
  type PublicNavKey,
} from './nav-visibility'
import { Menu01Icon } from './public-icons'

const DEFAULT_SITE_TITLE = 'NEWTG'

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
  className?: string
}

/**
 * F-09-34 brand mark. Same wave glyph, geometry and stroke weight as the
 * marketing header (website SiteHeader), so moving between `/` and the SPA
 * pages does not swap the logo. Coloured by `.brand-mark` with the shared
 * signal token; the mark is `aria-hidden` because the adjacent title already
 * names the link.
 */
function RelayMark() {
  return (
    <svg
      className="brand-mark"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M3 6h3.5c2 0 3.5 1.2 4.5 3l2 6c1 1.8 2.5 3 4.5 3H21" />
      <path d="M3 18h3.5c2 0 3.5-1.2 4.5-3l2-6c1-1.8 2.5-3 4.5-3H21" />
    </svg>
  )
}

/**
 * F-09-34 brand block: logo when configured, otherwise the relay mark, in both
 * cases followed by `site.title`. `nav.home.enabled=false` hides the Home
 * *entry* only — the logo always links back to `/` (F-09-34 special semantics).
 * The mark is `aria-hidden` because the adjacent title already names the link.
 */
export function SiteBrand({ site, className }: SiteBrandProps) {
  const title = site?.title?.trim() || DEFAULT_SITE_TITLE
  const logo = safeLogoUrl(site?.logo_url)

  return (
    <a href="/" className={['brand', className].filter(Boolean).join(' ')}>
      {logo !== null ? (
        // Intrinsic size is set so the header does not reflow when the image
        // arrives (F-09-06 CLS budget).
        <img src={logo} alt="" width={22} height={22} className="brand-logo" />
      ) : (
        <RelayMark />
      )}
      <span className="brand-name">{title}</span>
    </a>
  )
}

function navLabels(t: (key: string) => string): Record<PublicNavKey, string> {
  return {
    home: t('public.nav.home'),
    console: t('public.nav.console'),
    models: t('public.nav.models'),
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

/**
 * Short codes for the collapsed header. Written as codes rather than glyphs so
 * the i18n lint gate (no CJK literals in source) stays green.
 */
const COMPACT_LANGUAGE_LABELS: Record<Locale, string> = {
  en: 'EN',
  'zh-CN': 'ZH',
  'zh-TW': 'TW',
  fr: 'FR',
  ru: 'RU',
  ja: 'JA',
  vi: 'VI',
}

const ALL_LOCALES = Object.keys(COMPACT_LANGUAGE_LABELS) as Locale[]

/**
 * Narrow language control shown below 1024px. The visible code belongs to the
 * selected locale; the transparent native select keeps keyboard and screen
 * reader behaviour identical to the full `LanguageSwitch`.
 */
function CompactLanguage({
  label,
  onChange,
}: {
  label: string
  onChange?: (locale: Locale) => void
}) {
  const { locale, setLocale, availableLocales } = useI18n()
  const locales = availableLocales ?? ALL_LOCALES

  return (
    <label className="home-lang-compact">
      <span aria-hidden="true">{COMPACT_LANGUAGE_LABELS[locale]}</span>
      <select
        aria-label={label}
        value={locale}
        onChange={(event) => {
          const next = event.target.value
          if (!locales.includes(next as Locale)) return
          setLocale?.(next as Locale)
          onChange?.(next as Locale)
        }}
      >
        {locales.map((item) => (
          <option key={item} value={item}>
            {LANGUAGE_LABELS[item]}
          </option>
        ))}
      </select>
    </label>
  )
}

/**
 * One theme control at every width (the marketing header keeps the toggle in
 * both the expanded and the collapsed row). Moon in light mode, sun in dark.
 */
function ThemeButton() {
  const { t } = useI18n()
  const { resolved, setMode } = useTheme()
  const switchToDark = resolved === 'light'

  return (
    <button
      type="button"
      className="theme-btn"
      aria-label={
        switchToDark ? t('public.nav.theme.dark') : t('public.nav.theme.light')
      }
      onClick={() => setMode(switchToDark ? 'dark' : 'light')}
    >
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        width={16}
        height={16}
        fill="none"
        stroke="currentColor"
        strokeWidth={1.7}
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        {switchToDark ? (
          <path d="M20.5 14.6A8.5 8.5 0 0 1 9.4 3.5a8.5 8.5 0 1 0 11.1 11.1Z" />
        ) : (
          <>
            <circle cx="12" cy="12" r="4" />
            <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
          </>
        )}
      </svg>
    </button>
  )
}

interface AuthClusterProps {
  isLoggedIn: boolean
  signInTarget: string
  activePath: string
}

/**
 * Session-aware right cluster (F-09-34/35): guests get the Sign in hand-off,
 * signed-in visitors get the avatar menu with Console as the first item.
 * The marketing header carries the same Sign in entry; neither header renders
 * a sign-up call to action (业主裁定：顶栏只保留 导航 / 语言 / 明暗 / 登录).
 */
function AuthCluster({ isLoggedIn, signInTarget, activePath }: AuthClusterProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

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
      <a href={signInTarget} className="sign-in">
        {t('public.nav.sign_in')}
      </a>
    )
  }

  return (
    <div ref={menuRef} className="relative">
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
        <div role="menu" className="avatar-menu">
          <a
            role="menuitem"
            href="/dashboard"
            aria-current={activePath === '/dashboard' ? 'page' : undefined}
          >
            {t('public.nav.console')}
          </a>
          <a role="menuitem" href="/profile">
            {t('public.nav.profile')}
          </a>
          <div className="avatar-rule" />
          <button type="button" role="menuitem" onClick={() => void signOut()}>
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
 * F-09-34 public header, shared by every public page. Structure mirrors the
 * marketing site's header (fixed 64px row, blurred nav surface, text nav,
 * language pill, theme toggle, auth cluster, primary CTA).
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
  const [menuOpen, setMenuOpen] = useState(false)

  const site = status.data?.site
  const isLoggedIn = me.isSuccess
  const { onChange } = useLanguageSelection(isLoggedIn)
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

  return (
    <header className="site-header">
      <div className="wrap header-row">
        <SiteBrand site={site} />
        {status.isPending ? (
          <div aria-hidden="true" className="flex items-center gap-3">
            <span className="nav-skeleton w-12" />
            <span className="nav-skeleton w-16" />
            <span className="nav-skeleton w-14" />
          </div>
        ) : (
          <PrimaryNav items={items} activePath={activePath} labels={labels} />
        )}
        <div className="header-tools">
          <LanguageSwitch
            className="home-lang"
            label={t('public.language.label')}
            onChange={onChange}
            globe
          />
          <CompactLanguage
            label={t('public.language.label')}
            onChange={onChange}
          />
          <ThemeButton />
          <AuthCluster
            isLoggedIn={isLoggedIn}
            signInTarget={signInTarget}
            activePath={path}
          />
          <button
            type="button"
            aria-label={
              menuOpen ? t('public.nav.close') : t('public.nav.open_menu')
            }
            aria-expanded={menuOpen}
            aria-controls="mobile-menu"
            onClick={() => setMenuOpen((open) => !open)}
            className="menu-btn"
          >
            {menuOpen ? <CloseGlyph /> : <Menu01Icon size={20} strokeWidth={1.5} />}
          </button>
        </div>
      </div>
      <MobileNavPanel
        open={menuOpen}
        onOpenChange={setMenuOpen}
        items={items}
        activePath={activePath}
        labels={labels}
        isLoggedIn={isLoggedIn}
        signInTarget={signInTarget}
      />
    </header>
  )
}

function CloseGlyph() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" width={16} height={16} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round">
      <path d="M3 3l10 10M13 3L3 13" />
    </svg>
  )
}
