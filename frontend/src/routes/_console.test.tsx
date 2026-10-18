import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ConsoleHeader } from './_console'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('console layout i18n', () => {
  it('renders resolved common.nav keys and admin links for an admin', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ username: 'root', role: 'admin', status: 'enabled' }),
      ),
    )

    const { getByRole, getByText, queryByText } = await renderWithProviders(
      <ConsoleHeader />,
      ['common'],
    )

    const nav = getByRole('navigation')
    await waitFor(() => {
      expect(nav.textContent).toContain('Users')
      expect(nav.textContent).toContain('Groups')
    })
    expect(nav.textContent).toContain('Dashboard')
    expect(nav.textContent).toContain('Profile')
    expect(nav.textContent).toContain('Security')
    expect(getByText('NewTG')).toBeTruthy()
    expect(queryByText(/common\./)).toBeNull()
  })

  it('hides the admin section from a non-admin session', async () => {
    let meCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/me`, () => {
        meCalls += 1
        return HttpResponse.json({ username: 'alice', role: 'user', status: 'enabled' })
      }),
    )

    const { getByRole, findByText } = await renderWithProviders(
      <ConsoleHeader />,
      ['common'],
    )

    await findByText('Dashboard')
    await waitFor(() => {
      expect(meCalls).toBe(1)
    })
    const nav = getByRole('navigation')
    expect(nav.textContent).toContain('Dashboard')
    expect(nav.textContent).not.toContain('Users')
    expect(nav.textContent).not.toContain('Groups')
  })
})
