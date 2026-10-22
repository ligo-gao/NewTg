import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
} from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import {
  acknowledgeCompliance,
  clearComplianceAcknowledgement,
} from '@/features/account/compliance/compliance-session'

import { PaymentConfirmDialog } from './payment-confirm-dialog'
import { useState } from 'react'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => clearComplianceAcknowledgement())
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
 * G-M6C-19: the order endpoint is never called before the viewer acknowledges
 * the compliance gate, and the acknowledgement suppresses the gate for later
 * payments in the same tab session.
 */
describe('PaymentConfirmDialog compliance gate', () => {
  it('blocks the order until acknowledged and then skips the gate', async () => {
    let posts = 0
    server.use(
      http.post(`${ORIGIN}/api/orders`, () => {
        posts += 1
        return HttpResponse.json(
          { type: 'about:blank', title: 'Channel unavailable' },
          { status: 502 },
        )
      }),
    )

    await renderWithProviders(<Harness />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))
    const gate = screen.getByRole('alertdialog')
    expect(gate).toBeTruthy()
    expect(posts).toBe(0)

    const continueButton = within(gate).getByRole('button', {
      name: 'Agree & Continue',
    })
    expect((continueButton as HTMLButtonElement).disabled).toBe(true)

    // Cancelling the gate keeps the confirmation dialog open and sends nothing.
    await user.click(within(gate).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(screen.getByText('Confirm Payment')).toBeTruthy()
    expect(posts).toBe(0)

    // A second attempt asks again, then submits once acknowledged.
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))
    const reopened = screen.getByRole('alertdialog')
    await user.click(
      within(reopened).getByRole('checkbox', {
        name: /I have read and agree/i,
      }),
    )
    await user.click(
      within(reopened).getByRole('button', { name: 'Agree & Continue' }),
    )
    await waitFor(() => expect(posts).toBe(1))

    // Same session: the gate is skipped for the retry of this intent.
    await user.click(screen.getByRole('button', { name: 'Confirm & Pay' }))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    await waitFor(() => expect(posts).toBe(2))
  })
})

/**
 * F-04-10: one UUID per dialog instance. Retrying the same confirm dialog must
 * replay the original key (server-side idempotency), while a fresh dialog is a
 * new user intent and must mint a new key.
 */
describe('PaymentConfirmDialog idempotency key', () => {
  it('reuses the key on retry and mints a new one for a fresh dialog', async () => {
    acknowledgeCompliance()
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
