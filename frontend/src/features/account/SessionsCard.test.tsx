import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { SessionsCard } from './SessionsCard'

type SessionView = components['schemas']['SessionView']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function twoSessions(): SessionView[] {
  return [
    {
      id: 's-current',
      user_agent: 'Mozilla/5.0 (Macintosh) Chrome/131',
      ip: '192.168.1.10',
      created_at: '2026-10-01T10:00:00Z',
      last_seen_at: '2026-10-18T12:00:00Z',
      current: true,
    },
    {
      id: 's-other',
      user_agent: 'Mozilla/5.0 (iPhone) Safari',
      ip: '192.168.1.20',
      created_at: '2026-10-02T10:00:00Z',
      last_seen_at: '2026-10-17T12:00:00Z',
      current: false,
    },
  ]
}

describe('SessionsCard', () => {
  it('lists sessions and revokes a single non-current session', async () => {
    let sessions = twoSessions()
    let revokeCalls = 0

    server.use(
      http.get(`${ORIGIN}/api/sessions`, () => HttpResponse.json({ sessions })),
      http.delete(`${ORIGIN}/api/sessions/s-other`, () => {
        revokeCalls += 1
        sessions = sessions.filter((session) => session.id !== 's-other')
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const { getByText, getByRole, queryByText, findByText } =
      await renderWithProviders(<SessionsCard />, ['common', 'account'])
    const user = userEvent.setup()

    expect(await findByText(/192\.168\.1\.20/)).toBeTruthy()
    expect(getByText('Current device')).toBeTruthy()

    await user.click(getByRole('button', { name: 'Sign out' }))

    await waitFor(() => {
      expect(queryByText(/192\.168\.1\.20/)).toBeNull()
    })
    expect(revokeCalls).toBe(1)
  })

  it('revokes every other session through the confirmation dialog', async () => {
    let sessions = twoSessions()
    let revokeOthersCalls = 0

    server.use(
      http.get(`${ORIGIN}/api/sessions`, () => HttpResponse.json({ sessions })),
      http.post(`${ORIGIN}/api/sessions/revoke-others`, () => {
        revokeOthersCalls += 1
        sessions = sessions.filter((session) => session.current)
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const { getByRole, findByRole, queryByText, findByText } =
      await renderWithProviders(<SessionsCard />, ['common', 'account'])
    const user = userEvent.setup()

    expect(await findByText(/192\.168\.1\.20/)).toBeTruthy()
    await user.click(getByRole('button', { name: 'Sign out other sessions' }))

    const dialog = await findByRole('dialog')
    const confirm = dialog.querySelector('button[data-variant="danger"]')
    expect(confirm).not.toBeNull()
    await user.click(confirm as HTMLElement)

    await waitFor(() => {
      expect(queryByText(/192\.168\.1\.20/)).toBeNull()
    })
    expect(revokeOthersCalls).toBe(1)
  })
})
