/**
 * Subscription-management display helpers (billing-08 §16.2/§16.4).
 *
 * `price_amount` is stored in the smallest currency unit (integer cents, the
 * same convention as orders), so presentation is the only place that divides
 * by 100. `grant_credits` and subscription quota are integers; 0 has a domain
 * meaning (unlimited), so every caller must branch before formatting.
 */

const YUAN_SYMBOL = '¥'

/** Formats integer cents as CNY, e.g. `¥1,000.00`. Currency is frozen to CNY. */
export function formatPlanPrice(cents: number, locale: string): string {
  const formatted = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(cents / 100)
  return `${YUAN_SYMBOL}${formatted}`
}

/**
 * ISO instant → viewer-local date/time. The API only carries ISO instants, so
 * the UI never reinterprets them in UTC (that would shift day boundaries).
 * Unparsable values are echoed verbatim rather than replaced with
 * "Invalid Date", keeping a backend regression visible.
 */
export function formatDateTime(value: string, locale: string): string {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return value
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(timestamp))
}

/** Integer cents ↔ the yuan string shown in the drawer input. */
export function centsToYuanInput(cents: number): string {
  return (cents / 100).toFixed(2)
}

/**
 * Parses a yuan input into integer cents. Returns null unless the text is a
 * non-negative amount with at most two decimals — silently rounding a
 * hand-entered price would misprice every future purchase.
 */
export function parseYuanToCents(value: string): number | null {
  const trimmed = value.trim()
  if (trimmed === '') return null
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) return null
  const yuan = Number(trimmed)
  if (!Number.isFinite(yuan) || yuan < 0) return null
  // Cents travel as int64; beyond 2^53 the multiplication has already lost
  // precision, so reject instead of sending a silently wrong price.
  const cents = Math.round(yuan * 100)
  return Number.isSafeInteger(cents) ? cents : null
}
