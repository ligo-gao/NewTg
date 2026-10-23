import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'

/**
 * Console theme runtime (plan §2.4).
 *
 * The marketing site already persists its light/dark choice under the same
 * key, so a visitor who switched the public site keeps that choice in the
 * console. `system` is stored explicitly: it is the absence of an override,
 * not a third colour, and the pre-paint script in `index.html` treats any
 * value other than `light`/`dark` as "follow the OS".
 */
export const THEME_STORAGE_KEY = 'newtg-theme'

export type ThemeMode = 'light' | 'dark' | 'system'
export type ResolvedTheme = 'light' | 'dark'

export interface ThemeContextValue {
  mode: ThemeMode
  resolved: ResolvedTheme
  setMode: (mode: ThemeMode) => void
}

function isThemeMode(value: string | null | undefined): value is ThemeMode {
  return value === 'light' || value === 'dark' || value === 'system'
}

function readStoredMode(): ThemeMode {
  try {
    const stored = globalThis.localStorage?.getItem(THEME_STORAGE_KEY)
    return isThemeMode(stored) ? stored : 'system'
  } catch {
    // Private windows and disabled storage must not break the first render.
    return 'system'
  }
}

function systemPrefersDark(): boolean {
  if (
    typeof window === 'undefined' ||
    typeof window.matchMedia !== 'function'
  ) {
    return false
  }
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

/** Pure resolver, exported for tests and non-React callers. */
export function resolveThemeMode(
  mode: ThemeMode,
  prefersDark: boolean,
): ResolvedTheme {
  if (mode === 'system') return prefersDark ? 'dark' : 'light'
  return mode
}

const SAFE_DEFAULT: ThemeContextValue = {
  mode: 'system',
  resolved: 'light',
  setMode: () => {},
}

const ThemeContext = createContext<ThemeContextValue | null>(null)

/**
 * Resolves and applies the theme. Everything is driven by `data-theme` on the
 * document root (`styles/tokens.css` re-resolves every token there), so no
 * component needs a `dark:` variant.
 */
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(readStoredMode)
  const [prefersDark, setPrefersDark] = useState<boolean>(systemPrefersDark)

  const resolved = resolveThemeMode(mode, prefersDark)

  // Only `system` follows the OS. Explicit light/dark must ignore later OS
  // changes until the visitor switches back.
  useEffect(() => {
    if (mode !== 'system') return
    if (
      typeof window === 'undefined' ||
      typeof window.matchMedia !== 'function'
    ) {
      return
    }
    const list = window.matchMedia('(prefers-color-scheme: dark)')
    const update = (): void => setPrefersDark(list.matches)
    update()
    list.addEventListener('change', update)
    return () => list.removeEventListener('change', update)
  }, [mode])

  useEffect(() => {
    if (typeof document === 'undefined') return
    const root = document.documentElement
    root.dataset.theme = resolved
    // Keeps form controls and scrollbars in step with the token palette.
    root.style.colorScheme = resolved
  }, [resolved])

  const setMode = useCallback((next: ThemeMode) => {
    setModeState(next)
    try {
      globalThis.localStorage?.setItem(THEME_STORAGE_KEY, next)
    } catch {
      // The in-memory choice still applies for this session.
    }
  }, [])

  const value = useMemo<ThemeContextValue>(
    () => ({ mode, resolved, setMode }),
    [mode, resolved, setMode],
  )

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}

/**
 * Theme state. Rendering outside a provider (unit tests, isolated widgets)
 * yields a read-only default instead of throwing; only `AppProviders` owns the
 * real provider.
 */
export function useTheme(): ThemeContextValue {
  return useContext(ThemeContext) ?? SAFE_DEFAULT
}
