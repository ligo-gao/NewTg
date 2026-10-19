import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Dialog } from './dialog'

/**
 * UI-5 accessibility regression: the modal must expose `aria-modal`, be named
 * by its title, move focus inside on open, keep Tab from escaping and return
 * focus to the invoking control on close.
 */

beforeEach(() => {
  // jsdom performs no layout; the primitive filters non-rendered candidates
  // through `getClientRects()`, so give every element a box.
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockImplementation(
    () => ({ length: 1 }) as unknown as DOMRectList,
  )
})

afterEach(cleanup)

function Harness() {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open dialog
      </button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Confirm"
        description="Delete the token?"
        footer={<button type="button">Cancel</button>}
      >
        <button type="button">Middle</button>
      </Dialog>
    </>
  )
}

function openDialog() {
  render(<Harness />)
  const trigger = screen.getByRole('button', { name: 'Open dialog' })
  trigger.focus()
  fireEvent.click(trigger)
  return { trigger, dialog: screen.getByRole('dialog') }
}

describe('Dialog accessibility (UI-5)', () => {
  it('is a labelled modal and receives focus on open', () => {
    const { dialog } = openDialog()

    expect(dialog.getAttribute('aria-modal')).toBe('true')
    expect(dialog.getAttribute('aria-labelledby')).toBeTruthy()
    expect(dialog.textContent).toContain('Confirm')
    expect(document.activeElement).toBe(dialog)
  })

  it('cannot Tab out of the panel', () => {
    const { dialog } = openDialog()
    const middle = screen.getByRole('button', { name: 'Middle' })
    const cancel = screen.getByRole('button', { name: 'Cancel' })

    // Panel → first control.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(middle)

    // Tab from the last control wraps to the first, never leaving the panel.
    cancel.focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(middle)
    expect(dialog.contains(document.activeElement)).toBe(true)

    // Shift+Tab from the first control wraps to the last.
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(cancel)
    expect(dialog.contains(document.activeElement)).toBe(true)
  })

  it('returns focus to the trigger on Escape', () => {
    const { trigger } = openDialog()
    fireEvent.keyDown(document, { key: 'Escape' })

    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(trigger)
  })
})
