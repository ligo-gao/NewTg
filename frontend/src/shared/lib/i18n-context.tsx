import { createContext, useContext, type ReactNode } from 'react'

/**
 * i18n context shared between the runtime (`app/providers/i18n.tsx`) and its
 * consumers in `shared/ui`. It lives in `shared/lib` rather than `app/` because
 * web-00 §2.2 only allows `shared/` to import intra-shared and third-party
 * modules — importing the provider module from a shared component would invert
 * the layering (the same reason `toast.tsx` avoids importing `api/`).
 *
 * `Locale` is the compile-time union of the canonical seven languages
 * (web-00 §7.3, G7-R14). The runtime list of *advertised* locales still comes
 * from `GET /api/status`; this union only fixes the set the bundle ships.
 */

export type Locale = 'en' | 'zh-CN' | 'zh-TW' | 'fr' | 'ru' | 'ja' | 'vi'
export type TranslateValues = Record<string, string | number>
export type TranslateFn = (key: string, values?: TranslateValues) => string

export interface I18nContextValue {
  locale: Locale
  t: TranslateFn
  /**
   * Locales the server advertises (`GET /api/status.languages`) intersected
   * with the dictionaries bundled here. Optional so lightweight test doubles
   * that only exercise `t` keep compiling; real consumers get the runtime list.
   */
  availableLocales?: readonly Locale[]
  /** Switches the active locale immediately (web-00 §7.3). */
  setLocale?: (locale: Locale) => void
}

const I18nContext = createContext<I18nContextValue | null>(null)

export interface I18nContextProviderProps {
  value: I18nContextValue
  children: ReactNode
}

/**
 * Publishes the runtime value. Only `app/providers/i18n.tsx` should render
 * this; keeping the context object private prevents accidental second
 * providers from silently splitting the tree.
 */
export function I18nContextProvider({
  value,
  children,
}: I18nContextProviderProps) {
  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>
}

export function useI18n(): I18nContextValue {
  const context = useContext(I18nContext)
  if (context === null) {
    throw new Error('useI18n must be used within an I18nProvider')
  }
  return context
}
