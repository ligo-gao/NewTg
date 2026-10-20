import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { ConsoleHeader } from '@/routes/_console'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installActor(role: 'admin' | 'user', permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/notifications`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 }),
    ),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'alice', role, status: 'enabled', permissions }),
    ),
    // ConsoleHeader 的定价能力探测；空列表即「无 pricing:read」。
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

describe('models console entry', () => {
  it('renders the entry for an admin holding channels:read', async () => {
    installActor('admin', ['channels:read'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Users' })
    const link = await screen.findByRole('link', { name: 'Models & Vendors' })
    expect(link.getAttribute('href')).toBe('/admin/models')
  })

  it('hides the entry from admins without channels:read', async () => {
    installActor('admin', ['channels:manage'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Users' })
    expect(screen.queryByRole('link', { name: 'Models & Vendors' })).toBeNull()
  })

  it('hides the entry from non-admin sessions', async () => {
    installActor('user', ['channels:read'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Dashboard' })
    expect(screen.queryByRole('link', { name: 'Models & Vendors' })).toBeNull()
  })
})
