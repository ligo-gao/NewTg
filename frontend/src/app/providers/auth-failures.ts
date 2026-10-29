import { ApiError } from '@/api/errors'

/**
 * Global handling for the credential-plane failures web-00 §9.6/§9.4 freeze as
 * navigation events rather than page-local errors:
 *
 * - a runtime 401 outside the `['me']` session probe means the session expired
 *   or was revoked → invalidate `['me']` and land on `/401` with a return path;
 * - a `403 password_change_required` means the session is restricted to the
 *   change-password flow (temporary password) → hard navigation to
 *   `/security?force=password`.
 * - a `403 mfa_enrollment_required` means an already-signed-in administrator
 *   is not TOTP-enrolled (R-04) → discard the cached identity and send them to
 *   sign-in with `?reason=mfa_enrollment_required`, where they re-authenticate
 *   and receive an enrollment ticket.
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
  /** An unbound administrator session tried to reach a blocked endpoint. */
  mfaEnrollmentRequired: () => void
}

/**
 * Landing page for the forced MFA-enrollment re-login. The marker is what makes
 * the sign-in page explain why the user was thrown out of the console.
 */
export const MFA_ENROLLMENT_REQUIRED_REDIRECT =
  '/sign-in?reason=mfa_enrollment_required'

function currentPath(): string {
  if (typeof location === 'undefined') return '/'
  return `${location.pathname}${location.search}${location.hash}`
}

/**
 * True when the browser is already on the restricted-session landing page.
 *
 * A restricted session may only call `/api/me`, `/api/self`, `/api/logout`,
 * `/api/password` and the 2FA endpoints; every other query answers
 * `403 password_change_required`. The shell still mounts widgets that fetch
 * blocked endpoints (notifications, announcements), so without this guard the
 * handler hard-navigates back to the same URL on every mount — an endless
 * reload loop the user cannot escape to change their password.
 */
export function isForcedPasswordChangePath(
  pathname: string,
  search: string,
): boolean {
  return (
    pathname === '/security' &&
    new URLSearchParams(search).get('force') === 'password'
  )
}

/**
 * True when the browser is already on a page that belongs to the forced
 * MFA-enrollment flow. Both pages keep issuing their own requests
 * (`/api/status`, `/api/me` and the ticket-scoped enrollment calls); a 403 from
 * one of their widgets must not bounce them onto themselves forever.
 */
export function isMfaEnrollmentLandingPath(pathname: string): boolean {
  return pathname === '/sign-in' || pathname === '/two-factor-enroll'
}

const defaultHandlers: AuthFailureHandlers = {
  unauthorized: (path) => {
    window.location.assign(`/401?redirect=${encodeURIComponent(path)}`)
  },
  passwordChangeRequired: () => {
    // A restricted session on the change-password page must stay put: the
    // only useful action is submitting the form, not another full reload.
    if (
      typeof location !== 'undefined' &&
      isForcedPasswordChangePath(location.pathname, location.search)
    ) {
      return
    }
    // Hard navigation on purpose: a restricted session must not keep any
    // console state alive, and the reload re-runs route consumers against the
    // server-enforced whitelist.
    window.location.assign('/security?force=password')
  },
  mfaEnrollmentRequired: () => {
    // The sign-in page is the destination; re-entering it would loop.
    if (
      typeof location !== 'undefined' &&
      isMfaEnrollmentLandingPath(location.pathname)
    ) {
      return
    }
    // Hard navigation: the cached console state belongs to an unusable
    // session, and the re-login must start from a clean document.
    window.location.assign(MFA_ENROLLMENT_REQUIRED_REDIRECT)
  },
}

let handlers: AuthFailureHandlers = { ...defaultHandlers }

/**
 * One-shot hand-off for the `request_id` of a runtime 401.
 *
 * The failing query is what knows the Problem's `request_id`, but the
 * navigation happens through a handler that only receives the path. Writing
 * the id into the URL is forbidden (web-00 §8.4: it leaks into history,
 * referrers and logs), so it travels in `sessionStorage` under a fixed key,
 * where the `/401` page consumes and immediately removes it. Storage can be
 * blocked (private mode); both sides fail soft and simply render no id.
 */
export const UNAUTHORIZED_REQUEST_ID_KEY = 'newtg.unauthorized.request_id'

export function stashUnauthorizedRequestId(requestId: string | undefined): void {
  if (requestId === undefined || requestId === '') return
  try {
    globalThis.sessionStorage?.setItem(UNAUTHORIZED_REQUEST_ID_KEY, requestId)
  } catch {
    // No storage: the error page renders without an id, never with a fake one.
  }
}

/** Reads and clears the stashed id so a reload cannot replay a stale one. */
export function takeUnauthorizedRequestId(): string | undefined {
  try {
    const value = globalThis.sessionStorage?.getItem(UNAUTHORIZED_REQUEST_ID_KEY)
    if (value === null || value === undefined || value === '') return undefined
    globalThis.sessionStorage?.removeItem(UNAUTHORIZED_REQUEST_ID_KEY)
    return value
  } catch {
    return undefined
  }
}

/**
 * Many queries can fail together when a session dies (the dashboard fires
 * several). Navigation is idempotent in effect, but a burst of identical
 * redirects is wasteful and can race two router transitions; collapse them.
 */
let lastUnauthorizedAt = Number.NEGATIVE_INFINITY
const UNAUTHORIZED_DEBOUNCE_MS = 2000

/**
 * One-shot latch for the forced MFA-enrollment landing. A console page mounts
 * several queries at once and an unbound administrator makes every one of them
 * answer `403 mfa_enrollment_required`; only the first may navigate.
 */
let mfaEnrollmentRedirectIssued = false

export function setAuthFailureHandlers(
  next: Partial<AuthFailureHandlers> | undefined,
): void {
  handlers = { ...defaultHandlers, ...next }
}

/** Test-only reset: restores defaults and drops the debounce window. */
export function resetAuthFailureHandlers(): void {
  handlers = { ...defaultHandlers }
  lastUnauthorizedAt = Number.NEGATIVE_INFINITY
  mfaEnrollmentRedirectIssued = false
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
 * The backend's single refusal for an administrator session that is not
 * TOTP-enrolled (R-04). Accept the bare token, a URI suffix, or a namespaced
 * form so a future problem-type prefix cannot silently disable the landing.
 */
export function isMfaEnrollmentRequired(error: unknown): error is ApiError {
  if (!(error instanceof ApiError) || error.status !== 403) return false
  return (
    error.type === 'mfa_enrollment_required' ||
    error.type.endsWith('/mfa_enrollment_required') ||
    error.type.endsWith(':mfa_enrollment_required')
  )
}

/**
 * Reports the forced MFA-enrollment landing at most once per document. The
 * sign-in and enrollment pages are excluded: they issue their own requests and
 * must never bounce onto themselves.
 */
export function reportMfaEnrollmentRequired(): void {
  if (mfaEnrollmentRedirectIssued) return
  if (
    typeof location !== 'undefined' &&
    isMfaEnrollmentLandingPath(location.pathname)
  ) {
    return
  }
  mfaEnrollmentRedirectIssued = true
  handlers.mfaEnrollmentRequired()
}

/**
 * Query/mutation error hook. `queryKey` distinguishes the public session
 * probe: a 401 from `GET /api/me` is the *guest* signal (web-00 §9.2) and must
 * never bounce a visitor off a public page.
 */
export function reportServerError(error: unknown, queryKey: readonly unknown[]): void {
  if (isMfaEnrollmentRequired(error)) {
    reportMfaEnrollmentRequired()
    return
  }
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
  // Stash only when this error actually wins the debounce and navigates:
  // a suppressed burst must not overwrite the id of the redirect in flight.
  stashUnauthorizedRequestId(error.request_id)
  handlers.unauthorized(currentPath())
}
