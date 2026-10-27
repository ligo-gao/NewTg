/** Presentation helpers shared by the usage-log list and detail drawer. */

/** Milliseconds → one-decimal seconds (`1.2s`); missing values render an em dash. */
export function formatSeconds(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  return `${(value / 1000).toFixed(1)}s`
}

export function formatPercent(part: number, total: number): string {
  if (total <= 0) return '0.0'
  return ((part / total) * 100).toFixed(1)
}

export function formatUnitPrice(value: number): string {
  return new Intl.NumberFormat('en', {
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  }).format(value)
}

/**
 * Discount ratio (0..1, where 0 is free and 1 is full price) → the "折" fold
 * number used by the admin discount editor. `0.9 → "9"`, `0.85 → "8.5"`,
 * `0.01 → "0.1"`. The bill speaks in charged percentages instead; see
 * `describeDiscount`.
 */
export function discountFold(rate: number): string {
  if (!Number.isFinite(rate)) return '—'
  // `toFixed` also strips binary-float noise (`0.9 * 10` is 9.000000000000002).
  return String(Number((rate * 10).toFixed(2)))
}

/** How the bill renders the applied discount: nothing known, or a charged percentage. */
export type DiscountDisplay =
  | { kind: 'none' }
  | { kind: 'percent'; value: string }

/**
 * Applied discount ratio (0..1, where 0 is a full waiver and 1 is list price)
 * → the charged percentage shown on the bill (`0.9 → "90"`, `0.855 → "85.5"`).
 *
 * The product deliberately speaks in "charged percentage" only — never the
 * "折"/"off" forms — so the helper takes no locale. `null`/`undefined`/non-finite
 * mean the log carries no discount information, which is distinct from `0`
 * (an explicit full waiver). Out-of-range ratios are clamped rather than
 * trusted; the server writes 0..1 but a hand-edited row should not print 150%.
 */
export function describeDiscount(
  rate: number | null | undefined,
): DiscountDisplay {
  if (rate === null || rate === undefined || !Number.isFinite(rate)) {
    return { kind: 'none' }
  }
  const clamped = Math.min(1, Math.max(0, rate))
  // Round through thousandths so `0.9 * 1000` cannot leak a binary-float tail
  // (`0.9 * 100` is 90.00000000000001 in some engines).
  return { kind: 'percent', value: String(Math.round(clamped * 1000) / 10) }
}
