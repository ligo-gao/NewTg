import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { AdjustQuotaDialog } from './AdjustQuotaDialog'

type AdminUser = components['schemas']['AdminUser']

const ORIGIN = 'http://localhost:3000'
const USER_URL = `${ORIGIN}/api/users/7`
const ADJUST_URL = `${ORIGIN}/api/users/7/quota-adjustment`

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
    totp_enabled: false,
    must_change_password: false,
    token_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
  }
}

function installDetail() {
  server.use(http.get(USER_URL, () => HttpResponse.json(adminUser())))
}

async function renderDialog() {
  const onOpenChange = vi.fn()
  const result = await renderWithProviders(
    <AdjustQuotaDialog
      user={adminUser()}
      open
      onOpenChange={onOpenChange}
    />,
    ['common', 'admin'],
  )
  return { ...result, user: userEvent.setup(), onOpenChange }
}

describe('AdjustQuotaDialog idempotency (I4)', () => {
  it('reuses the same business key and Idempotency-Key header on retry', async () => {
    installDetail()
    const bodies: Record<string, unknown>[] = []
    const headers: (string | null)[] = []
    let calls = 0

    server.use(
      http.post(ADJUST_URL, async ({ request }) => {
        calls += 1
        bodies.push((await request.json()) as Record<string, unknown>)
        headers.push(request.headers.get('Idempotency-Key'))
        if (calls === 1) {
          return HttpResponse.json(
            { type: 'server_error', title: 'boom' },
            { status: 500 },
          )
        }
        return HttpResponse.json({
          user_id: 7,
          new_balance: 1100,
          transaction_id: 42,
        })
      }),
    )

    const { getByLabelText, getByRole, findByRole, user } = await renderDialog()

    await user.type(getByLabelText('Amount (credits)'), '100')
    await user.click(getByRole('button', { name: 'Confirm Adjustment' }))

    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('Could not adjust the quota')

    // Retry the same intent without touching mode/amount.
    await user.click(getByRole('button', { name: 'Confirm Adjustment' }))

    await waitFor(() => {
      expect(calls).toBe(2)
    })
    expect(bodies[0]?.idempotency_key).toBeTruthy()
    expect(bodies[1]?.idempotency_key).toBe(bodies[0]?.idempotency_key)
    // The HTTP header mirrors the ledger key so the client and server agree
    // on the replay identity.
    expect(headers[1]).toBe(headers[0])
    expect(headers[1]).toBe(bodies[0]?.idempotency_key)
  })

  it('mints a new key after a successful adjustment', async () => {
    installDetail()
    const keys: unknown[] = []
    let calls = 0

    server.use(
      http.post(ADJUST_URL, async ({ request }) => {
        calls += 1
        const body = (await request.json()) as Record<string, unknown>
        keys.push(body.idempotency_key)
        return HttpResponse.json({
          user_id: 7,
          new_balance: 1000 + calls * 100,
          transaction_id: calls,
        })
      }),
    )

    const { getByLabelText, getByRole, user } = await renderDialog()

    await user.type(getByLabelText('Amount (credits)'), '100')
    await user.click(getByRole('button', { name: 'Confirm Adjustment' }))
    await waitFor(() => {
      expect(calls).toBe(1)
    })

    // The first success closes the dialog (parent contract); simulate a second
    // intent on the still-mounted form by entering a new amount and retrying.
    await user.clear(getByLabelText('Amount (credits)'))
    await user.type(getByLabelText('Amount (credits)'), '200')
    await user.click(getByRole('button', { name: 'Confirm Adjustment' }))

    await waitFor(() => {
      expect(calls).toBe(2)
    })
    expect(keys[1]).not.toBe(keys[0])
  })
})

describe('AdjustQuotaDialog confirm-stage errors (I5)', () => {
  it('renders the failure while the confirmation step is active', async () => {
    installDetail()
    server.use(
      http.post(ADJUST_URL, () =>
        HttpResponse.json({ type: 'validation', title: 'nope' }, { status: 400 }),
      ),
    )

    const { getByLabelText, getByRole, findByRole, findByText, user } =
      await renderDialog()

    await user.click(getByRole('radio', { name: 'Subtract' }))
    await user.type(getByLabelText('Amount (credits)'), '100')
    await user.click(getByRole('button', { name: 'Confirm Adjustment' }))

    // The confirmation copy is on screen, and the failure must be visible next
    // to it (previously the alert lived only in the form branch).
    expect(await findByText(/reducing alice's balance by 100 credits/)).toBeTruthy()
    const confirmButton = getByRole('button', { name: 'Confirm Adjustment' })
    await user.click(confirmButton)

    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('Could not adjust the quota')
    // Still on the confirm step, so the message is not hidden by a branch swap.
    expect(getByRole('button', { name: 'Cancel' })).toBeTruthy()
  })
})
