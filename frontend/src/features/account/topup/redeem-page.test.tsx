import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { ToastViewport } from '@/shared/ui/toast'

import { RedeemPage } from './redeem-page'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function renderRedeem(onBalance = vi.fn()) {
  return renderWithProviders(
    <>
      <RedeemPage onBalance={onBalance} />
      <ToastViewport />
    </>,
    ['common', 'account'],
  )
}

function problem(type: string, title: string, status: number) {
  return HttpResponse.json(
    { type: `https://newtg.dev/problems/${type}`, title, detail: title },
    { status },
  )
}

describe('RedeemPage', () => {
  it('redeems a code, reports the credited amount and clears the input', async () => {
    const onBalance = vi.fn()
    server.use(
      http.post(`${ORIGIN}/api/redemption-codes/redeem`, () =>
        HttpResponse.json({ credits: 1000, balance_after: 1500 }),
      ),
    )
    await renderRedeem(onBalance)
    const user = userEvent.setup()

    const input = screen.getByLabelText('Redemption Code')
    await user.type(input, ' ABCD-1234 ')
    await user.click(screen.getByRole('button', { name: 'Redeem' }))

    expect(
      await screen.findByText('Redeemed successfully. 1,000 credits added.'),
    ).toBeTruthy()
    expect(onBalance).toHaveBeenCalledWith(1500)
    expect((input as HTMLInputElement).value).toBe('')
  })

  it.each([
    ['redemption_code_invalid', 400, 'Invalid redemption code.'],
    ['redemption_code_used', 409, 'This code has already been redeemed.'],
    ['redemption_code_expired', 410, 'This code has expired.'],
    ['redemption_code_disabled', 409, 'This code has been disabled.'],
  ])('distinguishes %s', async (type, status, message) => {
    server.use(
      http.post(`${ORIGIN}/api/redemption-codes/redeem`, () =>
        problem(type, 'Redemption failed', status),
      ),
    )
    await renderRedeem()
    const user = userEvent.setup()

    const input = screen.getByLabelText('Redemption Code')
    await user.type(input, 'BAD-CODE')
    await user.click(screen.getByRole('button', { name: 'Redeem' }))

    expect(await screen.findByText(message)).toBeTruthy()
    // The input keeps the typed value so the user can correct it.
    expect((input as HTMLInputElement).value).toBe('BAD-CODE')
  })

  it('surfaces the 10/min rate limit message', async () => {
    server.use(
      http.post(`${ORIGIN}/api/redemption-codes/redeem`, () =>
        problem('rate_limited', 'Too many requests', 429),
      ),
    )
    await renderRedeem()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Redemption Code'), 'CODE')
    await user.click(screen.getByRole('button', { name: 'Redeem' }))

    expect(
      await screen.findByText('Too many attempts. Please try again later.'),
    ).toBeTruthy()
  })

  it('trims the code before submitting', async () => {
    let submitted: unknown
    server.use(
      http.post(`${ORIGIN}/api/redemption-codes/redeem`, async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ credits: 1, balance_after: 1 })
      }),
    )
    await renderRedeem()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Redemption Code'), '  CODE  ')
    await user.click(screen.getByRole('button', { name: 'Redeem' }))

    await waitFor(() => {
      expect(submitted).toEqual({ code: 'CODE' })
    })
  })
})
