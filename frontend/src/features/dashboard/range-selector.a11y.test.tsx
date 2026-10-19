import { fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'

import type { UsageRange } from './api'
import { RangeSelector } from './range-selector'

import common from '@/locales/en/common.json'
import { I18nContextProvider } from '@/shared/lib/i18n-context'

const MESSAGES: Record<string, string> = {
  'common.dashboard.range.label': common.dashboard.range.label,
  'common.dashboard.range.24h': common.dashboard.range['24h'],
  'common.dashboard.range.7d': common.dashboard.range['7d'],
  'common.dashboard.range.14d': common.dashboard.range['14d'],
  'common.dashboard.range.30d': common.dashboard.range['30d'],
}

function Harness() {
  const [range, setRange] = useState<UsageRange>('24h')
  return <RangeSelector value={range} onChange={setRange} />
}

function renderSelector() {
  render(
    <I18nContextProvider
      value={{
        locale: 'en',
        t: (key) => MESSAGES[key] ?? key,
      }}
    >
      <Harness />
    </I18nContextProvider>,
  )
}

describe('dashboard range selector accessibility (UI-5)', () => {
  it('uses a radiogroup with one tab stop', () => {
    renderSelector()
    expect(screen.getByRole('radiogroup', { name: 'Time range' })).toBeTruthy()
    expect(screen.getByRole('radio', { name: '24h' }).tabIndex).toBe(0)
    expect(screen.getByRole('radio', { name: '7d' }).tabIndex).toBe(-1)
  })

  it('moves focus and selection with the arrow keys', () => {
    renderSelector()
    const first = screen.getByRole('radio', { name: '24h' })
    first.focus()

    fireEvent.keyDown(first, { key: 'ArrowRight' })
    const second = screen.getByRole('radio', { name: '7d' })
    expect(document.activeElement).toBe(second)
    expect(second.getAttribute('aria-checked')).toBe('true')
    expect(second.tabIndex).toBe(0)

    fireEvent.keyDown(second, { key: 'ArrowLeft' })
    expect(document.activeElement).toBe(first)
    expect(first.getAttribute('aria-checked')).toBe('true')
  })
})
