/**
 * Formats an ISO timestamp in the viewer's local timezone (billing-08 §16.3
 * "时间 ISO + 本地时区"). The API only ever carries ISO instants; the UI never
 * reinterprets them in UTC, which would shift day boundaries for most users.
 * An unparsable value is echoed verbatim rather than replaced with "Invalid
 * Date", so a backend regression stays visible instead of silently blanking.
 */
export function formatSubscriptionTime(value: string, locale: string): string {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return value
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(timestamp)
}
