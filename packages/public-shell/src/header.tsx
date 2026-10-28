import { useEffect, useRef, useState } from 'react'

import { ShellBrandMark } from './brand'
import type { ShellCopy } from './copy'
import { AvatarGlyph, ChevronGlyph, CloseGlyph, ExternalGlyph, MenuGlyph } from './icons'
import { ShellLanguageControl } from './language-control'
import { ShellThemeButton } from './theme-control'
import type { ShellHeaderProps, ShellNavItem } from './types'

/** External entries announce the new tab the same way in nav, panel and footer. */
function linkAriaLabel(copy: ShellCopy, item: ShellNavItem): string | undefined {
  return item.external ? `${item.label} (${copy['nav.external']})` : undefined
}

function linkTargetProps(item: ShellNavItem) {
  return {
    target: item.external ? '_blank' : undefined,
    rel: item.external ? 'noopener noreferrer' : undefined,
  }
}

/**
 * One marker travels between the entries instead of every link painting its
 * own underline. Its position is written to CSS variables on the nav, so the
 * motion is pure `transform`/`width` on a single absolutely positioned node —
 * no re-render per frame and no effect on the row's layout.
 *
 * Measurement runs after mount, on resize, when the entries change and once
 * the webfonts settle (Inter/Noto swap in after first paint and change label
 * widths). `data-nav-ready` is set only after the first placement so the
 * marker does not travel in from x=0 on load.
 */
function useNavMarker(activeKey: string | null, signature: string) {
  const navRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    const nav = navRef.current
    if (!nav) return
    let frame = 0
    let readyFrame = 0

    const place = () => {
      const link = activeKey
        ? nav.querySelector<HTMLElement>(`[data-nav-key="${activeKey}"]`)
        : null
      if (!link) {
        nav.removeAttribute('data-nav-active')
        return
      }
      const row = nav.getBoundingClientRect()
      const box = link.getBoundingClientRect()
      nav.style.setProperty('--ps-nav-x', `${box.left - row.left}px`)
      nav.style.setProperty('--ps-nav-w', `${box.width}px`)
      nav.setAttribute('data-nav-active', '')
    }
    const schedule = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(place)
    }

    place()
    // Transitions open only after the placed marker has been painted once:
    // one rAF commits that frame, the next lands after it. Without the second
    // frame the marker would animate in from `width: 0` on every page load.
    if (!nav.hasAttribute('data-nav-ready')) {
      readyFrame = requestAnimationFrame(() => {
        readyFrame = requestAnimationFrame(() => nav.setAttribute('data-nav-ready', ''))
      })
    }

    window.addEventListener('resize', schedule)
    const observer =
      typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(schedule)
    observer?.observe(nav)
    void document.fonts?.ready.then(schedule)

    return () => {
      cancelAnimationFrame(frame)
      cancelAnimationFrame(readyFrame)
      window.removeEventListener('resize', schedule)
      observer?.disconnect()
    }
  }, [activeKey, signature])

  return navRef
}

function PrimaryNav({ items, copy }: { items: readonly ShellNavItem[]; copy: ShellCopy }) {
  const activeKey = items.find((item) => item.current)?.key ?? null
  const signature = items.map((item) => `${item.key}:${item.href}`).join('|')
  const navRef = useNavMarker(activeKey, signature)

  // Every `nav.*` switch can be off (a legal configuration): the header then
  // simply has no navigation block, never an empty one.
  if (items.length === 0) return null
  return (
    <nav ref={navRef} aria-label={copy['nav.primary']} className="ps-nav">
      {items.map((item) => (
        <a
          key={item.key}
          data-nav-key={item.key}
          href={item.href}
          aria-current={item.current ? 'page' : undefined}
          aria-label={linkAriaLabel(copy, item)}
          {...linkTargetProps(item)}
          className="ps-nav-link"
        >
          {item.label}
          {item.external ? <ExternalGlyph /> : null}
        </a>
      ))}
      {/* Decorative: the current entry is already announced by aria-current. */}
      <span aria-hidden="true" className="ps-nav-marker" />
    </nav>
  )
}

/**
 * Session-aware right cluster. Guests get the sign-in hand-off; signed-in
 * visitors get the avatar menu. Apps without a session (the static marketing
 * site) omit `account` entirely.
 */
function AccountCluster({
  account,
  copy,
}: {
  account: NonNullable<ShellHeaderProps['account']>
  copy: ShellCopy
}) {
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

  // Echoes the marketing avatar contract: Escape closes and returns focus to
  // the trigger; choosing an item navigates away and closes with it.
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

  if (!account.signedIn) {
    return (
      <a href={account.signInHref} className="ps-sign-in">
        {copy['nav.sign_in']}
      </a>
    )
  }

  const menu = account.menu ?? []
  const name = account.name?.trim() ?? ''

  return (
    <div ref={menuRef} className="ps-account">
      <button
        type="button"
        aria-label={name ? `${copy['nav.account']}: ${name}` : copy['nav.account']}
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        onClick={() => setMenuOpen((open) => !open)}
        className="ps-account-btn"
      >
        {name ? (
          <>
            <span aria-hidden="true" className="ps-account-avatar">
              {Array.from(name)[0]}
            </span>
            <span className="ps-account-name">{name}</span>
            <ChevronGlyph />
          </>
        ) : (
          <AvatarGlyph />
        )}
      </button>
      {menuOpen ? (
        <div role="menu" className="ps-account-menu" aria-label={copy['nav.account']}>
          {menu.map((item) => (
            <a
              key={item.key}
              role="menuitem"
              href={item.href}
              aria-current={item.current ? 'page' : undefined}
              {...linkTargetProps(item)}
            >
              {item.label}
            </a>
          ))}
          {account.onSignOut ? (
            <>
              <div className="ps-account-rule" />
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  setMenuOpen(false)
                  account.onSignOut?.()
                }}
              >
                {copy['nav.sign_out']}
              </button>
            </>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

/** Gives focus back to the hamburger that owns the panel. */
function focusMenuTrigger(): void {
  document.querySelector<HTMLElement>('[aria-controls="mobile-menu"]')?.focus()
}

/**
 * Collapsed-row navigation. A plain block under the fixed header — not a
 * modal — so the only focus contract is "give focus back when it closes".
 */
function MobileNav({
  open,
  onClose,
  items,
  copy,
  signInHref,
}: {
  open: boolean
  onClose: () => void
  items: readonly ShellNavItem[]
  copy: ShellCopy
  signInHref?: string
}) {
  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      onClose()
      focusMenuTrigger()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [open, onClose])

  if (!open) return null

  function close() {
    onClose()
    focusMenuTrigger()
  }

  return (
    <nav id="mobile-menu" aria-label={copy['nav.primary']} className="ps-mobile-nav">
      <div className="ps-wrap ps-mobile-nav-inner">
        {items.map((item) => (
          <a
            key={item.key}
            href={item.href}
            aria-current={item.current ? 'page' : undefined}
            aria-label={linkAriaLabel(copy, item)}
            {...linkTargetProps(item)}
            onClick={close}
            className="ps-mobile-nav-link"
          >
            {item.label}
          </a>
        ))}
        {signInHref ? (
          <a href={signInHref} onClick={close} className="ps-mobile-nav-link">
            {copy['nav.sign_in']}
          </a>
        ) : null}
      </div>
    </nav>
  )
}

/**
 * The public header used by every page of both apps. Data comes from the page
 * (nav switches, session, active route); markup and styling live here, so a
 * change made once shows up everywhere.
 */
export function ShellHeader({
  copy,
  brand,
  items,
  locale,
  theme,
  account,
  loading = false,
}: ShellHeaderProps) {
  const [menuOpen, setMenuOpen] = useState(false)
  const guestSignIn =
    account && !account.signedIn ? account.signInHref : undefined

  return (
    <header className="ps-shell ps-header">
      <div className="ps-wrap ps-header-row">
        <ShellBrandMark brand={brand} />
        {loading ? (
          // Same-height placeholders: the header never reflows when the
          // navigation arrives.
          <div aria-hidden="true" className="ps-nav">
            <span className="ps-nav-skeleton ps-nav-skeleton-sm" />
            <span className="ps-nav-skeleton ps-nav-skeleton-md" />
            <span className="ps-nav-skeleton ps-nav-skeleton-sm" />
          </div>
        ) : (
          <PrimaryNav items={items} copy={copy} />
        )}
        <div className="ps-header-tools">
          <ShellLanguageControl control={locale} copy={copy} variant="pill" />
          <ShellThemeButton control={theme} copy={copy} />
          {account ? <AccountCluster account={account} copy={copy} /> : null}
          <button
            type="button"
            aria-label={menuOpen ? copy['nav.close_menu'] : copy['nav.open_menu']}
            aria-expanded={menuOpen}
            aria-controls="mobile-menu"
            onClick={() => setMenuOpen((open) => !open)}
            className="ps-menu-btn"
          >
            {menuOpen ? <CloseGlyph /> : <MenuGlyph />}
          </button>
        </div>
      </div>
      <MobileNav
        open={menuOpen}
        onClose={() => setMenuOpen(false)}
        items={items}
        copy={copy}
        signInHref={guestSignIn}
      />
    </header>
  )
}
