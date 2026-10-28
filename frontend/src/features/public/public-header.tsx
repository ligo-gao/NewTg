import { ShellBrandMark, ShellHeader, type ShellBrand, type ShellNavItem } from '@newtg/public-shell'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { useTheme } from '@/app/providers/theme'
import type { PublicSite } from '@/lib/public-status'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useLanguageSelection } from '@/lib/use-language-selection'
import { useI18n, type Locale } from '@/shared/lib/i18n-context'

import {
  currentSearch,
  sectionForPath,
  signInHref,
  visibleNavItems,
  type PublicNavItem,
} from './nav-visibility'
import { NAV_COPY_KEY, shellCopy, shellLocaleOptions } from './shell-copy'

const DEFAULT_SITE_TITLE = 'NEWTG'

/** Brand data for the shell: the SPA always links the mark back to `/`. */
export function siteBrand(site?: PublicSite): ShellBrand {
  return {
    title: site?.title?.trim() || DEFAULT_SITE_TITLE,
    href: '/',
    logoUrl: site?.logo_url,
  }
}

export interface SiteBrandProps {
  site?: PublicSite
  className?: string
}

/**
 * Standalone brand lockup for page sections that are not inside the header or
 * footer (the empty-home page). Wrapped in `.ps-shell` so the shell's tokens
 * resolve outside its own chrome.
 */
export function SiteBrand({ site, className }: SiteBrandProps) {
  return (
    <span className="ps-shell">
      <ShellBrandMark brand={siteBrand(site)} className={className} />
    </span>
  )
}

function toShellItems(
  items: readonly PublicNavItem[],
  copy: ReturnType<typeof shellCopy>,
  activePath?: string,
): ShellNavItem[] {
  // 高亮按「所属分区」而不是路径全等：`/pricing/<id>` 仍属于定价、
  // `/models/<id>` 仍属于模型库。旧的 `activePath === item.href` 会让
  // 模型详情页上一个导航项都不亮。
  const active = activePath === undefined ? null : sectionForPath(activePath)
  return items.map((item) => ({
    key: item.key,
    label: copy[NAV_COPY_KEY[item.key]],
    href: item.href,
    external: item.external,
    current: active === item.key,
  }))
}

export interface PublicHeaderProps {
  /** Current route pathname; drives `aria-current`. */
  activePath?: string
}

/**
 * F-09-34 public header. The markup, styling and copy live in
 * `@newtg/public-shell`; this component only supplies the SPA's data — the
 * server's nav switches, the session, the active route and the locale runtime.
 *
 * `/api/status` is `no-store` and shared through one React Query entry, so the
 * header, footer and page sections observe the same snapshot (R-PUB-06).
 * `/api/me` is probed in parallel; a 401 is the guest answer and is never
 * surfaced (web-00 §9.2).
 */
export function PublicHeader({ activePath }: PublicHeaderProps) {
  const { locale, setLocale, availableLocales } = useI18n()
  const { resolved, setMode } = useTheme()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const queryClient = useQueryClient()
  const { onChange } = useLanguageSelection(me.isSuccess)

  const copy = shellCopy(locale)
  const site = status.data?.site
  const path = activePath ?? '/'
  // F-09-34 异常流程: with a failed status and no cached snapshot the shell must
  // not guess operator switches — render brand + session cluster only.
  const degraded = status.isError && status.data === undefined
  const items = degraded
    ? []
    : toShellItems(visibleNavItems(status.data?.nav, me.isSuccess, site?.docs_url), copy, activePath)

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

  function changeLocale(next: string) {
    const value = next as Locale
    setLocale?.(value)
    onChange?.(value)
  }

  return (
    <ShellHeader
      copy={copy}
      brand={siteBrand(site)}
      items={items}
      loading={status.isPending}
      locale={{
        value: locale,
        options: shellLocaleOptions(availableLocales),
        onChange: changeLocale,
      }}
      theme={{
        value: resolved,
        onToggle: () => setMode(resolved === 'light' ? 'dark' : 'light'),
      }}
      account={{
        signedIn: me.isSuccess,
        signInHref: signInHref(path, currentSearch()),
        name: me.data?.display_name?.trim() || me.data?.username,
        menu: [
          {
            key: 'console',
            label: copy['nav.console'],
            href: '/dashboard',
            current: path === '/dashboard',
          },
          { key: 'profile', label: copy['nav.profile'], href: '/profile' },
        ],
        onSignOut: () => void signOut(),
      }}
    />
  )
}
