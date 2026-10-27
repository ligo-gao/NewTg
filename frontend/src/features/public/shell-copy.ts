import {
  SHELL_LOCALE_LABELS,
  SHELL_LOCALE_SHORT,
  getShellCopy,
  type ShellCopy,
  type ShellCopyKey,
  type ShellLocaleOption,
} from '@newtg/public-shell'

import type { Locale } from '@/shared/lib/i18n-context'

import type { PublicNavKey } from './nav-visibility'

/**
 * Glue between the SPA's runtime (locale context, server-advertised language
 * list, nav switches) and the shared shell package. The header and footer both
 * read copy through here, so the shell's dictionary is the only source for
 * their strings.
 */
export const NAV_COPY_KEY: Record<PublicNavKey, ShellCopyKey> = {
  home: 'nav.home',
  console: 'nav.console',
  models: 'nav.models',
  pricing: 'nav.pricing',
  rankings: 'nav.rankings',
  docs: 'nav.docs',
  about: 'nav.about',
}

/** Shell copy for the active locale, falling back to English like the package. */
export function shellCopy(locale: Locale): ShellCopy {
  return getShellCopy(locale)
}

/**
 * Options for the shell's language control, in the order the server advertises
 * (the provider hands back the bundled intersection). Unknown identifiers are
 * dropped rather than rendered with a missing name.
 */
export function shellLocaleOptions(
  locales: readonly Locale[] | undefined,
): ShellLocaleOption[] {
  const list = locales && locales.length > 0 ? locales : (Object.keys(SHELL_LOCALE_LABELS) as Locale[])
  return list.map((value) => ({
    value,
    label: SHELL_LOCALE_LABELS[value] ?? value,
    short: SHELL_LOCALE_SHORT[value] ?? value.toUpperCase(),
  }))
}
