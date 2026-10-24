import { useId, useState, type ReactNode } from 'react'

import { UNAVAILABLE } from './format'

export interface StatCardProps {
  /** Test id base; the value/subline/retry elements derive from it. */
  testId: string
  label: string
  value: string
  unit?: string
  subline?: ReactNode
  /** Hover/focus copy (pre-deducted/refunded, peak, exact TPM…). */
  tooltip?: string
  loading?: boolean
  error?: boolean
  onRetry?: () => void
  retryLabel: string
}

/**
 * One metric card shared by the overview cards and the RPM/TPM strip
 * (modules-05 §3.4, §4.4). Numeral values are rendered once, final: there is
 * deliberately no count-up animation (ledger discipline, §4.4).
 *
 * The tooltip opens on hover *and* focus so keyboard users can read the
 * pre-deducted/refund totals instead of losing them to a mouse-only title.
 */
export function StatCard({
  testId,
  label,
  value,
  unit,
  subline,
  tooltip,
  loading = false,
  error = false,
  onRetry,
  retryLabel,
}: StatCardProps) {
  const [tooltipOpen, setTooltipOpen] = useState(false)
  const tooltipId = useId()
  const showTooltip = tooltip !== undefined && !loading && !error

  return (
    <article
      data-testid={testId}
      aria-busy={loading || undefined}
      tabIndex={showTooltip ? 0 : undefined}
      aria-describedby={showTooltip && tooltipOpen ? tooltipId : undefined}
      onMouseEnter={() => setTooltipOpen(true)}
      onMouseLeave={() => setTooltipOpen(false)}
      onFocus={() => setTooltipOpen(true)}
      onBlur={() => setTooltipOpen(false)}
      className="relative min-h-24 rounded-xl border border-border-default bg-surface-card p-4 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
    >
      <p className="text-meta text-ink-2">{label}</p>

      {loading ? (
        <div
          data-testid={`${testId}-skeleton`}
          className="mt-2 h-5 w-[72px] animate-pulse rounded bg-surface-hover motion-reduce:animate-none"
        />
      ) : error ? (
        <div className="mt-1 flex items-center gap-2">
          <p data-testid={`${testId}-value`} className="text-stat font-semibold text-ink-3">
            {UNAVAILABLE}
          </p>
          {onRetry !== undefined ? (
            <button
              type="button"
              data-testid={`${testId}-retry`}
              onClick={onRetry}
              className="rounded border border-border-default px-2 py-0.5 text-badge text-ink-2 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {retryLabel}
            </button>
          ) : null}
        </div>
      ) : (
        <>
          <p
            data-testid={`${testId}-value`}
            className="mt-1 text-stat font-semibold leading-8 text-ink-1"
          >
            {value}
          </p>
          {unit !== undefined ? (
            <p className="text-meta text-ink-2">{unit}</p>
          ) : null}
          {subline !== undefined ? (
            <p data-testid={`${testId}-subline`} className="text-meta text-ink-2">
              {subline}
            </p>
          ) : null}
        </>
      )}

      {showTooltip && tooltipOpen ? (
        <div
          role="tooltip"
          id={tooltipId}
          className="absolute left-3 top-full z-10 mt-1 whitespace-nowrap rounded-lg bg-ink-1 px-2.5 py-1.5 text-meta text-surface-card"
        >
          {tooltip}
        </div>
      ) : null}
    </article>
  )
}
