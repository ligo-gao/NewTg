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

describe('Dialog focus management', () => {  it('moves focus to the panel and makes the background inert', () => {
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

/**
 * A confirmation opened from a sheet/drawer is a second modal layer. The lower
 * dialog must stay mounted (it holds the form) but must not consume keyboard
 * events; otherwise Escape closes both layers and Tab can walk into the
 * now-inert lower panel.
 */
function NestedHarness() {
  const [outerOpen, setOuterOpen] = useState(false)
  const [innerOpen, setInnerOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOuterOpen(true)}>
        Open outer
      </button>
      <Dialog
        open={outerOpen}
        onOpenChange={setOuterOpen}
        title="Outer"
        footer={
          <>
            <button type="button" onClick={() => setOuterOpen(false)}>
              Outer cancel
            </button>
            <button type="button" onClick={() => setInnerOpen(true)}>
              Open inner
            </button>
          </>
        }
      >
        <button type="button">Outer middle</button>
      </Dialog>
      <Dialog
        open={innerOpen}
        onOpenChange={setInnerOpen}
        title="Inner"
        footer={
          <button type="button" onClick={() => setInnerOpen(false)}>
            Inner cancel
          </button>
        }
      >
        <button type="button">Inner middle</button>
      </Dialog>
    </>
  )
}

interface CloseHarnessProps {
  closeLabel?: string
  dismissible?: boolean
}

/** Opt-in close button: callers pass `closeLabel`, nothing renders by default. */
function CloseHarness({ closeLabel, dismissible }: CloseHarnessProps) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open dialog
      </button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Closeable"
        closeLabel={closeLabel}
        dismissible={dismissible}
      >
        <button type="button">Middle</button>
      </Dialog>
    </>
  )
}

describe('Dialog close button', () => {
  it('renders an accessible close button when closeLabel is set', () => {
    render(<CloseHarness closeLabel="Close dialog" />)
    fireEvent.click(screen.getByRole('button', { name: 'Open dialog' }))
    const dialog = screen.getByRole('dialog')

    const close = screen.getByRole('button', { name: 'Close dialog' })
    expect(dialog.contains(close)).toBe(true)
    // The glyph is decorative so the accessible name is exactly the label.
    expect(close.textContent).toBe('×')
    expect(close.querySelector('[aria-hidden="true"]')?.textContent).toBe('×')

    // Rendered before `children`: it is now the first focusable element.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(close)

    fireEvent.click(close)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('does not render a close button without closeLabel', () => {
    render(<CloseHarness />)
    fireEvent.click(screen.getByRole('button', { name: 'Open dialog' }))
    const dialog = screen.getByRole('dialog')

    expect(screen.queryByRole('button', { name: 'Close dialog' })).toBeNull()
    expect(dialog.querySelectorAll('button')).toHaveLength(1)

    // Focus order is unchanged: the child is still the first focusable element.
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(
      screen.getByRole('button', { name: 'Middle' }),
    )
  })

  it('keeps the close button out of the DOM when dismissible is false', () => {
    render(<CloseHarness closeLabel="Close dialog" dismissible={false} />)
    fireEvent.click(screen.getByRole('button', { name: 'Open dialog' }))
    const dialog = screen.getByRole('dialog')

    expect(screen.queryByRole('button', { name: 'Close dialog' })).toBeNull()
    expect(dialog.querySelectorAll('button')).toHaveLength(1)
  })
})

describe('Dialog modal stack', () => {
  it('lets only the topmost dialog handle Tab, Escape and inertness', () => {
    render(<NestedHarness />)
    fireEvent.click(screen.getByRole('button', { name: 'Open outer' }))
    const outer = screen.getByRole('dialog')
    fireEvent.click(screen.getByRole('button', { name: 'Open inner' }))

    // 外层 portal 在确认期间被 aria-hidden + inert，`getByRole` 会按可访问性
    // 树过滤掉它；这里直接查 DOM 以同时观察两层。
    const dialogs = Array.from(
      document.querySelectorAll<HTMLElement>('[role="dialog"]'),
    )
    expect(dialogs).toHaveLength(2)
    const inner = dialogs[1] as HTMLElement
    expect(inner.textContent).toContain('Inner')
    // 外层 portal 在确认期间必须整体 inert，读屏与 Tab 都不能进入。
    expect(outer.parentElement?.hasAttribute('inert')).toBe(true)

    const innerMiddle = screen.getByRole('button', { name: 'Inner middle' })
    const innerCancel = screen.getByRole('button', { name: 'Inner cancel' })

    // Tab 在顶层内部循环，且不会落回外层表单。
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(innerMiddle)
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(innerCancel)
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(innerMiddle)
    expect(inner.contains(document.activeElement)).toBe(true)

    // Escape 只关闭最上层。
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    expect(screen.getByRole('dialog').textContent).toContain('Outer')
    expect(outer.parentElement?.hasAttribute('inert')).toBe(false)

    // 顶层退出后，下层重新接管键盘。
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})
