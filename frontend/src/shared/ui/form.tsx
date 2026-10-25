import {
  useId,
  useState,
  type InputHTMLAttributes,
  type ReactNode,
} from 'react'

/**
 * Shared form primitives (modules-01 §0.6). They own the label/field/error
 * wiring (`htmlFor`, `aria-describedby`, `aria-invalid`) so every M1 form has
 * the same accessible structure; copy is always supplied by the caller via
 * i18n keys — no user-facing string lives here.
 */

export interface TextFieldProps
  extends Omit<InputHTMLAttributes<HTMLInputElement>, 'id'> {
  label: string
  error?: string
  hint?: string
  id?: string
  /** Inline control rendered opposite the label, e.g. a recovery link. */
  labelAction?: ReactNode
  /**
   * `line` is the auth hall's underline field. It only changes the skin; the
   * label/error wiring below is identical for both variants.
   */
  variant?: 'default' | 'line'
}

const INPUT_CLASSES =
  'h-10 w-full rounded-lg border bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

export function TextField({
  label,
  error,
  hint,
  id,
  labelAction,
  variant = 'default',
  className,
  ...props
}: TextFieldProps) {
  const generatedId = useId()
  const fieldId = id ?? generatedId
  const errorId = `${fieldId}-error`
  const hintId = `${fieldId}-hint`
  // Reference the hint only while it is rendered: when an error replaces the
  // hint its paragraph disappears, so a dangling id would describe nothing.
  const describedBy =
    error !== undefined ? errorId : hint !== undefined ? hintId : undefined

  return (
    <div className="flex flex-col gap-1.5" data-variant={variant}>
      <div className="flex items-baseline justify-between gap-3">
        <label htmlFor={fieldId} className="text-body font-medium text-ink-1">
          {label}
        </label>
        {labelAction}
      </div>
      <input
        id={fieldId}
        aria-invalid={error !== undefined ? true : undefined}
        aria-describedby={describedBy}
        className={[
          INPUT_CLASSES,
          error !== undefined ? 'border-danger' : 'border-border-default',
          className,
        ]
          .filter(Boolean)
          .join(' ')}
        {...props}
      />
      {hint !== undefined && error === undefined ? (
        <p id={hintId} className="text-meta text-ink-3">
          {hint}
        </p>
      ) : null}
      {error !== undefined ? (
        <p id={errorId} className="text-meta text-danger">
          {error}
        </p>
      ) : null}
    </div>
  )
}

export interface PasswordFieldProps extends Omit<TextFieldProps, 'type'> {
  /** Translated "Show password" label (icon-only button). */
  showLabel: string
  /** Translated "Hide password" label (icon-only button). */
  hideLabel: string
}

export function PasswordField({
  label,
  error,
  hint,
  id,
  labelAction,
  variant = 'default',
  showLabel,
  hideLabel,
  ...props
}: PasswordFieldProps) {
  const generatedId = useId()
  const fieldId = id ?? generatedId
  const errorId = `${fieldId}-error`
  const hintId = `${fieldId}-hint`
  const [visible, setVisible] = useState(false)
  const describedBy =
    error !== undefined ? errorId : hint !== undefined ? hintId : undefined

  return (
    <div className="flex flex-col gap-1.5" data-variant={variant}>
      <div className="flex items-baseline justify-between gap-3">
        <label htmlFor={fieldId} className="text-body font-medium text-ink-1">
          {label}
        </label>
        {labelAction}
      </div>
      <div className="relative">
        <input
          id={fieldId}
          type={visible ? 'text' : 'password'}
          aria-invalid={error !== undefined ? true : undefined}
          aria-describedby={describedBy}
          className={[
            INPUT_CLASSES,
            'pr-11',
            error !== undefined ? 'border-danger' : 'border-border-default',
          ].join(' ')}
          {...props}
        />
        <button
          type="button"
          aria-label={visible ? hideLabel : showLabel}
          aria-pressed={visible}
          data-password-toggle=""
          className="absolute inset-y-0 right-0 flex w-10 items-center justify-center rounded-r-lg text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          onClick={() => {
            setVisible((current) => !current)
          }}
        >
          <svg
            aria-hidden="true"
            viewBox="0 0 20 20"
            className="size-4"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
          >
            {visible ? (
              <path d="M3 3l14 14M8.5 8.6A2.5 2.5 0 0011.4 11M5.2 5.4C3.8 6.5 2.8 8.2 2 10c1.6 3.5 4.6 5.5 8 5.5 1.5 0 2.9-.4 4.1-1.1M8 4.7A7.9 7.9 0 0110 4.5c3.4 0 6.4 2 8 5.5-.5 1.1-1.2 2.1-2 3" />
            ) : (
              <>
                <path d="M2 10c1.6-3.5 4.6-5.5 8-5.5s6.4 2 8 5.5c-1.6 3.5-4.6 5.5-8 5.5S3.6 13.5 2 10z" />
                <circle cx="10" cy="10" r="2.5" />
              </>
            )}
          </svg>
        </button>
      </div>
      {hint !== undefined && error === undefined ? (
        <p id={hintId} className="text-meta text-ink-3">
          {hint}
        </p>
      ) : null}
      {error !== undefined ? (
        <p id={errorId} className="text-meta text-danger">
          {error}
        </p>
      ) : null}
    </div>
  )
}

export type AlertTone = 'danger' | 'warning' | 'success' | 'info'

const ALERT_CLASSES: Record<AlertTone, string> = {
  danger: 'border-danger-line bg-danger-bg text-danger',
  warning: 'border-warning-line bg-warning-bg text-warning',
  success: 'border-success-line bg-success-bg text-success',
  info: 'border-info-line bg-info-bg text-info',
}

export interface FormAlertProps {
  tone?: AlertTone
  children: ReactNode
}

/** Form-level alert. `role="alert"` is reserved for these (assertive). */
export function FormAlert({ tone = 'danger', children }: FormAlertProps) {
  return (
    <div
      role="alert"
      className={[
        'rounded-lg border px-3 py-2 text-body leading-5',
        ALERT_CLASSES[tone],
      ].join(' ')}
    >
      {children}
    </div>
  )
}
