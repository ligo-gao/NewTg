/**
 * Request-ID shape gate. The gateway mints opaque 32-hex IDs with no RFC
 * version/variant bits (`relay/context.go`), and they are stored through
 * `$1::uuid`; users paste them either compact or hyphenated. Both forms are
 * therefore accepted, with arbitrary hex nibbles — rejecting non-v4 IDs here
 * would mark roughly 92% of real request IDs as invalid and break deep links.
 * Comparison uses `normalizeRequestId` (hyphen-insensitive, lowercase).
 */
const COMPACT_REQUEST_ID = /^[0-9a-f]{32}$/i
const HYPHENATED_REQUEST_ID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function isUuid(value: string): boolean {
  return COMPACT_REQUEST_ID.test(value) || HYPHENATED_REQUEST_ID.test(value)
}

/** Canonical comparison key: lowercase without hyphens. */
export function normalizeRequestId(value: string): string {
  return value.replace(/-/g, '').toLowerCase()
}
