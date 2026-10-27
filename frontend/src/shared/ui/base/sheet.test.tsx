import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Dialog } from './dialog'
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

/**
 * The production shape: a destructive confirmation `Dialog` opened from inside
 * a `Sheet` (discount delete over the discount sheet). Both are modal layers,
 * so they must share one stack; otherwise the sheet's document listener runs
 * first and closes the sheet together with the dialog (Escape) and pulls focus
 * into the sheet panel that is inert while the dialog sits on top (Tab).
 */
function NestedSheetHarness() {
  const [sheetOpen, setSheetOpen] = useState(true)
  const [dialogOpen, setDialogOpen] = useState(false)
  return (
    <Sheet
      open={sheetOpen}
      onOpenChange={setSheetOpen}
      title="Sheet"
      footer={
        <button type="button" onClick={() => setDialogOpen(true)}>
          Open confirmation
        </button>
      }
    >
      <button type="button">Sheet body</button>
      <Dialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        title="Confirm"
        footer={
          <>
            <button type="button" onClick={() => setDialogOpen(false)}>
              Dialog cancel
            </button>
            <button type="button">Delete</button>
          </>
        }
      >
        <button type="button">Dialog middle</button>
      </Dialog>
    </Sheet>
  )
}

describe('Sheet in the shared modal stack', () => {
  it('leaves Escape and Tab to a Dialog opened on top of it', () => {
    render(<NestedSheetHarness />)

    const sheetBody = screen.getByRole('button', { name: 'Sheet body' })
    fireEvent.click(screen.getByRole('button', { name: 'Open confirmation' }))

    // Both portals are body children; the sheet's is inert while the dialog is
    // open, so the accessibility tree hides it — query the DOM to see both.
    const layers = Array.from(
      document.querySelectorAll<HTMLElement>('[role="dialog"]'),
    )
    expect(layers).toHaveLength(2)
    const [sheetPanel, confirm] = layers as [HTMLElement, HTMLElement]
    expect(sheetPanel.textContent).toContain('Sheet')
    expect(confirm.textContent).toContain('Confirm')
    expect(sheetPanel.parentElement?.hasAttribute('inert')).toBe(true)

    // Tab from inside the Dialog must never be served by the lower sheet: focus
    // stays in the dialog and the (inert) sheet panel is never focused.
    const dialogMiddle = screen.getByRole('button', { name: 'Dialog middle' })
    dialogMiddle.focus()
    const sheetFocus = vi.fn()
    sheetBody.addEventListener('focus', sheetFocus)

    fireEvent.keyDown(document, { key: 'Tab' })
    expect(sheetFocus).not.toHaveBeenCalled()
    expect(confirm.contains(document.activeElement)).toBe(true)

    // Escape closes only the confirmation; the sheet stays open behind it.
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(1)
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
      'Sheet',
    )
    expect(sheetPanel.parentElement?.hasAttribute('inert')).toBe(false)
  })
})
