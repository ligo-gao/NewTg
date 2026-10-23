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

describe('SecurityPage forced password change errors', () => {
  async function submitWith(
    response: () => Response | Promise<Response>,
  ): Promise<Awaited<ReturnType<typeof renderWithProviders>>> {
    server.use(http.post(`${ORIGIN}/api/password`, response))
    const result = await renderWithProviders(
      <SecurityPage forcePasswordChange />,
      ['common', 'account', 'auth'],
    )
    const user = userEvent.setup()
    await user.type(result.getByLabelText('Current password'), 'temp password 1')
    await user.type(result.getByLabelText('New password'), 'new password 1')
    await user.type(result.getByLabelText('Confirm new password'), 'new password 1')
    await user.click(result.getByRole('button', { name: 'Save new password' }))
    return result
  }

  it('names the current password when the server rejects it', async () => {
    const { findByText } = await submitWith(() =>
      HttpResponse.json(
        {
          status: 400,
          type: 'https://newtg.dev/problems/invalid_current_password',
          title: 'Current password is incorrect',
        },
        { status: 400 },
      ),
    )

    expect(await findByText('The current password is incorrect.')).toBeTruthy()
  })

  it('shows the server retry window on a rate limit', async () => {
    const { findByText } = await submitWith(() =>
      HttpResponse.json(
        {
          status: 429,
          type: 'https://newtg.dev/problems/too_many_requests',
          title: 'Too many password change attempts',
        },
        { status: 429, headers: { 'Retry-After': '42' } },
      ),
    )

    expect(
      await findByText('Too many attempts. Try again in 42 seconds.'),
    ).toBeTruthy()
  })

  it('explains that the new password was rejected', async () => {
    const { findByText } = await submitWith(() =>
      HttpResponse.json(
        {
          status: 400,
          type: 'https://newtg.dev/problems/bad_request',
          title: 'Password is too weak or has been compromised',
        },
        { status: 400 },
      ),
    )

    expect(
      await findByText('The new password does not meet the requirements.'),
    ).toBeTruthy()
  })
})
