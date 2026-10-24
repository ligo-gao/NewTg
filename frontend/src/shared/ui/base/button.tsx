import type { ButtonHTMLAttributes } from 'react'

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
}

const BASE_CLASSES =
  'inline-flex h-9 items-center justify-center gap-1.5 rounded-lg border border-transparent px-3.5 text-body font-medium transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

const VARIANT_CLASSES: Record<ButtonVariant, string> = {
  primary: 'bg-brand text-on-brand hover:bg-brand-hover active:bg-brand-active',
  secondary:
    'border-border-default bg-surface-card text-ink-1 hover:bg-surface-hover',
  ghost: 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
  danger: 'bg-danger text-on-brand hover:opacity-90',
}

/**
 * Minimal Base UI Button primitive (web-00 §2.1). M0 only needs a compilable,
 * token-driven shell; variants and states expand in later milestones.
 */
export function Button({
  variant = 'primary',
  className,
  type = 'button',
  ...props
}: ButtonProps) {
  const classes = [BASE_CLASSES, VARIANT_CLASSES[variant], className]
    .filter(Boolean)
    .join(' ')

  return <button type={type} data-variant={variant} className={classes} {...props} />
}
