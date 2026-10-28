import type { ShellCopy } from './copy'

/** Brand lockup: the wave mark (or a configured logo) followed by the site name. */
export interface ShellBrand {
  title: string
  href: string
  /** Absolute http(s) URL or site-relative path; anything else falls back to the mark. */
  logoUrl?: string | null
}

/** One header navigation entry. The label comes from the shared dictionary. */
export interface ShellNavItem {
  key: string
  label: string
  href: string
  external?: boolean
  current?: boolean
}

export interface ShellLocaleOption {
  value: string
  label: string
  short: string
}

/** Controlled language control; each app wires its own persistence/navigation. */
export interface ShellLocaleControl {
  value: string
  options: readonly ShellLocaleOption[]
  onChange: (value: string) => void
}

export interface ShellThemeControl {
  value: 'light' | 'dark'
  onToggle: () => void
}

/**
 * Right-hand session cluster. Guests see the sign-in hand-off; signed-in
 * visitors get the avatar menu (console, profile, sign out). Optional: the
 * marketing site has no session and simply passes `undefined`.
 */
export interface ShellAccountControl {
  signedIn: boolean
  signInHref: string
  /**
   * Display name shown next to the avatar (`display_name`, else `username`).
   * Omitted means the host has no name yet: the button then shows the avatar
   * alone instead of an empty label.
   */
  name?: string
  menu?: readonly ShellNavItem[]
  onSignOut?: () => void
}

export interface ShellHeaderProps {
  copy: ShellCopy
  brand: ShellBrand
  items: readonly ShellNavItem[]
  locale: ShellLocaleControl
  theme: ShellThemeControl
  account?: ShellAccountControl
  /** Status is still loading: render same-height placeholders, not empty space. */
  loading?: boolean
}

export interface ShellFooterLink {
  label: string
  href: string
  external?: boolean
  /** Draws the live-status dot before the label. */
  dot?: boolean
}

export interface ShellFooterColumn {
  key: string
  title: string
  links?: readonly ShellFooterLink[]
  /**
   * Pre-sanitized operator HTML (`site.footer_html`) rendered instead of the
   * link list. The host is responsible for sanitizing it before it gets here.
   */
  html?: string
}

export interface ShellFooterProps {
  copy: ShellCopy
  brand: ShellBrand
  /** The site statement paragraph. Omitted when the host has nothing to say. */
  statement?: string
  /**
   * Absolute http(s) API base (for example `https://gateway.example.com/v1`),
   * rendered as a copyable strip. Hidden when omitted; the host decides
   * whether its configured address is public enough to show.
   */
  endpoint?: string
  columns: readonly ShellFooterColumn[]
  /** Rendered as its own column when present (the shell's language switcher). */
  locale?: ShellLocaleControl
  copyright: string
  filing?: { label: string; href: string } | null
}
