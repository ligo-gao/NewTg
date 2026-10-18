import { createContext, useContext, type ReactNode } from 'react'

/**
 * i18n context shared between the runtime (`app/providers/i18n.tsx`) and its
 * consumers in `shared/ui`. It lives in `shared/lib` rather than `app/` because
 * web-00 §2.2 only allows `shared/` to import intra-shared and third-party
 * modules — importing the provider module from a shared component would invert
 * the layering (the same reason `toast.tsx` avoids importing `api/`).
 *
 * `Locale` is the compile-time union. The runtime list of *advertised* locales
 * still comes from the dictionary loader map (later `GET /api/status`, §7.3);
 * M6 extends this union together with the locale files.
 */

export type Locale = 'en'
export type TranslateValues = Record<string, string | number>
export type TranslateFn = (key: string, values?: TranslateValues) => string

export interface I18nContextValue {
  locale: Locale
  t: TranslateFn
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
