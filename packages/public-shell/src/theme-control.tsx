import type { ShellCopy } from './copy'
import { ThemeGlyph } from './icons'
import type { ShellThemeControl } from './types'

export interface ShellThemeControlProps {
  control: ShellThemeControl
  copy: ShellCopy
}

/**
 * One theme control at every width. The icon shows the *target* theme: a moon
 * while light, a sun while dark, and the accessible name says the same.
 */
export function ShellThemeButton({ control, copy }: ShellThemeControlProps) {
  const target = control.value === 'light' ? 'dark' : 'light'
  return (
    <button
      type="button"
      className="ps-theme-btn"
      aria-label={target === 'dark' ? copy['nav.theme.dark'] : copy['nav.theme.light']}
      onClick={control.onToggle}
    >
      <ThemeGlyph target={target} />
    </button>
  )
}
