import { useEffect, useMemo, useState, type ReactNode } from 'react'

import {
  I18nContextProvider,
  type I18nContextValue,
  type Locale,
  type TranslateValues,
} from '@/shared/lib/i18n-context'

// The context/hook live in shared/lib so shared components (e.g. the toast
// viewport) can consume them without importing app/ (web-00 §2.2). Re-exported
// here to keep the existing `app/providers/i18n` import path stable.
export { useI18n } from '@/shared/lib/i18n-context'
export type {
  I18nContextValue,
  Locale,
  TranslateFn,
  TranslateValues,
} from '@/shared/lib/i18n-context'

/**
 * i18n runtime skeleton (web-00 §7).
 *
 * Dictionaries live at `locales/<lang>/<module>.json`; the key inside a file is
 * `{feature}.{element}` and the runtime restores the module namespace, so the
 * public key is always `{module}.{feature}.{element}`. M0 ships `en` only —
 * the remaining canonical languages land with M6, together with the parity
 * check that compares their key sets against this baseline.
 *
 * Loading is per domain, not per app: web-00 §7.2 requires a dynamic `import()`
 * of the domain dictionary on route/domain entry so the public first paint
 * (JS ≤150KB, §3.4) never pulls console dictionaries. `common` is the one
 * always-loaded baseline (app-wide toast/error copy); every other domain is
 * fetched only when a caller asks for it. The loader map below is therefore the
 * source of truth for both the available locales and the available domains;
 * the bundler emits one async chunk per entry.
 */

/** Shape of a JSON module after a dynamic `import()`. */
type JsonDictionary = { default: Record<string, unknown> }

/**
 * `locale → domain → loader`. Keep every loader lazy: a static import here
 * would merge the dictionary back into the entry chunk and defeat §7.2.
 *
 * `SUPPORTED_LOCALES` is derived from this map instead of a bare hardcoded
 * list. M6 replaces that derivation with the canonical locale list delivered
 * by `GET /api/status` (§7.3 — the backend owns the advertised set); at that
 * point this map gains one entry per advertised locale, not a second list.
 */
const DICTIONARY_LOADERS = {
  en: {
    common: () => import('@/locales/en/common.json'),
    public: () => import('@/locales/en/public.json'),
    auth: () => import('@/locales/en/auth.json'),
    account: () => import('@/locales/en/account.json'),
    admin: () => import('@/locales/en/admin.json'),
    logs: () => import('@/locales/en/logs.json'),
  },
} satisfies Record<Locale, Record<string, () => Promise<JsonDictionary>>>

/** Domain names are the `<module>` segment of `{module}.{feature}.{element}`. */
export type DictionaryModule = keyof (typeof DICTIONARY_LOADERS)[Locale]

export const SUPPORTED_LOCALES = Object.keys(DICTIONARY_LOADERS) as Locale[]
export const DEFAULT_LOCALE: Locale = 'en'

const MISSING_KEY_WARNING = import.meta.env.DEV

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

type FlatDictionary = Record<string, string>

/**
 * Flattened dictionaries, filled only by `loadDictionary`. Splitting the cache
 * by locale *and* domain is what makes the lazy load observable: `translate`
 * can tell "not imported yet" from "key genuinely missing" and stay silent in
 * the first case instead of warning on every pre-load render.
 */
const dictionaries: Partial<
  Record<Locale, Partial<Record<DictionaryModule, FlatDictionary>>>
> = {}

/** In-flight imports, so concurrent callers share a single chunk request. */
const pendingLoads = new Map<string, Promise<FlatDictionary>>()

function isDictionaryLoaded(
  locale: Locale,
  module: DictionaryModule,
): boolean {
  return dictionaries[locale]?.[module] !== undefined
}

/**
 * Loads, flattens and caches one domain dictionary (web-00 §7.2). The provider
 * calls this for its `modules`; route/feature code calls it for the domain it
 * is entering. Repeat calls are served from the in-memory cache.
 */
export async function loadDictionary(
  locale: Locale,
  module: DictionaryModule,
): Promise<FlatDictionary> {
  const cached = dictionaries[locale]?.[module]
  if (cached !== undefined) return cached

  const cacheKey = `${locale}:${module}`
  const inflight = pendingLoads.get(cacheKey)
  if (inflight !== undefined) return inflight

  const promise = DICTIONARY_LOADERS[locale][module]()
    .then((json) => {
      const flat: FlatDictionary = {}
      flatten(module, json.default, flat)
      const localeDictionaries = (dictionaries[locale] ??= {})
      localeDictionaries[module] = flat
      return flat
    })
    .finally(() => {
      // A rejected import must not poison the cache forever: the next caller
      // (e.g. the next provider mount) retries the chunk.
      pendingLoads.delete(cacheKey)
    })

  pendingLoads.set(cacheKey, promise)
  return promise
}

function interpolate(template: string, values?: TranslateValues): string {
  if (values === undefined) return template
  return template.replace(/\{([a-zA-Z0-9_]+)\}/g, (match, name: string) => {
    const value = values[name]
    return value === undefined ? match : String(value)
  })
}

function moduleOf(key: string): DictionaryModule {
  // Keys are `{module}.{feature}.{element}`; a key without a dot cannot name a
  // domain, so the whole string is returned and fails the "known module" check
  // in `translate`, surfacing through the missing-key warning.
  const separator = key.indexOf('.')
  return (separator === -1 ? key : key.slice(0, separator)) as DictionaryModule
}

/** Pure lookup used by the provider; exported for tests and non-React callers. */
export function translate(
  locale: Locale,
  key: string,
  values?: TranslateValues,
): string {
  const module = moduleOf(key)
  const dictionary = dictionaries[locale]?.[module]

  // §7.2 fallback chain: current locale → `en` → the key name itself.
  let template = dictionary?.[key]
  if (template === undefined && locale !== DEFAULT_LOCALE) {
    template = dictionaries[DEFAULT_LOCALE]?.[module]?.[key]
  }

  if (template === undefined) {
    const moduleIsKnown = Object.prototype.hasOwnProperty.call(
      DICTIONARY_LOADERS[locale],
      module,
    )
    // A known domain that has not been imported yet is *pending*, not missing:
    // the provider bumps its revision when the chunk lands and re-renders.
    // Only a loaded domain with an absent key (or a typo'd domain segment) is a
    // genuine miss worth warning about (dev only, §7.2).
    if (MISSING_KEY_WARNING && (dictionary !== undefined || !moduleIsKnown)) {
      console.warn(`[i18n] missing key: ${key} (${locale})`)
    }
    // Fall back to the key itself rather than rendering a blank string (§7.2).
    return key
  }
  return interpolate(template, values)
}

/**
 * App-wide strings (toast, errors) live in `common`, so it is the always-loaded
 * baseline. Console domains stay on-demand and must never enter the public
 * first-paint path (web-00 §7.2, §3.4).
 */
const DEFAULT_MODULES: readonly DictionaryModule[] = ['common']

export interface I18nProviderProps {
  children: ReactNode
  locale?: Locale
  /**
   * Domains to fetch for this subtree. `common` is the always-loaded baseline;
   * route code that needs another dictionary passes its own stable list or
   * calls `loadDictionary`, so the public page does not download console
   * domains it never renders (web-00 §7.2). This is a per-subtree baseline, not
   * a promise that only `public` loads on the public site.
   */
  modules?: readonly DictionaryModule[]
}

export function I18nProvider({
  children,
  locale = DEFAULT_LOCALE,
  modules = DEFAULT_MODULES,
}: I18nProviderProps) {
  // Cache revision: dictionaries are module-level mutable state, so the
  // provider needs an explicit signal to re-render consumers once a lazy chunk
  // resolves. Without it, `t()` would keep returning key names until some
  // unrelated state change happened.
  const [readyRevision, setReadyRevision] = useState(0)

  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  useEffect(() => {
    let cancelled = false
    const pendingModules = modules.filter(
      (module) => !isDictionaryLoaded(locale, module),
    )
    if (pendingModules.length === 0) return

    void Promise.all(
      pendingModules.map((module) => loadDictionary(locale, module)),
    )
      .then(() => {
        if (!cancelled) setReadyRevision((revision) => revision + 1)
      })
      .catch((error: unknown) => {
        // A failed chunk must not blank the app: `translate` keeps returning
        // key names (visible, debuggable) and the next mount retries.
        console.error('[i18n] failed to load dictionary', error)
      })

    return () => {
      cancelled = true
    }
  }, [locale, modules])

  const value = useMemo<I18nContextValue>(
    () => ({
      locale,
      t: (key, values) => {
        // Read the revision so the closure is recreated after a lazy load even
        // though `translate` itself only touches the module-level cache.
        void readyRevision
        return translate(locale, key, values)
      },
    }),
    [locale, readyRevision],
  )

  return <I18nContextProvider value={value}>{children}</I18nContextProvider>
}
