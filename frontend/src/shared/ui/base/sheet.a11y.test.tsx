import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Sheet } from './sheet'

/**
 * UI-5 accessibility regression for the right-hand Sheet: identical modal
 * semantics to Dialog (focus enters, Tab stays inside, focus returns to the
 * trigger, `aria-modal` + title association).
 */

beforeEach(() => {
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
        Open sheet
      </button>
      <Sheet
        open={open}
        onOpenChange={setOpen}
        title="Channel"
        description="Edit channel"
        footer={<button type="button">Save</button>}
      >
        <input aria-label="Channel name" />
      </Sheet>
    </>
  )
}

function openSheet() {
  render(<Harness />)
  const trigger = screen.getByRole('button', { name: 'Open sheet' })
  trigger.focus()
  fireEvent.click(trigger)
  return { trigger, sheet: screen.getByRole('dialog') }
}

describe('Sheet accessibility (UI-5)', () => {
  it('is a labelled modal and receives focus on open', () => {
    const { sheet } = openSheet()

    expect(sheet.getAttribute('aria-modal')).toBe('true')
    expect(sheet.getAttribute('aria-labelledby')).toBeTruthy()
    expect(sheet.textContent).toContain('Channel')
    expect(document.activeElement).toBe(sheet)
  })

  it('keeps Tab inside the sheet', () => {
    const { sheet } = openSheet()
    const name = screen.getByLabelText('Channel name')
    const save = screen.getByRole('button', { name: 'Save' })

    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(name)

    save.focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(name)
    expect(sheet.contains(document.activeElement)).toBe(true)

    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(save)
  })

  it('returns focus to the trigger on Escape', () => {
    const { trigger } = openSheet()
    fireEvent.keyDown(document, { key: 'Escape' })

    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(trigger)
  })
})
