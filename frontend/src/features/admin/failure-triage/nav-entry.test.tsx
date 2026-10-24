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
 * session resolves — otherwise the "hides the entry" case could pass against
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

describe('failure triage console entry', () => {
  it('renders the entry with logs:read', async () => {
    installActor(['logs:read'])
    await renderWithProviders(<SidebarHarness />, ['common'])

    // Finding the nav at all proves the session and its permission set have
    // been applied; the entry itself is gated on logs:read alone.
    await screen.findByRole('link', { name: 'Dashboard' })
    const link = await screen.findByRole('link', { name: 'Failure triage' })
    expect(link.getAttribute('href')).toBe('/admin/system/failure-triage')
  })

  it('hides the entry without logs:read', async () => {
    installActor(['users:read'])
    await renderWithProviders(<SidebarHarness />, ['common'])

    // Users is gated by users:read, so finding it proves the sidebar rendered
    // with the actor's permission set before we assert the absence.
    await screen.findByRole('link', { name: 'Users' })
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'Failure triage' })).toBeNull()
    })
  })
})
