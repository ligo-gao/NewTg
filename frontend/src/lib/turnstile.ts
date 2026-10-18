/**
 * Cloudflare Turnstile loader (modules-01 §3.15).
 *
 * The widget script is only injected when the deployment enables Turnstile, so
 * a disabled site never downloads a third-party script or leaks a request to
 * challenges.cloudflare.com. `render=explicit` keeps us in control of mount
 * and reset, which matters because every token is single-use.
 */

export const TURNSTILE_SCRIPT_SRC =
  'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'

export interface TurnstileRenderOptions {
  sitekey: string
  callback: (token: string) => void
  'error-callback'?: (code?: string) => void
  'expired-callback'?: () => void
  theme?: 'auto' | 'light' | 'dark'
  action?: string
}

export interface TurnstileApi {
  render: (container: HTMLElement, options: TurnstileRenderOptions) => string
  reset: (widgetId?: string) => void
  remove: (widgetId?: string) => void
}

declare global {
  interface Window {
    turnstile?: TurnstileApi
  }
}

let scriptPromise: Promise<TurnstileApi> | undefined

/**
 * Returns the Turnstile API, injecting the script once on first use. Tests
 * pre-install `window.turnstile`; the early return also means a previously
 * cached promise never shadows a freshly installed mock.
 */
export function loadTurnstile(): Promise<TurnstileApi> {
  if (typeof window === 'undefined') {
    return Promise.reject(new Error('turnstile requires a browser'))
  }
  if (window.turnstile !== undefined) return Promise.resolve(window.turnstile)

  scriptPromise ??= new Promise<TurnstileApi>((resolve, reject) => {
    const script = document.createElement('script')
    script.src = TURNSTILE_SCRIPT_SRC
    script.async = true
    script.onload = () => {
      if (window.turnstile !== undefined) resolve(window.turnstile)
      else reject(new Error('turnstile api missing after load'))
    }
    script.onerror = () => {
      reject(new Error('turnstile script failed to load'))
    }
    document.head.appendChild(script)
  }).catch((error: unknown) => {
    // A failed load must be retryable on the next mount.
    scriptPromise = undefined
    throw error
  })

  return scriptPromise
}

/** Test-only: clears the cached script promise. */
export function resetTurnstileLoader(): void {
  scriptPromise = undefined
}
