import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { CommandPalette, useCommandPaletteShortcut } from './command-palette'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  // Spies installed with `vi.spyOn` (notably `getClientRects`) must not leak
  // into the next test.
  vi.restoreAllMocks()
  useToastStore.setState({ toasts: [] })
})
afterAll(() => server.close())

beforeEach(() => {
  // jsdom has no layout: every element reports zero client rects, which makes
  // the dialog's focus-trap helper treat the whole panel as having no
  // focusable content. Give elements one synthetic rect so Tab trapping can be
  // exercised the way a browser implements it.
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue({
    length: 1,
    item: () => null,
    [Symbol.iterator]: () => [][Symbol.iterator](),
  } as unknown as DOMRectList)
})

const ME_FULL_ADMIN = {
  username: 'root',
  role: 'admin' as const,
  status: 'enabled' as const,
  permissions: ['*'],
}

const ME_LOGS_ADMIN = {
  username: 'root',
  role: 'admin' as const,
  status: 'enabled' as const,
  permissions: ['logs:read'],
}

const ME_USER = {
  username: 'alice',
  role: 'user' as const,
  status: 'enabled' as const,
  permissions: [] as string[],
}

function installMe(me: typeof ME_FULL_ADMIN | typeof ME_USER) {
  server.use(http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(me)))
}

/**
 * Reproduces the two shell responsibilities under test: the global shortcut
 * listener and the single mounted palette instance.
 */
function ShellHarness({ onNavigate }: { onNavigate: (to: string) => void }) {
  const [open, setOpen] = useState(false)
  useCommandPaletteShortcut(() => setOpen(true))

  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        trigger
      </button>
      <CommandPalette
        open={open}
        onOpenChange={setOpen}
        onNavigate={onNavigate}
      />
    </>
  )
}

async function renderHarness(
  me: typeof ME_FULL_ADMIN | typeof ME_USER = ME_USER,
  onNavigate: (to: string) => void = () => {},
) {
  installMe(me)
  const result = await renderWithProviders(
    <ShellHarness onNavigate={onNavigate} />,
    ['common'],
  )
  return result
}

async function openPalette() {
  fireEvent.keyDown(document, { key: 'k', metaKey: true })
  return screen.findByRole('combobox')
}

describe('command palette shortcuts and focus', () => {
  it('opens on Cmd+K and Ctrl+K', async () => {
    await renderHarness()

    fireEvent.keyDown(document, { key: 'k', metaKey: true })
    expect(await screen.findByRole('dialog')).toBeTruthy()

    // Close again, then prove the Windows/Linux combination works too.
    fireEvent.keyDown(screen.getByRole('combobox'), { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    fireEvent.keyDown(document, { key: 'k', ctrlKey: true })
    expect(await screen.findByRole('dialog')).toBeTruthy()
  })

  it('closes on Escape and restores focus to the previously focused element', async () => {
    const user = userEvent.setup()
    await renderHarness()

    const trigger = screen.getByRole('button', { name: 'trigger' })
    trigger.focus()
    expect(document.activeElement).toBe(trigger)

    fireEvent.keyDown(document, { key: 'k', ctrlKey: true })
    const input = await screen.findByRole('combobox')
    expect(document.activeElement).toBe(input)

    // web-00 §15.1.3: the first Escape clears a non-empty query, the second
    // closes the panel.
    await user.type(input, 'dash')
    expect((input as HTMLInputElement).value).toBe('dash')
    fireEvent.keyDown(input, { key: 'Escape' })
    expect((input as HTMLInputElement).value).toBe('')

    fireEvent.keyDown(input, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(document.activeElement).toBe(trigger)
  })

  it('traps Tab inside the panel and keeps focus in the combobox', async () => {
    const user = userEvent.setup()
    await renderHarness()

    const input = await openPalette()
    await user.tab()
    expect(document.activeElement).toBe(input)
    await user.tab({ shift: true })
    expect(document.activeElement).toBe(input)
  })
})

describe('command palette RBAC', () => {
  it('keeps session pages for an ordinary user but hides every privileged command', async () => {
    const user = userEvent.setup()
    await renderHarness(ME_USER)

    const input = await openPalette()

    // /audit-logs is a session page: it shows the caller their own events, so
    // ordinary users keep the command; logs:read only widens scope inside it.
    expect(
      await screen.findByRole('option', { name: /Audit Logs/ }),
    ).toBeTruthy()

    await user.type(input, 'users')
    await waitFor(() => {
      expect(screen.queryByRole('option', { name: /Users/ })).toBeNull()
    })
    expect(screen.queryByRole('option', { name: /Channels/ })).toBeNull()
    expect(screen.getByRole('status').textContent).toContain('No results')
  })

  it('shows session pages but no admin pages for a scoped admin', async () => {
    const user = userEvent.setup()
    await renderHarness(ME_LOGS_ADMIN)

    const input = await openPalette()
    await user.type(input, 'audit')

    expect(
      await screen.findByRole('option', { name: /Audit Logs/ }),
    ).toBeTruthy()
    expect(screen.queryByRole('option', { name: /Users/ })).toBeNull()
    expect(screen.queryByRole('option', { name: /Channels/ })).toBeNull()
    expect(screen.queryByRole('option', { name: /Pricing/ })).toBeNull()
  })

  it('shows all admin pages for a full admin', async () => {
    const user = userEvent.setup()
    await renderHarness(ME_FULL_ADMIN)

    const input = await openPalette()
    await user.type(input, 'users')
    expect(await screen.findByRole('option', { name: /Users/ })).toBeTruthy()

    await user.clear(input)
    await user.type(input, 'channels')
    expect(await screen.findByRole('option', { name: /Channels/ })).toBeTruthy()

    await user.clear(input)
    await user.type(input, 'model pricing')
    expect(
      await screen.findByRole('option', { name: /Model pricing/ }),
    ).toBeTruthy()
  })

  it('fails closed while /api/me is still loading', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () => new Promise<never>(() => {})),
    )
    const result = await renderWithProviders(
      <ShellHarness onNavigate={() => {}} />,
      ['common'],
    )

    await openPalette()
    within(screen.getByRole('dialog')).getByText('Dashboard')

    // Ungated session pages (including Audit Logs) remain available while the
    // capability probe is pending.
    expect(
      await screen.findByRole('option', { name: /Audit Logs/ }),
    ).toBeTruthy()
    expect(screen.queryByRole('option', { name: /Users/ })).toBeNull()
    expect(screen.queryByRole('option', { name: /Channels/ })).toBeNull()

    result.unmount()
  })

  it('fails closed when /api/me errors', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Server error' },
          { status: 500 },
        ),
      ),
    )
    const result = await renderWithProviders(
      <ShellHarness onNavigate={() => {}} />,
      ['common'],
    )

    await openPalette()
    expect(
      await screen.findByRole('option', { name: /Audit Logs/ }),
    ).toBeTruthy()
    expect(screen.queryByRole('option', { name: /Users/ })).toBeNull()
    expect(screen.queryByRole('option', { name: /Channels/ })).toBeNull()
    expect(screen.queryByRole('option', { name: /Pricing/ })).toBeNull()

    result.unmount()
  })
})

describe('command palette matching and navigation', () => {
  it('matches a case-insensitive subsequence, groups results and navigates on Enter', async () => {
    const user = userEvent.setup()
    const onNavigate = vi.fn()
    await renderHarness(ME_FULL_ADMIN, onNavigate)

    const input = await openPalette()

    // Fuzzy subsequence over label + keywords: u-s-l-g.
    await user.type(input, 'uslg')
    const option = await screen.findByRole('option', { name: /Usage Logs/ })
    expect(option.getAttribute('aria-selected')).toBe('true')

    await user.keyboard('{Enter}')
    expect(onNavigate).toHaveBeenCalledWith('/usage-logs')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('renders grouped pages and actions with combobox/listbox semantics', async () => {
    await renderHarness(ME_FULL_ADMIN)
    const input = await openPalette()

    expect(input.getAttribute('aria-expanded')).toBe('true')
    expect(input.getAttribute('aria-autocomplete')).toBe('list')
    expect(input.getAttribute('aria-controls')).toBe('command-listbox')
    expect(screen.getByRole('listbox')).toBeTruthy()
    expect(screen.getByRole('group', { name: 'Pages' })).toBeTruthy()
    expect(screen.getByRole('group', { name: 'Actions' })).toBeTruthy()
    expect(screen.getByText(/\d+ results/)).toBeTruthy()
  })

  it('moves the active option with ArrowDown/ArrowUp and Home/End', async () => {
    await renderHarness(ME_FULL_ADMIN)
    const input = await openPalette()

    const initial = input.getAttribute('aria-activedescendant')
    expect(initial).not.toBeNull()

    fireEvent.keyDown(input, { key: 'ArrowDown' })
    const second = input.getAttribute('aria-activedescendant')
    expect(second).not.toBe(initial)
    expect(document.getElementById(second ?? '')?.getAttribute('role')).toBe(
      'option',
    )
    expect(document.getElementById(second ?? '')?.getAttribute('aria-selected')).toBe(
      'true',
    )

    fireEvent.keyDown(input, { key: 'ArrowUp' })
    expect(input.getAttribute('aria-activedescendant')).toBe(initial)

    fireEvent.keyDown(input, { key: 'End' })
    const last = input.getAttribute('aria-activedescendant')
    expect(last).not.toBe(initial)
    fireEvent.keyDown(input, { key: 'Home' })
    expect(input.getAttribute('aria-activedescendant')).toBe(initial)
  })

  it('renders an empty state when nothing matches', async () => {
    const user = userEvent.setup()
    await renderHarness(ME_FULL_ADMIN)
    const input = await openPalette()

    await user.type(input, 'zzzzz')
    expect(screen.queryAllByRole('option')).toHaveLength(0)
    expect(screen.getByRole('status').textContent).toContain('No results')
    expect(screen.getByText('0 results')).toBeTruthy()
  })

  it('reports a clipboard failure instead of a false success', async () => {
    const user = userEvent.setup()
    // userEvent installs a working clipboard stub; make the write deny so the
    // legacy fallback (absent in jsdom) also fails.
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(
      new Error('denied'),
    )
    await renderHarness(ME_USER)

    const input = await openPalette()
    await user.type(input, 'copy')
    await user.click(
      await screen.findByRole('option', { name: /Copy current page URL/ }),
    )

    await waitFor(() => {
      expect(useToastStore.getState().toasts).toHaveLength(1)
    })
    expect(useToastStore.getState().toasts[0]?.tone).toBe('error')
  })
})

describe('command palette global listener lifecycle', () => {
  it('installs exactly one keydown listener and removes it on unmount', async () => {
    const onOpen = vi.fn()
    function Probe() {
      useCommandPaletteShortcut(onOpen)
      return null
    }

    const addSpy = vi.spyOn(document, 'addEventListener')
    const removeSpy = vi.spyOn(document, 'removeEventListener')

    const { unmount } = await renderWithProviders(<Probe />, ['common'])

    const keydownAdds = addSpy.mock.calls.filter(([type]) => type === 'keydown')
    expect(keydownAdds).toHaveLength(1)
    const handler = keydownAdds[0]?.[1]

    unmount()
    expect(
      removeSpy.mock.calls.some(
        ([type, removed]) => type === 'keydown' && removed === handler,
      ),
    ).toBe(true)

    fireEvent.keyDown(document, { key: 'k', metaKey: true })
    expect(onOpen).not.toHaveBeenCalled()
  })
})
