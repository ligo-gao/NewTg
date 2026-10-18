/**
 * Legal document publish date.
 *
 * The value is intentionally kept in one module instead of inline in
 * `LegalPage`: the build pipeline does not inject a publish date yet, so this
 * is the single replacement point (either a build-time env var or a config
 * field) when that wiring lands. Until then, updating the legal copy means
 * updating this constant with it.
 */
export const LEGAL_LAST_UPDATED = '2026-10-18'
