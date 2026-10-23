/**
 * Money formatting for module 04.
 *
 * The API carries `amount` in the smallest currency unit (integer cents, P-1),
 * so all arithmetic stays in integers and only the presentation layer divides
 * by 100. Credits are integers too: `quota_per_unit` (default 100) credits per
 * yuan, i.e. credits = cents * quota_per_unit / 100.
 */

const YUAN_SYMBOL = '¥'

/** Formats integer cents as yuan, e.g. `¥1,000.00` (or `¥1,000` for chips). */
export function formatYuan(
  cents: number,
  options: { decimals?: boolean; locale?: string } = {},
): string {
  const decimals = options.decimals ?? true
  const formatted = new Intl.NumberFormat(options.locale ?? 'en', {
    minimumFractionDigits: decimals ? 2 : 0,
    maximumFractionDigits: decimals ? 2 : 0,
  }).format(cents / 100)
  return `${YUAN_SYMBOL}${formatted}`
}

/**
 * Converts a top-up amount (cents) to the credits the server will credit:
 * `floor(cents × quota_per_unit / 100 × credits_ratio)`.
 *
 * The group ratio can be below 1 (F-04-01), so assuming 1.0 would overstate
 * the preview. It is still only a display helper: the server reprices every
 * order and its value always wins.
 */
export function centsToCredits(
  cents: number,
  quotaPerUnit: number,
  creditsRatio = 1,
): number {
  return Math.floor((cents * quotaPerUnit * creditsRatio) / 100)
}

/** Parses a positive integer yuan input into cents; non-numeric input yields null. */
export function parseYuanToCents(value: string): number | null {
  const trimmed = value.trim()
  if (!/^\d+$/.test(trimmed)) return null
  const yuan = Number(trimmed)
  if (!Number.isSafeInteger(yuan) || yuan <= 0) return null
  return yuan * 100
}

/** Formats remaining seconds as `m:ss` for the Alipay countdown. */
export function formatCountdown(totalSeconds: number): string {
  const safe = Math.max(0, Math.floor(totalSeconds))
  const minutes = Math.floor(safe / 60)
  const seconds = safe % 60
  return `${minutes}:${String(seconds).padStart(2, '0')}`
}
