import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import {
  THEME_STORAGE_KEY,
  ThemeProvider,
  useTheme,
} from './theme'

const originalMatchMedia = window.matchMedia

interface MediaListener {
  (event: MediaQueryListEvent): void
}

let prefersDark = false
let listeners: MediaListener[] = []

function installMatchMedia(initialPrefersDark: boolean): void {
  prefersDark = initialPrefersDark
  listeners = []
  window.matchMedia = ((query: string) => ({
    // A getter, like a real MediaQueryList: the provider's change handler reads
    // `matches` after the event fires, not the value captured at creation.
    get matches() {
      return prefersDark
    },
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: (_type: string, listener: MediaListener) => {
      listeners.push(listener)
    },
    removeEventListener: (_type: string, listener: MediaListener) => {
      listeners = listeners.filter((current) => current !== listener)
    },
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
}

function changeSystemPreference(next: boolean): void {
  prefersDark = next
  const event = { matches: next } as MediaQueryListEvent
  for (const listener of listeners) listener(event)
}

function ThemeProbe() {
  const { mode, resolved, setMode } = useTheme()
  return (
    <div>
      <output data-testid="mode">{mode}</output>
      <output data-testid="resolved">{resolved}</output>
      <button type="button" onClick={() => setMode('light')}>
        light
      </button>
      <button type="button" onClick={() => setMode('dark')}>
        dark
      </button>
      <button type="button" onClick={() => setMode('system')}>
        system
      </button>
    </div>
  )
}

function renderThemed() {
  return render(
    <ThemeProvider>
      <ThemeProbe />
    </ThemeProvider>,
  )
}

beforeEach(() => {
  window.localStorage.clear()
  delete document.documentElement.dataset.theme
  installMatchMedia(false)
})

afterEach(() => {
  window.matchMedia = originalMatchMedia
  window.localStorage.clear()
  delete document.documentElement.dataset.theme
})

describe('ThemeProvider', () => {
  it('follows the system preference when no explicit mode is stored', () => {
    installMatchMedia(true)

    renderThemed()

    expect(screen.getByTestId('mode').textContent).toBe('system')
    expect(screen.getByTestId('resolved').textContent).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('resolves to light when the system prefers light', () => {
    renderThemed()

    expect(screen.getByTestId('resolved').textContent).toBe('light')
    expect(document.documentElement.dataset.theme).toBe('light')
  })

  it('restores an explicitly stored mode over the system preference', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'dark')
    installMatchMedia(false)

    renderThemed()

    expect(screen.getByTestId('mode').textContent).toBe('dark')
    expect(screen.getByTestId('resolved').textContent).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('applies and persists an explicit light or dark choice', async () => {
    installMatchMedia(true)
    const user = userEvent.setup()

    renderThemed()

    await user.click(screen.getByRole('button', { name: 'light' }))
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
    expect(document.documentElement.dataset.theme).toBe('light')

    await user.click(screen.getByRole('button', { name: 'dark' }))
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('persists system and resumes following media-query changes', async () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'light')
    installMatchMedia(false)
    const user = userEvent.setup()

    renderThemed()

    await user.click(screen.getByRole('button', { name: 'system' }))
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('system')

    act(() => changeSystemPreference(true))
    expect(screen.getByTestId('resolved').textContent).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')

    act(() => changeSystemPreference(false))
    expect(document.documentElement.dataset.theme).toBe('light')
  })

  it('ignores system changes while an explicit mode is set', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'light')
    installMatchMedia(false)

    renderThemed()
    act(() => changeSystemPreference(true))

    expect(screen.getByTestId('resolved').textContent).toBe('light')
    expect(document.documentElement.dataset.theme).toBe('light')
  })

  it('exposes a safe read-only default outside a provider', () => {
    render(<ThemeProbe />)

    expect(screen.getByTestId('mode').textContent).toBe('system')
    expect(screen.getByTestId('resolved').textContent).toBe('light')
  })
})
