import { formatCredits, formatTokenAmount } from '@/shared/lib/format'

/**
 * Numeric presentation for the overview dashboard (modules-00 §4.7).
 *
 * Every formatter is total: non-finite input yields `--` instead of `NaN`.
 * That matters more here than on a single-value page because the same helpers
 * receive four different endpoints' payloads, and one missing field must not
 * blank a card. `0` remains a real value — only unavailable data is `--`.
 */
export const UNAVAILABLE = '--'

export function formatInteger(value: number, locale: string): string {
  if (!Number.isFinite(value)) return UNAVAILABLE
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(Math.round(value))
}

export function formatCreditsValue(value: number, locale: string): string {
  if (!Number.isFinite(value) || value < 0) return UNAVAILABLE
  return formatCredits(Math.round(value), locale)
}

/**
 * Converts integer credits to the operator's display currency
 * (`billing.display_unit=cny`). The server's convention is
 * `credits = cents * quota_per_unit / 100` (same as
 * `features/account/topup/money.ts`), so the display amount is
 * `credits / quota_per_unit`, formatted with exactly two decimals and the
 * configured symbol. A missing/zero quota cannot be converted and renders
 * `--`, matching the other dashboard helpers.
 */
export function formatCurrencyValue(
  credits: number,
  quotaPerUnit: number,
  symbol: string,
  locale: string,
): string {
  if (!Number.isFinite(credits) || credits < 0) return UNAVAILABLE
  if (!Number.isFinite(quotaPerUnit) || quotaPerUnit <= 0) return UNAVAILABLE
  const amount = credits / quotaPerUnit
  return `${symbol}${new Intl.NumberFormat(locale, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(amount)}`
}

export function formatTokensValue(value: number, locale: string): string {
  if (!Number.isFinite(value) || value < 0) return UNAVAILABLE
  return formatTokenAmount(value, locale)
}

/**
 * `success_rate` is a 0…1 fraction (the backend defines it as success/total).
 * With zero rows it is defined as 0, but there is no rate to show — the card
 * renders `--` rather than a misleading "0.0%".
 */
export function formatSuccessRate(value: number, requestCount: number): string {
  if (!Number.isFinite(value) || requestCount <= 0) return UNAVAILABLE
  return `${(value * 100).toFixed(1)}%`
}

/** RPM is displayed as a whole number; the exact average is not meaningful to readers. */
export function formatRpm(value: number, locale: string): string {
  if (!Number.isFinite(value) || value < 0) return UNAVAILABLE
  return formatInteger(value, locale)
}

/**
 * TPM is abbreviated (`86K`, `1.2M`) as specified for the rates strip, while
 * the exact grouped number stays available in the hover tooltip.
 */
export function formatTpm(value: number, locale: string): string {
  if (!Number.isFinite(value) || value < 0) return UNAVAILABLE
  // Decide the branch on the same rounded number that will be printed: a raw
  // 999.6 otherwise renders "1,000" without the K abbreviation.
  const rounded = Math.round(value)
  if (rounded >= 1_000_000) {
    return `${new Intl.NumberFormat(locale, {
      minimumFractionDigits: 1,
      maximumFractionDigits: 1,
    }).format(rounded / 1_000_000)}M`
  }
  if (rounded >= 1000) {
    const thousands = rounded / 1000
    const fractionDigits = thousands >= 10 ? 0 : 1
    return `${new Intl.NumberFormat(locale, {
      minimumFractionDigits: fractionDigits,
      maximumFractionDigits: fractionDigits,
    }).format(thousands)}K`
  }
  return formatInteger(rounded, locale)
}
