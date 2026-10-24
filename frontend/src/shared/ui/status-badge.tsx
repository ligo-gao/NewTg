import type { ReactNode } from 'react'

/**
 * Status badges express *state* (modules-00 §4.6.4). They are always
 * dual-encoded: a 6px dot plus the status text, never colour alone.
 *
 * A disabled resource is rendered with `neutral`, never `danger`; `danger` is
 * reserved for errors and irreversible confirmations. The caller chooses the
 * variant, so this component only guarantees the shape and encoding.
 */
export type StatusBadgeVariant =
  | 'success'
  | 'warning'
  | 'danger'
  | 'info'
  | 'neutral'
  | 'neutral-outline'
  | 'danger-outline'
  | 'brand'

export interface StatusBadgeProps {
  variant?: StatusBadgeVariant
  children: ReactNode
  className?: string
}

const BASE_CLASSES =
  'inline-flex h-[22px] items-center gap-1.5 rounded-full px-2 text-badge whitespace-nowrap'

const VARIANT_CLASSES: Record<StatusBadgeVariant, string> = {
  success: 'border border-success-line bg-success-bg text-success',
  warning: 'border border-warning-line bg-warning-bg text-warning',
  danger: 'border border-danger-line bg-danger-bg text-danger',
  info: 'border border-info-line bg-info-bg text-info',
  neutral: 'border border-neutral-line bg-neutral-bg text-neutral',
  // Cancelled: dashed neutral outline, no fill.
  'neutral-outline': 'border border-dashed border-neutral-line bg-transparent text-neutral',
  // Refused: dashed danger outline, hollow (no fill).
  'danger-outline': 'border border-dashed border-danger-line bg-transparent text-danger',
  brand: 'border border-brand-line bg-brand-subtle text-brand',
}

export function StatusBadge({
  variant = 'neutral',
  children,
  className,
}: StatusBadgeProps) {
  const classes = [BASE_CLASSES, VARIANT_CLASSES[variant], className]
    .filter(Boolean)
    .join(' ')

  return (
    <span data-testid="status-badge" data-variant={variant} className={classes}>
      <span
        aria-hidden="true"
        data-testid="status-badge-dot"
        className="size-1.5 shrink-0 rounded-full bg-current"
      />
      <span>{children}</span>
    </span>
  )
}
