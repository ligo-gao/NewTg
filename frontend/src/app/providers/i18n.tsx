import { createContext, useContext, useEffect, useMemo, type ReactNode } from 'react'

import common from '@/locales/en/common.json'
import publicMessages from '@/locales/en/public.json'

/**
 * i18n runtime skeleton (web-00 §7).
 *
 * Dictionaries live at `locales/<lang>/<module>.json`; the key inside a file is
 * `{feature}.{element}` and the runtime restores the module namespace, so the
 * public key is always `{module}.{feature}.{element}`. M0 ships `en` only —
 * the remaining canonical languages land with M6, together with the parity
 * check that compares their key sets against this baseline.
 */

export const SUPPORTED_LOCALES = ['en'] as const
export type Locale = (typeof SUPPORTED_LOCALES)[number]
export const DEFAULT_LOCALE: Locale = 'en'

export type TranslateValues = Record<string, string | number>
export type TranslateFn = (key: string, values?: TranslateValues) => string

function flatten(
  prefix: string,
  messages: Record<string, unknown>,
  out: Record<string, string>,
): void {
  for (const [key, value] of Object.entries(messages)) {
    const fullKey = `${prefix}.${key}`
    if (typeof value === 'string') {
      out[fullKey] = value
    } else if (typeof value === 'object' && value !== null) {
      flatten(fullKey, value as Record<string, unknown>, out)
    }
  }
}

function loadEnglish(): Record<string, string> {
  const messages: Record<string, string> = {}
  flatten('common', common as Record<string, unknown>, messages)
  flatten('public', publicMessages as Record<string, unknown>, messages)
  return messages
}

const MESSAGES: Record<Locale, Record<string, string>> = {
  en: loadEnglish(),
}

const MISSING_KEY_WARNING =
  typeof process !== 'undefined' && process.env.NODE_ENV !== 'production'

function interpolate(template: string, values?: TranslateValues): string {
  if (values === undefined) return template
  return template.replace(/\{([a-zA-Z0-9_]+)\}/g, (match, name: string) => {
    const value = values[name]
    return value === undefined ? match : String(value)
  })
}

/** Pure lookup used by the provider; exported for tests and non-React callers. */
export function translate(
  locale: Locale,
  key: string,
  values?: TranslateValues,
): string {
  const template = MESSAGES[locale][key]
  if (template === undefined) {
    if (MISSING_KEY_WARNING && typeof console !== 'undefined') {
      console.warn(`[i18n] missing key: ${key} (${locale})`)
    }
    // Fall back to the key itself rather than rendering a blank string (§7.2).
    return key
  }
  return interpolate(template, values)
}

export interface I18nContextValue {
  locale: Locale
  t: TranslateFn
}

const I18nContext = createContext<I18nContextValue | null>(null)

export interface I18nProviderProps {
  children: ReactNode
  locale?: Locale
}

export function I18nProvider({ children, locale = DEFAULT_LOCALE }: I18nProviderProps) {
  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  const value = useMemo<I18nContextValue>(
    () => ({
      locale,
      t: (key, values) => translate(locale, key, values),
    }),
    [locale],
  )

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>
}

export function useI18n(): I18nContextValue {
  const context = useContext(I18nContext)
  if (context === null) {
    throw new Error('useI18n must be used within an I18nProvider')
  }
  return context
}
