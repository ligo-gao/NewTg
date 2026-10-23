import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ChannelRowActions } from './channel-row-actions'
import { makeChannel, renderWithProviders } from './test-utils'

/**
 * Row actions render two controls: the status switch that was previously
 * inline in the list row, and a kebab menu that owns the per-row actions.
 *
 * The menu must be portalled to `document.body`: the table wrapper is
 * `overflow-x-auto`, which clips absolutely-positioned descendants vertically
 * in real browsers (jsdom has no layout, so the portal itself is asserted
 * structurally instead).
 */

const ITEMS = [
  ['Test connection', 'onTest'],
  ['Batch test', 'onBatchTest'],
  ['Edit', 'onEdit'],
  ['Delete', 'onDelete'],
] as const

function makeHandlers() {
  return {
    onToggleStatus: vi.fn(),
    onTest: vi.fn(),
    onBatchTest: vi.fn(),
    onEdit: vi.fn(),
    onDelete: vi.fn(),
  }
}

function renderActions(
  handlers: ReturnType<typeof makeHandlers>,
  channel = makeChannel(),
  busy = false,
) {
  return renderWithProviders(
    <ChannelRowActions
      channel={channel}
      busy={busy}
      onToggleStatus={handlers.onToggleStatus}
      onTest={handlers.onTest}
      onBatchTest={handlers.onBatchTest}
      onEdit={handlers.onEdit}
      onDelete={handlers.onDelete}
    />,
  )
}

describe('ChannelRowActions', () => {
  it('renders the status switch and the kebab trigger', () => {
    const handlers = makeHandlers()
    renderActions(handlers)

    const switchControl = screen.getByRole('switch')
    expect(switchControl.getAttribute('aria-checked')).toBe('true')
    expect(switchControl.getAttribute('aria-label')).toBe('Disable channel alpha')
    expect(switchControl.getAttribute('aria-busy')).toBe('false')

    const kebab = screen.getByRole('button', { name: 'More actions for alpha' })
    expect(kebab.getAttribute('aria-haspopup')).toBe('menu')
    expect(kebab.getAttribute('aria-expanded')).toBe('false')
  })

  it('opens a portalled menu with the four actions and focuses the first', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderActions(handlers)

    await user.click(screen.getByRole('button', { name: 'More actions for alpha' }))

    const menu = screen.getByRole('menu')
    expect(menu.parentElement).toBe(document.body)
    expect(screen.getByRole('button', { name: 'More actions for alpha' }).getAttribute('aria-expanded')).toBe(
      'true',
    )
    expect(screen.getAllByRole('menuitem').map((item) => item.textContent)).toEqual([
      'Test connection',
      'Batch test',
      'Edit',
      'Delete',
    ])
    expect(document.activeElement).toBe(
      screen.getByRole('menuitem', { name: 'Test connection' }),
    )
  })

  it.each(ITEMS)('runs %s and closes the menu', async (label, handlerName) => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderActions(handlers)

    await user.click(screen.getByRole('button', { name: 'More actions for alpha' }))
    await user.click(screen.getByRole('menuitem', { name: label }))

    expect(handlers[handlerName]).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(
      screen.getByRole('button', { name: 'More actions for alpha' }),
    )
  })

  it('closes on Escape and returns focus to the kebab', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderActions(handlers)

    const kebab = screen.getByRole('button', { name: 'More actions for alpha' })
    await user.click(kebab)
    expect(screen.getByRole('menu')).toBeTruthy()

    await user.keyboard('{Escape}')

    expect(screen.queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(kebab)
  })

  it('closes on an outside pointerdown without running an action', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderWithProviders(
      <>
        <ChannelRowActions
          channel={makeChannel()}
          busy={false}
          onToggleStatus={handlers.onToggleStatus}
          onTest={handlers.onTest}
          onBatchTest={handlers.onBatchTest}
          onEdit={handlers.onEdit}
          onDelete={handlers.onDelete}
        />
        <button type="button">outside</button>
      </>,
    )

    await user.click(screen.getByRole('button', { name: 'More actions for alpha' }))
    expect(screen.getByRole('menu')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'outside' }))

    expect(screen.queryByRole('menu')).toBeNull()
    expect(handlers.onEdit).not.toHaveBeenCalled()
  })

  it('closes on a viewport scroll and restores focus to the kebab', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderActions(handlers)

    const kebab = screen.getByRole('button', { name: 'More actions for alpha' })
    await user.click(kebab)
    expect(screen.getByRole('menu')).toBeTruthy()

    fireEvent.scroll(window)

    expect(screen.queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(kebab)
  })

  it('keeps the kebab focusable and inert while the row is busy', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderActions(handlers, makeChannel(), true)

    expect((screen.getByRole('switch') as HTMLButtonElement).disabled).toBe(true)
    expect(screen.getByTestId('channel-row-busy-1')).toBeTruthy()

    const kebab = screen.getByRole('button', { name: 'More actions for alpha' })
    // `aria-disabled` instead of `disabled`: a disabled control cannot hold
    // focus, and blurring it would drop keyboard focus to <body>.
    expect(kebab.getAttribute('aria-disabled')).toBe('true')
    expect((kebab as HTMLButtonElement).disabled).toBe(false)
    kebab.focus()
    expect(document.activeElement).toBe(kebab)

    await user.click(kebab)
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('closes the open menu, restores focus and locks items when the row becomes busy', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    const rendered = renderActions(handlers)

    const kebab = screen.getByRole('button', { name: 'More actions for alpha' })
    await user.click(kebab)
    expect(screen.getByRole('menu')).toBeTruthy()

    rendered.rerender(
      <ChannelRowActions
        channel={makeChannel()}
        busy
        onToggleStatus={handlers.onToggleStatus}
        onTest={handlers.onTest}
        onBatchTest={handlers.onBatchTest}
        onEdit={handlers.onEdit}
        onDelete={handlers.onDelete}
      />,
    )

    // The menu cannot stay open over a locked row, and focus lands back on the
    // (still focusable) kebab rather than <body>.
    expect(screen.queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(kebab)
    expect(kebab.getAttribute('aria-disabled')).toBe('true')
  })

  it('toggles the switch label with the channel status', () => {
    const handlers = makeHandlers()
    renderActions(handlers, makeChannel({ status: 2 }))

    const switchControl = screen.getByRole('switch')
    expect(switchControl.getAttribute('aria-checked')).toBe('false')
    expect(switchControl.getAttribute('aria-label')).toBe('Enable channel alpha')
  })

  it('toggles the status through the callback', async () => {
    const user = userEvent.setup()
    const handlers = makeHandlers()
    renderActions(handlers)

    await user.click(screen.getByRole('switch'))

    expect(handlers.onToggleStatus).toHaveBeenCalledTimes(1)
  })
})
