import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PaymentConfirmDialog } from './payment-confirm-dialog'
import { useState } from 'react'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function Harness() {
  const [open, setOpen] = useState(true)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open confirm
      </button>
      <PaymentConfirmDialog
        open={open}
        amount={1000}
        provider="alipay"
        onOpenChange={setOpen}
        onCreated={() => undefined}
      />
    </>
  )
}

/**
 * F-04-10: one UUID per dialog instance. Retrying the same confirm dialog must
 * replay the original key (server-side idempotency), while a fresh dialog is a
 * new user intent and must mint a new key.
 */
describe('PaymentConfirmDialog idempotency key', () => {
  it('reuses the key on retry and mints a new one for a fresh dialog', async () => {
    const keys: string[] = []
    server.use(
      http.post(`${ORIGIN}/api/orders`, ({ request }) => {
        const key = request.headers.get('idempotency-key')
        if (key !== null) keys.push(key)
        return HttpResponse.json(
          { type: 'about:blank', title: 'Channel unavailable' },
          { status: 502 },
        )
      }),
    )

    await renderWithProviders(<Harness />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))
    await waitFor(() => expect(keys).toHaveLength(1))

    // Retrying the same dialog reuses the key.
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))
    await waitFor(() => expect(keys).toHaveLength(2))
    expect(keys[1]).toBe(keys[0])

    // Closing and reopening is a fresh intent: a different key.
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await user.click(screen.getByRole('button', { name: 'Open confirm' }))
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))
    await waitFor(() => expect(keys).toHaveLength(3))
    expect(keys[2]).not.toBe(keys[0])
  })
})
