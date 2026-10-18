import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SecurityPage } from './SecurityPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('SecurityPage sessions cache (I8)', () => {
  it('refetches the session list after a successful password change', async () => {
    let sessionCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/sessions`, () => {
        sessionCalls += 1
        return HttpResponse.json({ sessions: [] })
      }),
      http.post(`${ORIGIN}/api/password`, () => new HttpResponse(null, { status: 204 })),
    )

    const { getByLabelText, getByRole, findByText, user } = {
      ...(await renderWithProviders(<SecurityPage />, [
        'common',
        'account',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    expect(await findByText('Login sessions')).toBeTruthy()
    await waitFor(() => {
      expect(sessionCalls).toBe(1)
    })

    await user.type(getByLabelText('Current password'), 'old password 1')
    await user.type(getByLabelText('New password'), 'new password 1')
    await user.type(getByLabelText('Confirm new password'), 'new password 1')
    await user.click(getByRole('button', { name: 'Change password' }))

    // The server revoked every other session; the mounted list must re-read it
    // instead of showing the killed devices for a staleTime window.
    await waitFor(() => {
      expect(sessionCalls).toBe(2)
    })
  })
})
