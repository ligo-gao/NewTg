/**
 * Copy for the public shell (header + footer). This is the single dictionary
 * for both apps: the SPA (`frontend/`) and the marketing site (`website/`)
 * read the same file, so a label change happens in exactly one place.
 *
 * Keys are flat and dotted, matching the SPA's locale JSON conventions. Every
 * locale must carry the same key set — `copy.test.ts` fails otherwise.
 */

import en from './en.json'
import endonyms from './endonyms.json'
import fr from './fr.json'
import ja from './ja.json'
import ru from './ru.json'
import vi from './vi.json'
import zhCN from './zh-CN.json'
import zhTW from './zh-TW.json'

export const SHELL_LOCALES = ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi'] as const

export type ShellLocale = (typeof SHELL_LOCALES)[number]

/** Every string the shell renders. Templates use `{year}` / `{site}`. */
export interface ShellCopy {
  'nav.home': string
  'nav.models': string
  'nav.pricing': string
  'nav.console': string
  'nav.rankings': string
  'nav.docs': string
  'nav.about': string
  'nav.sign_in': string
  'nav.account': string
  'nav.profile': string
  'nav.sign_out': string
  'nav.primary': string
  'nav.external': string
  'nav.open_menu': string
  'nav.close_menu': string
  'nav.theme.dark': string
  'nav.theme.light': string
  'language.label': string
  'footer.product': string
  'footer.docs_column': string
  'footer.community': string
  'footer.legal': string
  'footer.resources': string
  'footer.language': string
  'footer.status': string
  'footer.terms': string
  'footer.privacy': string
  'footer.quickstart': string
  'footer.api_reference': string
  'footer.sdks': string
  'footer.changelog': string
  'footer.github': string
  'footer.discord': string
  'footer.x': string
  'footer.contact': string
  'footer.home_aria': string
  'footer.tagline': string
  'footer.copyright': string
  'footer.all_rights': string
}

export type ShellCopyKey = keyof ShellCopy

export const COPY_BY_LOCALE: Record<ShellLocale, ShellCopy> = {
  en: en as ShellCopy,
  'zh-CN': zhCN as ShellCopy,
  'zh-TW': zhTW as ShellCopy,
  fr: fr as ShellCopy,
  ru: ru as ShellCopy,
  ja: ja as ShellCopy,
  vi: vi as ShellCopy,
}

/** Language names are never translated: a picker shows each language natively. */
export const SHELL_LOCALE_LABELS: Record<ShellLocale, string> = endonyms as Record<
  ShellLocale,
  string
>

/** Short codes for the collapsed header pill. */
export const SHELL_LOCALE_SHORT: Record<ShellLocale, string> = {
  en: 'EN',
  'zh-CN': 'ZH',
  'zh-TW': 'TW',
  fr: 'FR',
  ru: 'RU',
  ja: 'JA',
  vi: 'VI',
}

export function isShellLocale(value: string): value is ShellLocale {
  return (SHELL_LOCALES as readonly string[]).includes(value)
}

/** Falls back to English so an unknown build-time locale never renders keys. */
export function getShellCopy(locale: string): ShellCopy {
  return isShellLocale(locale) ? COPY_BY_LOCALE[locale] : COPY_BY_LOCALE.en
}

/** Minimal `{name}` interpolation (no ICU: the shell templates are trivial). */
export function formatShellCopy(
  template: string,
  vars: Record<string, string | number>,
): string {
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in vars ? String(vars[name]) : match,
  )
}
