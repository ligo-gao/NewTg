import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { TwoFactorChallengePage } from './TwoFactorChallengePage'

const ORIGIN = 'http://localhost:3000'
const CHALLENGE_URL = `${ORIGIN}/api/auth/2fa/challenge`

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

async function renderChallenge() {
  const onNavigate = vi.fn()
  const result = await renderWithProviders(
    <TwoFactorChallengePage
      ticket="ticket-1"
      redirect="/profile"
      onNavigate={onNavigate}
    />,
    ['common', 'auth'],
  )
  return { ...result, user: userEvent.setup(), onNavigate }
}

describe('TwoFactorChallengePage expiry (I10/N1)', () => {
  it('renders the expired state and a way back to sign-in on the ticket-invalid problem type', async () => {
    server.use(
      http.post(CHALLENGE_URL, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/two_factor_ticket_invalid',
            title: 'unknown or expired ticket',
          },
          { status: 400 },
        ),
      ),
    )

    const { getByRole, findByText, user, onNavigate } = await renderChallenge()

    await user.click(getByRole('tab', { name: 'Backup code' }))
    await user.type(getByRole('textbox', { name: 'Backup code' }), 'abcdefgh')
    await user.click(getByRole('button', { name: 'Verify' }))

    expect(await findByText('Verification expired')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Back to sign in' }))
    expect(onNavigate).toHaveBeenCalledWith('/sign-in')
  })

  it('keeps the form on a wrong-code 400 (bad_request)', async () => {
    server.use(
      http.post(CHALLENGE_URL, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/bad_request', title: 'bad request' },
          { status: 400 },
        ),
      ),
    )

    const { getByRole, findByRole, getByText, queryByText, user } =
      await renderChallenge()

    await user.click(getByRole('tab', { name: 'Backup code' }))
    await user.type(getByRole('textbox', { name: 'Backup code' }), 'abcdefgh')
    await user.click(getByRole('button', { name: 'Verify' }))

    expect(await findByRole('alert')).toBeTruthy()
    expect(getByText('Incorrect code')).toBeTruthy()
    // Not the terminal expired screen: the visitor can immediately retry.
    expect(queryByText('Verification expired')).toBeNull()
    expect(getByRole('button', { name: 'Verify' })).toBeTruthy()
  })

  it('moves between tabs with the arrow keys', async () => {
    const { getByRole, user } = await renderChallenge()

    const totpTab = getByRole('tab', { name: 'Code' })
    totpTab.focus()
    await user.keyboard('{ArrowRight}')

    const backupTab = getByRole('tab', { name: 'Backup code' })
    expect(backupTab.getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(backupTab)
  })

  it('starts a Retry-After countdown on 429 and disables submission', async () => {
    server.use(
      http.post(CHALLENGE_URL, () =>
        HttpResponse.json(
          { type: 'rate_limited', title: 'Too many requests' },
          { status: 429, headers: { 'Retry-After': '25' } },
        ),
      ),
    )

    const { getByRole, findByRole, user } = await renderChallenge()

    await user.click(getByRole('tab', { name: 'Backup code' }))
    await user.type(getByRole('textbox', { name: 'Backup code' }), 'abcdefgh')
    await user.click(getByRole('button', { name: 'Verify' }))

    const retry = await findByRole('button', { name: 'Try again in 25s' })
    expect((retry as HTMLButtonElement).disabled).toBe(true)
  })
})
