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
