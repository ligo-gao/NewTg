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

/**
 * Canonical paths of the two legal documents.
 *
 * The files are the single source for the copy; the payment compliance gate
 * only links to them. There is no runtime `site.legal.*` configuration in the
 * backend/public status projection yet, so these same-site routes are the
 * bundled default. When such configuration lands, resolve it into the gate's
 * `termsHref`/`privacyHref` props and keep the `isSafePublicHref` guard.
 */
export const LEGAL_DOC_LINKS = {
  terms: '/legal/terms',
  privacy: '/legal/privacy',
} as const

