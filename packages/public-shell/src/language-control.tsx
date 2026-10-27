import { useId } from 'react'

import type { ShellCopy } from './copy'
import { GlobeGlyph } from './icons'
import type { ShellLocaleControl } from './types'

export interface ShellLanguageControlProps {
  control: ShellLocaleControl
  copy: ShellCopy
  /**
   * `pill` — header, expanded row (globe + native select).
   * `compact` — header, collapsed row (visible short code, transparent select).
   * `footer` — footer column (plain select; the column heading names it).
   */
  variant?: 'pill' | 'compact' | 'footer'
}

/**
 * Native `<select>` on purpose: it is keyboard and screen-reader accessible on
 * every platform, never traps focus, and costs no JS. The shell owns the
 * markup; each app keeps its own persistence/navigation in `onChange`.
 */
export function ShellLanguageControl({
  control,
  copy,
  variant = 'pill',
}: ShellLanguageControlProps) {
  const id = useId()
  const label = copy['language.label']
  const options = control.options
  const current = options.find((option) => option.value === control.value)

  function change(next: string) {
    if (!options.some((option) => option.value === next)) return
    control.onChange(next)
  }

  if (variant === 'compact') {
    return (
      <label className="ps-lang-compact">
        <span aria-hidden="true">{current?.short ?? control.value.toUpperCase()}</span>
        <select
          aria-label={label}
          value={control.value}
          onChange={(event) => change(event.target.value)}
        >
          {options.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </label>
    )
  }

  return (
    <div className="ps-lang">
      {variant === 'pill' ? (
        <>
          <label htmlFor={id} className="ps-sr-only">
            {label}
          </label>
          <GlobeGlyph />
        </>
      ) : null}
      <select
        id={variant === 'pill' ? id : undefined}
        aria-label={variant === 'footer' ? label : undefined}
        className="ps-lang-select"
        value={control.value}
        onChange={(event) => change(event.target.value)}
      >
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </div>
  )
}
