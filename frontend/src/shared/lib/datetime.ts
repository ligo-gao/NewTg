/**
 * Locale-aware relative time for session rows (modules-01 F-01-13).
 * `Intl.RelativeTimeFormat` keeps the output translated (the UI never
 * hardcodes "3 minutes ago") while the absolute timestamp remains available
 * via the caller's `title` attribute.
 */
export function formatRelativeTime(
  value: string,
  locale: string,
  now: number = Date.now(),
): string {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return value

  const diffSeconds = Math.round((timestamp - now) / 1000)
  const abs = Math.abs(diffSeconds)
  const formatter = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' })

  if (abs < 60) return formatter.format(diffSeconds, 'second')
  if (abs < 3600) return formatter.format(Math.round(diffSeconds / 60), 'minute')
  if (abs < 86_400) return formatter.format(Math.round(diffSeconds / 3600), 'hour')
  return formatter.format(Math.round(diffSeconds / 86_400), 'day')
}
