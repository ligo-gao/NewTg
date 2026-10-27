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
 * number used by both the admin discount editor and the usage-log drawer.
 * `0.9 → "9"`, `0.85 → "8.5"`, `0.01 → "0.1"`.
 */
export function discountFold(rate: number): string {
  if (!Number.isFinite(rate)) return '—'
  // `toFixed` also strips binary-float noise (`0.9 * 10` is 9.000000000000002).
  return String(Number((rate * 10).toFixed(2)))
}
