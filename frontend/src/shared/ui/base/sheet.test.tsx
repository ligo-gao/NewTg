import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Sheet } from './sheet'

/**
 * UI-5 review follow-up: the Sheet's Tab-trap candidates must use the same
 * filter as the Dialog. jsdom reports no layout, so give every element a box
 * and assert that non-tabbable (`tabindex="-1"`) and `aria-hidden` controls are
 * skipped when the trap picks the first/last stop.
 */

beforeEach(() => {
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockImplementation(
    () => ({ length: 1 }) as unknown as DOMRectList,
  )
})

afterEach(cleanup)

describe('Sheet focus trap candidate filtering (UI-5)', () => {
  it('skips non-tabbable and aria-hidden controls', () => {
    render(
      <Sheet open onOpenChange={() => {}} title="Filters">
        <button type="button" tabIndex={-1}>
          Programmatic
        </button>
        <button type="button" aria-hidden="true">
          Decorative
        </button>
        <button type="button">Visible</button>
      </Sheet>,
    )

    const visible = screen.getByRole('button', { name: 'Visible' })

    // From the panel, Tab lands on the first *eligible* control.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(visible)

    // Only one candidate: forward and backward both wrap onto it.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(visible)
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(visible)
  })
})
