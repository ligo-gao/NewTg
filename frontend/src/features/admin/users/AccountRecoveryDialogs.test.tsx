import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { AccountRecoveryDialogs } from './AccountRecoveryDialogs'

type AdminUser = components['schemas']['AdminUser']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function adminUser(): AdminUser {
  return {
    id: 7,
    username: 'alice',
    display_name: null,
    email: null,
    role: 'user',
    status: 'enabled',
    balance: 1000,
    group: 'default',
    admin_permissions: null,
    totp_enabled: true,
    must_change_password: false,
    token_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
  }
}

describe('AccountRecoveryDialogs one-time material (I6)', () => {
  it('does not dismiss the result step on Escape', async () => {
    server.use(
      http.post(`${ORIGIN}/api/users/7/reset-password`, () =>
        HttpResponse.json({ temp_password: 'TMP-2468-ABCD' }),
      ),
    )

    const onClose = vi.fn()
    const { getByRole, findByText, queryByText, user } = {
      ...(await renderWithProviders(
        <AccountRecoveryDialogs user={adminUser()} onClose={onClose} />,
        ['common', 'admin'],
      )),
      user: userEvent.setup(),
    }

    await user.click(getByRole('button', { name: 'Reset Password' }))
    await user.click(getByRole('button', { name: 'Reset Password' }))

    expect(await findByText('TMP-2468-ABCD')).toBeTruthy()

    await user.keyboard('{Escape}')

    // The temporary password exists only in component state; Escape must not
    // discard it. Only the footer's explicit acknowledgement closes.
    expect(queryByText('TMP-2468-ABCD')).not.toBeNull()
    expect(getByRole('dialog')).toBeTruthy()
    expect(onClose).not.toHaveBeenCalled()

    await user.click(getByRole('button', { name: "I've saved it, close" }))
    await waitFor(() => {
      expect(onClose).toHaveBeenCalledTimes(1)
    })
  })
})
