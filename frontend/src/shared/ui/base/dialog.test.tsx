import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Dialog } from './dialog'

beforeEach(() => {
  // jsdom performs no layout, so `getClientRects()` is empty for every element
  // and the dialog's visibility filter would reject all focus-trap candidates.
  // Give each element one rect so the test exercises browser behaviour.
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockImplementation(
    () => ({ length: 1 }) as unknown as DOMRectList,
  )
})

afterEach(cleanup)

interface HarnessProps {
  notify?: (open: boolean) => void
}

/**
 * Mirrors real usage: the caller owns `open` and passes an inline
 * `onOpenChange`, which is exactly the pattern that used to re-run the focus
 * effect on every parent render.
 */
function Harness({ notify }: HarnessProps) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open dialog
      </button>
      <Dialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          notify?.(next)
        }}
        title="Confirm"
        description="Delete the token?"
        footer={
          <>
            <button type="button" onClick={() => setOpen(false)}>
              Cancel
            </button>
            <button type="button">Delete</button>
          </>
        }
      >
        <button type="button">Middle</button>
      </Dialog>
    </>
  )
}

function openDialog() {
  render(<Harness />)
  const trigger = screen.getByRole('button', { name: 'Open dialog' })
  // Keyboard activation focuses the trigger before opening; jsdom's
  // `fireEvent.click` does not, so do it explicitly. The dialog restores focus
  // to whatever was focused at open time (the invoking control).
  trigger.focus()
  fireEvent.click(trigger)
  return { trigger, dialog: screen.getByRole('dialog') }
}

describe('Dialog focus management', () => {
  it('moves focus to the panel and makes the background inert', () => {
    const { dialog } = openDialog()

    expect(document.activeElement).toBe(dialog)

    const portalRoot = dialog.parentElement
    const background = Array.from(document.body.children).filter(
      (element) => element !== portalRoot,
    )
    expect(background.length).toBeGreaterThan(0)
    expect(background.every((element) => element.hasAttribute('inert'))).toBe(
      true,
    )
  })

  it('cycles Tab and Shift+Tab inside the panel', () => {
    const { dialog } = openDialog()
    const middle = screen.getByRole('button', { name: 'Middle' })
    const remove = screen.getByRole('button', { name: 'Delete' })

    // From the panel itself, Tab enters the first control in DOM order.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(middle)

    // Shift+Tab from the first control wraps to the last focusable element.
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(remove)

    // Tab from the last control wraps to the first.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(middle)

    expect(dialog.contains(document.activeElement)).toBe(true)
  })

  it('closes on Escape and restores focus to the trigger', () => {
    const { trigger, dialog } = openDialog()
    const portalRoot = dialog.parentElement
    const background = Array.from(document.body.children).filter(
      (element) => element !== portalRoot,
    )

    fireEvent.keyDown(document, { key: 'Escape' })

    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(trigger)
    expect(background.every((element) => !element.hasAttribute('inert'))).toBe(
      true,
    )
  })

  it('does not steal focus when the parent re-renders with a new callback', () => {
    const { rerender } = render(<Harness notify={() => {}} />)
    fireEvent.click(screen.getByRole('button', { name: 'Open dialog' }))

    const middle = screen.getByRole('button', { name: 'Middle' })
    middle.focus()

    // A fresh inline `onOpenChange` identity must not re-run capture/restore.
    rerender(<Harness notify={() => {}} />)

    expect(document.activeElement).toBe(middle)
  })
})
