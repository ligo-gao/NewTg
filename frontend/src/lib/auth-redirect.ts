/**
 * Post-login redirect sanitizer (modules-01 F-01-03).
 *
 * `?redirect=` is attacker-controlled (it arrives in a link), so only
 * same-origin absolute paths are accepted. The URL parser strips ASCII tab and
 * newline characters before parsing, which is why `/\t/evil.example` — a
 * decoded `%2F%09%2F…` — would otherwise be read as protocol-relative
 * `//evil.example`. Control characters are therefore rejected up front, and
 * the parsed URL is additionally required to stay on the same origin.
 */

export const DEFAULT_AUTH_REDIRECT = '/dashboard'

// C0 controls plus DEL. This covers tab/newline/CR, the characters the URL
// parser silently removes before resolving the path. Implemented as a loop
// rather than a regex because `no-control-regex` forbids the literal class.
function hasControlCharacter(value: string): boolean {
  for (const character of value) {
    const codePoint = character.codePointAt(0) ?? 0
    if (codePoint <= 0x1f || codePoint === 0x7f) return true
  }
  return false
}

function currentOrigin(): string {
  return typeof location !== 'undefined' ? location.origin : 'http://localhost'
}

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
  if (hasControlCharacter(raw)) return DEFAULT_AUTH_REDIRECT

  const origin = currentOrigin()
  try {
    const resolved = new URL(raw, origin)
    if (resolved.origin !== origin) return DEFAULT_AUTH_REDIRECT
    // Return the normalised same-origin relative form (never the raw input,
    // which may still carry parser-substituted characters).
    return `${resolved.pathname}${resolved.search}${resolved.hash}`
  } catch {
    return DEFAULT_AUTH_REDIRECT
  }
}
