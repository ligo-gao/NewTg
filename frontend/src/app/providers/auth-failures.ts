import { ApiError } from '@/api/errors'

/**
 * Global handling for the two credential-plane failures web-00 §9.6/§9.4 freeze
 * as navigation events rather than page-local errors:
 *
 * - a runtime 401 outside the `['me']` session probe means the session expired
 *   or was revoked → invalidate `['me']` and land on `/401` with a return path;
 * - a `403 password_change_required` means the session is restricted to the
 *   change-password flow (temporary password) → hard navigation to
 *   `/security?force=password`.
 *
 * The navigation functions are injectable so the router can use SPA navigation
 * in production while tests can observe the calls without patching jsdom's
 * unforgeable `location`. Defaults use a document navigation because the
 * restricted-session case must survive a deep link / reload.
 */
export interface AuthFailureHandlers {
  /** Called once per detected session expiry; `path` is the source location. */
  unauthorized: (path: string) => void
  /** A restricted session tried to reach a blocked endpoint. */
  passwordChangeRequired: () => void
}

function currentPath(): string {
  if (typeof location === 'undefined') return '/'
  return `${location.pathname}${location.search}${location.hash}`
}

const defaultHandlers: AuthFailureHandlers = {
  unauthorized: (path) => {
    window.location.assign(`/401?redirect=${encodeURIComponent(path)}`)
  },
  passwordChangeRequired: () => {
    // Hard navigation on purpose: a restricted session must not keep any
    // console state alive, and the reload re-runs route consumers against the
    // server-enforced whitelist.
    window.location.assign('/security?force=password')
  },
}

let handlers: AuthFailureHandlers = { ...defaultHandlers }

/**
 * Many queries can fail together when a session dies (the dashboard fires
 * several). Navigation is idempotent in effect, but a burst of identical
 * redirects is wasteful and can race two router transitions; collapse them.
 */
let lastUnauthorizedAt = Number.NEGATIVE_INFINITY
const UNAUTHORIZED_DEBOUNCE_MS = 2000

export function setAuthFailureHandlers(
  next: Partial<AuthFailureHandlers> | undefined,
): void {
  handlers = { ...defaultHandlers, ...next }
}

/** Test-only reset: restores defaults and drops the debounce window. */
export function resetAuthFailureHandlers(): void {
  handlers = { ...defaultHandlers }
  lastUnauthorizedAt = Number.NEGATIVE_INFINITY
}

export function isUnauthorizedError(error: unknown): error is ApiError {
  return error instanceof ApiError && error.kind === 'auth'
}

export function isPasswordChangeRequired(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    error.status === 403 &&
    error.type.endsWith('password_change_required')
  )
}

/**
 * Query/mutation error hook. `queryKey` distinguishes the public session
 * probe: a 401 from `GET /api/me` is the *guest* signal (web-00 §9.2) and must
 * never bounce a visitor off a public page.
 */
export function reportServerError(error: unknown, queryKey: readonly unknown[]): void {
  if (isPasswordChangeRequired(error)) {
    handlers.passwordChangeRequired()
    return
  }
  if (!isUnauthorizedError(error)) return
  if (queryKey[0] === 'me') return
  if (currentPath().startsWith('/401')) return

  const now = Date.now()
  if (now - lastUnauthorizedAt < UNAUTHORIZED_DEBOUNCE_MS) return
  lastUnauthorizedAt = now
  handlers.unauthorized(currentPath())
}
