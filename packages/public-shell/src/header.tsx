import { useEffect, useRef, useState } from 'react'

import { ShellBrandMark } from './brand'
import type { ShellCopy } from './copy'
import { AvatarGlyph, CloseGlyph, ExternalGlyph, MenuGlyph } from './icons'
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

function PrimaryNav({ items, copy }: { items: readonly ShellNavItem[]; copy: ShellCopy }) {
  // Every `nav.*` switch can be off (a legal configuration): the header then
  // simply has no navigation block, never an empty one.
  if (items.length === 0) return null
  return (
    <nav aria-label={copy['nav.primary']} className="ps-nav">
      {items.map((item) => (
        <a
          key={item.key}
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

  return (
    <div ref={menuRef} className="ps-account">
      <button
        type="button"
        aria-label={copy['nav.account']}
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        onClick={() => setMenuOpen((open) => !open)}
        className="ps-avatar-btn"
      >
        <AvatarGlyph />
      </button>
      {menuOpen ? (
        <div role="menu" className="ps-avatar-menu">
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
              <div className="ps-avatar-rule" />
              <button type="button" role="menuitem" onClick={account.onSignOut}>
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
          <ShellLanguageControl control={locale} copy={copy} variant="compact" />
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
