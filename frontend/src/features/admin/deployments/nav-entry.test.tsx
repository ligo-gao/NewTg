import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { ConsoleHeader } from '@/routes/_console'

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
    // ConsoleHeader 的价格能力探测；返回空行即「无 pricing:read」。
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

describe('model deployments console entry', () => {
  it('renders the entry only with settings:manage', async () => {
    installActor(['settings:manage'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Users' })
    const link = await screen.findByRole('link', { name: 'Model deployments' })
    expect(link.getAttribute('href')).toBe('/admin/system/deployments')
  })

  it('hides the entry without settings:manage', async () => {
    installActor(['users:read'])
    await renderWithProviders(<ConsoleHeader />, ['common'])

    await screen.findByRole('link', { name: 'Users' })
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'Model deployments' })).toBeNull()
    })
  })
})
