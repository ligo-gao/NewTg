import { useQuery } from '@tanstack/react-query'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { AppSidebar } from '@/features/shell/app-sidebar'
import { meQueryOptions } from '@/lib/session'
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
  )
}

/**
 * The sidebar derives its entries from `/api/me`, so hold it back until the
 * session resolves — otherwise the "hides the entry" case would pass against
 * a nav that has not been populated yet.
 */
function SidebarHarness() {
  const me = useQuery(meQueryOptions())
  if (!me.isSuccess) return null
  return (
    <AppSidebar
      me={me.data}
      pathname="/dashboard"
      collapsed={false}
      onToggleCollapsed={() => {}}
      playgroundEnabled
    />
  )
}

describe('upstream reconciliation console entry', () => {
  it('renders the entry for a wildcard admin', async () => {
    installActor(['*'])
    await renderWithProviders(<SidebarHarness />, ['common'])

    const link = await screen.findByRole('link', {
      name: 'Upstream reconciliation',
    })
    expect(link.getAttribute('href')).toBe('/admin/upstream-reconciliation')
  })

  it('hides the entry without settings:manage', async () => {
    installActor(['channels:read'])
    await renderWithProviders(<SidebarHarness />, ['common'])

    // Channels is gated by channels:read, so finding it proves the session and
    // its permission set have been applied.
    await screen.findByRole('link', { name: 'Channels' })
    await waitFor(() => {
      expect(
        screen.queryByRole('link', { name: 'Upstream reconciliation' }),
      ).toBeNull()
    })
  })
})
