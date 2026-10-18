/**
 * Post-login redirect sanitizer (modules-01 F-01-03).
 *
 * `?redirect=` is attacker-controlled (it arrives in a link), so only
 * same-origin absolute paths are accepted. Anything that a browser could read
 * as another origin — `https://…`, protocol-relative `//…`, or the backslash
 * trick `/\…` — falls back to the console dashboard.
 */

export const DEFAULT_AUTH_REDIRECT = '/dashboard'

export function sanitizeAuthRedirect(raw: string | null | undefined): string {
  if (raw === null || raw === undefined || raw === '') {
    return DEFAULT_AUTH_REDIRECT
  }
  if (!raw.startsWith('/')) return DEFAULT_AUTH_REDIRECT
  // `//host` is protocol-relative and `/\host` is normalised to `//host` by
  // browsers, so both must be rejected rather than treated as a path.
  if (raw.startsWith('//') || raw.startsWith('/\\')) {
    return DEFAULT_AUTH_REDIRECT
  }
  return raw
}
