import { useEffect, useId, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'

import type { ShellCopy } from './copy'
import { CheckGlyph, GlobeGlyph } from './icons'
import type { ShellLocaleControl } from './types'

export interface ShellLanguageControlProps {
  control: ShellLocaleControl
  copy: ShellCopy
  /**
   * `pill` — header: a globe button that opens the locale menu.
   * `footer` — footer column: the plain native select.
   */
  variant?: 'pill' | 'footer'
}

/**
 * Header language picker. A globe-only button keeps the tool cluster compact;
 * the menu is a `menu`/`menuitemradio` pair so the current locale is announced
 * as checked, arrow keys walk the list, Escape returns focus to the globe and
 * a click outside dismisses it. The shell owns the markup; each app keeps its
 * own persistence/navigation in `onChange`.
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
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)

  // Opened from the keyboard or a pointer, the menu takes focus so the next
  // Tab leaves the picker instead of walking hidden items.
  useEffect(() => {
    if (!open) return
    const checked = menuRef.current?.querySelector<HTMLElement>(
      '[role="menuitemradio"][aria-checked="true"]',
    )
    checked?.focus()
  }, [open])

  useEffect(() => {
    if (!open) return
    function onPointerDown(event: MouseEvent) {
      if (wrapRef.current?.contains(event.target as Node)) return
      setOpen(false)
    }
    function onEscape(event: globalThis.KeyboardEvent) {
      if (event.key !== 'Escape') return
      setOpen(false)
      triggerRef.current?.focus()
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onEscape)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onEscape)
    }
  }, [open])

  function change(next: string) {
    if (!options.some((option) => option.value === next)) return
    setOpen(false)
    control.onChange(next)
  }

  function onMenuKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return
    event.preventDefault()
    const items = Array.from(
      menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitemradio"]') ?? [],
    )
    if (items.length === 0) return
    const index = items.indexOf(document.activeElement as HTMLElement)
    const step = event.key === 'ArrowDown' ? 1 : -1
    items[(index + step + items.length) % items.length]?.focus()
  }

  if (variant === 'footer') {
    return (
      <div className="ps-lang ps-lang-foot">
        <select
          aria-label={label}
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

  return (
    <div ref={wrapRef} className="ps-lang">
      <button
        ref={triggerRef}
        type="button"
        aria-label={current ? `${label}: ${current.label}` : label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen((value) => !value)}
        className="ps-lang-btn"
      >
        <GlobeGlyph />
      </button>
      {open ? (
        <div
          ref={menuRef}
          id={id}
          role="menu"
          aria-label={label}
          className="ps-lang-menu"
          onKeyDown={onMenuKeyDown}
        >
          {options.map((option) => {
            const checked = option.value === control.value
            return (
              <button
                key={option.value}
                type="button"
                role="menuitemradio"
                aria-checked={checked}
                tabIndex={-1}
                onClick={() => change(option.value)}
                className="ps-lang-item"
              >
                <span>{option.label}</span>
                <CheckGlyph />
              </button>
            )
          })}
        </div>
      ) : null}
    </div>
  )
}
