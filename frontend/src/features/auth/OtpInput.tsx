import { useRef, type ClipboardEvent, type KeyboardEvent } from 'react'

export interface OtpInputProps {
  value: string
  onChange: (value: string) => void
  length?: number
  disabled?: boolean
  /** Group label, e.g. "6-digit code"; each box appends its 1-based index. */
  ariaLabel: string
}

/**
 * Six single-character inputs (modules-01 F-01-08): auto-advance, backspace
 * retreat and whole-code paste. Filling the last box never auto-submits, so a
 * mis-tap cannot burn one of the five challenge attempts.
 */
export function OtpInput({
  value,
  onChange,
  length = 6,
  disabled = false,
  ariaLabel,
}: OtpInputProps) {
  const inputs = useRef<Array<HTMLInputElement | null>>([])

  function focusAt(index: number): void {
    const clamped = Math.max(0, Math.min(index, length - 1))
    inputs.current[clamped]?.focus()
  }

  function applyDigits(startIndex: number, digits: string): void {
    if (digits === '') {
      // Clearing a box removes that position and keeps the rest aligned.
      const next = value.slice(0, startIndex) + value.slice(startIndex + 1)
      onChange(next.slice(0, length))
      focusAt(startIndex)
      return
    }
    const next = (value.slice(0, startIndex) + digits).slice(0, length)
    onChange(next)
    focusAt(startIndex + digits.length)
  }

  function handleKeyDown(
    index: number,
    event: KeyboardEvent<HTMLInputElement>,
  ): void {
    if (event.key === 'Backspace' && (value[index] ?? '') === '' && index > 0) {
      event.preventDefault()
      onChange(value.slice(0, index - 1))
      focusAt(index - 1)
      return
    }
    if (event.key === 'ArrowLeft') {
      event.preventDefault()
      focusAt(index - 1)
    } else if (event.key === 'ArrowRight') {
      event.preventDefault()
      focusAt(index + 1)
    }
  }

  function handlePaste(event: ClipboardEvent<HTMLInputElement>): void {
    const digits = event.clipboardData.getData('text').replace(/\D/g, '')
    if (digits === '') return
    event.preventDefault()
    applyDigits(0, digits.slice(0, length))
  }

  return (
    <div role="group" aria-label={ariaLabel} className="flex gap-2">
      {Array.from({ length }, (_, index) => (
        <input
          key={index}
          ref={(element) => {
            inputs.current[index] = element
          }}
          value={value[index] ?? ''}
          onChange={(event) => {
            applyDigits(index, event.target.value.replace(/\D/g, ''))
          }}
          onKeyDown={(event) => {
            handleKeyDown(index, event)
          }}
          onPaste={handlePaste}
          aria-label={`${ariaLabel} ${index + 1}`}
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={1}
          disabled={disabled}
          autoFocus={index === 0}
          data-otp-input=""
          className="h-12 w-10 rounded-md border border-border-default bg-surface-card text-center font-mono text-page-title text-ink-1 focus-visible:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
        />
      ))}
    </div>
  )
}
