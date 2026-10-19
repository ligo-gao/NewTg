import type { ReactNode } from 'react'

/**
 * Classification tags (modules-00 §4.6.4) — semantically disjoint from
 * `StatusBadge`: a Tag classifies a thing (order type, adjustment method,
 * settings key, group, chart legend) and never expresses state. It has a 6px
 * radius, a soft fill with a matching border, and **no dot**.
 *
 * Exporting a `StatusBadge`-shaped API here is intentionally avoided so callers
 * cannot pass a state word to a Tag by accident.
 */
export type TagVariant =
  | 'neutral'
  | 'brand'
  | 'success'
  | 'warning'
  | 'danger'
  | 'info'
  | 'admin'
  | 'reversal'

export interface TagProps {
  variant?: TagVariant
  children: ReactNode
  className?: string
  /**
   * 可访问名称。分类标签在表格里常与状态徽章、数字相邻，读屏需要一个
   * 明确名称；可见文本不变。
   */
  ariaLabel?: string
}

const BASE_CLASSES =
  'inline-flex h-5 items-center rounded-[6px] px-2 text-xs font-medium leading-[18px] whitespace-nowrap'

const VARIANT_CLASSES: Record<TagVariant, string> = {
  neutral: 'border border-neutral-line bg-neutral-bg text-neutral',
  brand: 'border border-brand-line bg-brand-subtle text-brand',
  success: 'border border-success-line bg-success-bg text-success',
  warning: 'border border-warning-line bg-warning-bg text-warning',
  danger: 'border border-danger-line bg-danger-bg text-danger',
  info: 'border border-info-line bg-info-bg text-info',
  admin: 'border border-accent-admin bg-surface-subtle text-accent-admin',
  // Reversal is a warning outline rather than an error (§4.6.3).
  reversal: 'border border-dashed border-accent-reversal bg-transparent text-accent-reversal',
}

export function Tag({ variant = 'neutral', children, className, ariaLabel }: TagProps) {
  const classes = [BASE_CLASSES, VARIANT_CLASSES[variant], className]
    .filter(Boolean)
    .join(' ')

  return (
    <span
      data-testid="tag"
      data-variant={variant}
      aria-label={ariaLabel}
      className={classes}
    >
      {children}
    </span>
  )
}
