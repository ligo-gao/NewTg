import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { Me } from '@/lib/session'
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
afterEach(() => server.resetHandlers())
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
