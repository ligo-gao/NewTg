import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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

import { THEME_STORAGE_KEY, ThemeProvider } from '@/app/providers/theme'
import type { Me } from '@/lib/session'
import { useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { AppTopbar } from './app-topbar'

const ORIGIN = 'http://localhost:3000'
const ADMIN = {
  role: 'admin',
  username: 'root',
  permissions: ['*'],
} as unknown as Me

const server = setupServer(
  http.get(`${ORIGIN}/api/notifications/unread-count`, () =>
    HttpResponse.json({ unread: 0 }),
  ),
  http.get(`${ORIGIN}/api/notifications`, () =>
    HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
  ),
)

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => window.localStorage.clear())
afterEach(() => {
  server.resetHandlers()
  window.localStorage.clear()
})
afterAll(() => server.close())

describe('AppTopbar', () => {
  it('shows the current page and opens the command palette', async () => {
    const onOpenCommandPalette = vi.fn()
    const { getByTestId, getByText } = await renderWithProviders(
      <AppTopbar
        title="API tokens"
        me={ADMIN}
        onOpenCommandPalette={onOpenCommandPalette}
      />,
      ['common'],
    )

    expect(getByText('API tokens')).toBeTruthy()
    expect(getByText('Console')).toBeTruthy()
    getByTestId('command-palette-trigger').click()
    expect(onOpenCommandPalette).toHaveBeenCalledTimes(1)
  })

  it('opens the account menu from the avatar and closes it with Escape', async () => {
    const { getByRole, queryByRole } = await renderWithProviders(
      <AppTopbar
        title="API tokens"
        me={ADMIN}
        onOpenCommandPalette={() => {}}
      />,
      ['common'],
    )
    const user = userEvent.setup()
    const trigger = getByRole('button', { name: 'Account menu' })

    expect(trigger.getAttribute('aria-expanded')).toBe('false')
    expect(queryByRole('menu')).toBeNull()

    await user.click(trigger)

    expect(trigger.getAttribute('aria-expanded')).toBe('true')
    const menu = getByRole('menu')
    expect(
      within(menu).getByRole('menuitem', { name: 'Profile' }).getAttribute('href'),
    ).toBe('/profile')
    expect(within(menu).getByRole('menuitem', { name: 'Sign out' })).toBeTruthy()

    await user.keyboard('{Escape}')

    expect(queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(trigger)
  })

  it('reports a failed sign-out and stays on the page', async () => {
    useToastStore.setState({ toasts: [] })
    let calls = 0
    server.use(
      http.post(`${ORIGIN}/api/logout`, () => {
        calls += 1
        return HttpResponse.json(
          { title: 'Service Unavailable', status: 503 },
          { status: 503 },
        )
      }),
    )
    const { getByRole, queryByRole } = await renderWithProviders(
      <AppTopbar
        title="API tokens"
        me={ADMIN}
        onOpenCommandPalette={() => {}}
      />,
      ['common'],
    )
    const user = userEvent.setup()

    await user.click(getByRole('button', { name: 'Account menu' }))
    await user.click(getByRole('menuitem', { name: 'Sign out' }))

    await waitFor(() => expect(calls).toBe(1))
    await waitFor(() => {
      expect(useToastStore.getState().toasts.at(-1)?.message).toBe(
        'Could not sign out. Please try again.',
      )
    })
    expect(queryByRole('menu')).toBeNull()
  })

  it('renders the back-to-site link, language and theme controls', async () => {
    const { getByLabelText, getByRole } = await renderWithProviders(
      <AppTopbar
        title="API tokens"
        me={ADMIN}
        onOpenCommandPalette={() => {}}
      />,
      ['common'],
    )

    expect(getByRole('link', { name: 'Back to home' }).getAttribute('href')).toBe(
      '/',
    )
    expect(getByLabelText('Interface language').tagName).toBe('SELECT')
    const theme = getByLabelText('Theme') as HTMLSelectElement
    expect(theme.tagName).toBe('SELECT')
    expect(theme.value).toBe('system')
  })

  it('persists a language change for a signed-in session', async () => {
    let putBody: unknown
    server.use(
      http.get(`${ORIGIN}/api/self`, () =>
        HttpResponse.json({
          language: 'en',
          sidebar_modules: null,
          record_ip_log: false,
          updated_at: '2026-10-19T10:00:00Z',
        }),
      ),
      http.put(`${ORIGIN}/api/self/setting`, async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json({
          language: 'fr',
          sidebar_modules: null,
          record_ip_log: false,
          updated_at: '2026-10-19T10:00:00Z',
        })
      }),
    )

    const { getByLabelText } = await renderWithProviders(
      <AppTopbar
        title="API tokens"
        me={ADMIN}
        onOpenCommandPalette={() => {}}
      />,
      ['common'],
    )
    const user = userEvent.setup()

    await user.selectOptions(
      getByLabelText('Interface language') as HTMLSelectElement,
      'fr',
    )

    await waitFor(() => {
      expect(putBody).toMatchObject({
        language: 'fr',
        updated_at: '2026-10-19T10:00:00Z',
      })
    })  })

  it('applies and persists a theme change through the provider', async () => {
    const { getByLabelText } = await renderWithProviders(
      <ThemeProvider>
        <AppTopbar
          title="API tokens"
          me={ADMIN}
          onOpenCommandPalette={() => {}}
        />
      </ThemeProvider>,
      ['common'],
    )
    const user = userEvent.setup()

    await user.selectOptions(getByLabelText('Theme') as HTMLSelectElement, 'dark')

    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('hides the notification bell when the session is restricted', async () => {
    const { queryByRole } = await renderWithProviders(
      <AppTopbar
        title="Set a new password"
        me={ADMIN}
        showNotifications={false}
        onOpenCommandPalette={() => {}}
      />,
      ['common'],
    )

    expect(
      queryByRole('button', { name: /^Notifications/ }),
    ).toBeNull()
  })
})
