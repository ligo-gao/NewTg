/**
 * Number formatting helpers implementing the NEW TG numeric discipline
 * (modules-00 §4.7):
 *
 * - credits: always integers, thousands separators, no decimals;
 * - token amounts: abbreviate at 1,000,000 with one decimal (e.g. `1.2M`);
 * - currency approximations: always two decimals and always prefixed with `≈`.
 *
 * `font-variant-numeric: tabular-nums` is a presentation concern and is applied
 * at the call site. Callers add the Unicode minus sign (U+2212) for expenses;
 * these helpers reject negative values so the responsibility stays explicit.
 */

const DEFAULT_LOCALE = 'en'

function assertFinite(value: number, fn: string): void {
  if (!Number.isFinite(value)) {
    throw new TypeError(`${fn} expects a finite number, received ${String(value)}`)
  }
}

/** Formats a credit amount: integer, grouped, never negative and never fractional. */
export function formatCredits(value: number, locale: string = DEFAULT_LOCALE): string {
  assertFinite(value, 'formatCredits')
  if (!Number.isInteger(value)) {
    throw new RangeError('formatCredits only accepts integer credit amounts')
  }
  if (value < 0) {
    throw new RangeError(
      'formatCredits does not accept negative values; prefix U+2212 at the call site',
    )
  }
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(value)
}

/** Formats a token amount, abbreviating values of at least 1,000,000 with one decimal. */
export function formatTokenAmount(value: number, locale: string = DEFAULT_LOCALE): string {
  assertFinite(value, 'formatTokenAmount')
  if (value < 0) {
    throw new RangeError('formatTokenAmount does not accept negative values')
  }
  // Decide the format on the same integer the formatter would print. Checking
  // the raw value first made 999,999.6 fall through to the grouped branch,
  // whose maximumFractionDigits: 0 then rendered "1,000,000" — a value that
  // reads as at least a million but is not abbreviated. Rounding first keeps
  // the threshold and the rendered number consistent (modules-00 §4.7).
  const rounded = Math.round(value)
  if (rounded < 1_000_000) {
    return new Intl.NumberFormat(locale, {
      minimumFractionDigits: 0,
      maximumFractionDigits: 0,
    }).format(rounded)
  }
  const millions = rounded / 1_000_000
  const formatted = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(millions)
  return `${formatted}M`
}

/** Formats a currency approximation: two decimals, always prefixed with `≈`. */
export function formatCurrencyApprox(
  value: number,
  locale: string = DEFAULT_LOCALE,
): string {
  assertFinite(value, 'formatCurrencyApprox')
  const formatted = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value)
  return `≈${formatted}`
}
