import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it } from 'vitest'

import {
  handleRadioGroupKeyDown,
  handleTabListKeyDown,
} from './roving-focus'

/**
 * UI-5: shared WAI-ARIA keyboard handlers. Radio groups must move focus and
 * selection with the arrow keys; tab lists must support arrows plus Home/End.
 */

afterEach(cleanup)

function RadioHarness() {
  const [value, setValue] = useState('a')
  return (
    <div role="radiogroup" aria-label="group" onKeyDown={handleRadioGroupKeyDown}>
      {['a', 'b', 'c'].map((key) => (
        <button
          key={key}
          type="button"
          role="radio"
          aria-checked={value === key}
          tabIndex={value === key ? 0 : -1}
          onClick={() => setValue(key)}
        >
          {key}
        </button>
      ))}
    </div>
  )
}

function TabHarness() {
  const [tab, setTab] = useState('one')
  return (
    <div role="tablist" aria-label="tabs" onKeyDown={handleTabListKeyDown}>
      <button
        type="button"
        role="tab"
        aria-selected={tab === 'one'}
        tabIndex={tab === 'one' ? 0 : -1}
        onClick={() => setTab('one')}
      >
        one
      </button>
      <button
        type="button"
        role="tab"
        aria-selected={tab === 'two'}
        tabIndex={tab === 'two' ? 0 : -1}
        onClick={() => setTab('two')}
      >
        two
      </button>
    </div>
  )
}

describe('roving focus keyboard handlers (UI-5)', () => {
  it('moves and selects radios with arrows, wrapping at both ends', () => {
    render(<RadioHarness />)
    const a = screen.getByRole('radio', { name: 'a' })
    const b = screen.getByRole('radio', { name: 'b' })
    const c = screen.getByRole('radio', { name: 'c' })

    a.focus()
    fireEvent.keyDown(a, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(b)
    expect(b.getAttribute('aria-checked')).toBe('true')

    fireEvent.keyDown(b, { key: 'ArrowDown' })
    expect(document.activeElement).toBe(c)

    fireEvent.keyDown(c, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(a)
    expect(a.getAttribute('aria-checked')).toBe('true')

    fireEvent.keyDown(a, { key: 'ArrowLeft' })
    expect(document.activeElement).toBe(c)

    fireEvent.keyDown(c, { key: 'ArrowUp' })
    expect(document.activeElement).toBe(b)
  })

  it('moves and selects tabs with arrows and supports Home/End', () => {
    render(<TabHarness />)
    const one = screen.getByRole('tab', { name: 'one' })
    const two = screen.getByRole('tab', { name: 'two' })

    one.focus()
    fireEvent.keyDown(one, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(two)
    expect(two.getAttribute('aria-selected')).toBe('true')

    fireEvent.keyDown(two, { key: 'Home' })
    expect(document.activeElement).toBe(one)

    fireEvent.keyDown(one, { key: 'End' })
    expect(document.activeElement).toBe(two)

    fireEvent.keyDown(two, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(one)
  })
})
