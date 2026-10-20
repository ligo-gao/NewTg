import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { ConsoleHeader } from '@/routes/_console'
import { renderWithProviders } from '@/test-support/providers'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installActor(permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/notifications`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 }),
    ),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
    // ConsoleHeader 的定价能力探测；空列表即「无 pricing:read」。
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

describe('subscription plans console entry', () => {
  it('renders the entry only with settings:manage', async () => {
    installActor(['settings:manage'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Users' })
    const link = await screen.findByRole('link', { name: 'Subscriptions' })
    expect(link.getAttribute('href')).toBe('/admin/subscriptions')
  })

  it('hides the entry without settings:manage', async () => {
    installActor(['users:read'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Users' })
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'Subscriptions' })).toBeNull()
    })
  })
})
