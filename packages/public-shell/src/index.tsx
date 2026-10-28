/**
 * `@newtg/public-shell` — the one header and footer every public page renders.
 *
 * Both the SPA (`frontend/`) and the marketing site (`website/`) mount these
 * components, so markup, styling and copy live here and nowhere else. Hosts
 * supply data only: navigation items, the active route, the session cluster,
 * and their own locale persistence in `onChange`.
 *
 * Styles: import `@newtg/public-shell/styles.css` once per app.
 */

export { ShellHeader } from './header'
export { ShellFooter } from './footer'
export { ShellBrandMark } from './brand'
export { ShellLanguageControl } from './language-control'
export { ShellThemeButton } from './theme-control'
export { FRIEND_LINKS } from './friend-links'

export {
  COPY_BY_LOCALE,
  SHELL_LOCALES,
  SHELL_LOCALE_LABELS,
  SHELL_LOCALE_SHORT,
  formatShellCopy,
  getShellCopy,
  isShellLocale,
} from './copy'
export type { ShellCopy, ShellCopyKey, ShellLocale } from './copy'

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
} from './nav'
export type {
  PublicNavEntryState,
  PublicNavItem,
  PublicNavKey,
  PublicNavSwitches,
} from './nav'

export type {
  ShellAccountControl,
  ShellBrand,
  ShellFooterColumn,
  ShellFooterLink,
  ShellFooterProps,
  ShellHeaderProps,
  ShellLocaleControl,
  ShellLocaleOption,
  ShellNavItem,
  ShellThemeControl,
} from './types'
