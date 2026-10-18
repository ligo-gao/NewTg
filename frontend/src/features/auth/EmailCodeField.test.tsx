import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { EmailCodeField } from './EmailCodeField'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('EmailCodeField countdown', () => {
  it('starts the register countdown from the purpose default (60s)', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => new HttpResponse(null, { status: 204 })),
    )

    const { user, getByRole } = await renderEmailField('register', 60)
    await user.click(getByRole('button', { name: 'Send code' }))

    const resend = await getByRole('button', { name: 'Resend in 60s' })
    expect((resend as HTMLButtonElement).disabled).toBe(true)
  })

  it('uses the provided cooldown override instead of the purpose default', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => new HttpResponse(null, { status: 204 })),
    )

    const { user, getByRole } = await renderEmailField('reset', 30)
    await user.click(getByRole('button', { name: 'Send code' }))

    const resend = await getByRole('button', { name: 'Resend in 30s' })
    expect((resend as HTMLButtonElement).disabled).toBe(true)
  })

  it('defaults every purpose to the 60s server cooldown', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => new HttpResponse(null, { status: 204 })),
    )

    // No explicit override: the reset purpose must use the server's 60s.
    const result = await renderWithProviders(
      <EmailCodeField
        purpose="reset"
        email="alice@example.com"
        value=""
        onChange={() => {}}
      />,
      ['common', 'auth'],
    )
    const user = userEvent.setup()
    await user.click(result.getByRole('button', { name: 'Send code' }))

    const resend = await result.getByRole('button', { name: 'Resend in 60s' })
    expect((resend as HTMLButtonElement).disabled).toBe(true)
  })

  it('obeys the server Retry-After value on a 429', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () =>
        HttpResponse.json(
          { type: 'rate_limited', title: 'Too many requests' },
          { status: 429, headers: { 'Retry-After': '45' } },
        ),
      ),
    )

    const { user, getByRole } = await renderEmailField('register', 60)
    await user.click(getByRole('button', { name: 'Send code' }))

    // 45 comes from the server, not from the 60s purpose default.
    const resend = await getByRole('button', { name: 'Resend in 45s' })
    expect((resend as HTMLButtonElement).disabled).toBe(true)
  })

  it('keeps the code input numeric and reports changes', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => new HttpResponse(null, { status: 204 })),
    )

    let value = ''
    function Harness() {
      const [current, setCurrent] = useState('')
      value = current
      return (
        <EmailCodeField
          purpose="register"
          email="alice@example.com"
          value={current}
          onChange={setCurrent}
        />
      )
    }

    const { getByLabelText } = await renderWithProviders(<Harness />, [
      'common',
      'auth',
    ])
    const user = userEvent.setup()

    const input = getByLabelText('Verification code')
    expect(input.getAttribute('inputmode')).toBe('numeric')
    expect(input.getAttribute('autocomplete')).toBe('one-time-code')

    await user.type(input, '123456')
    expect(value).toBe('123456')
  })
})

async function renderEmailField(
  purpose: 'register' | 'reset',
  cooldownSeconds: number,
) {
  const result = await renderWithProviders(
    <EmailCodeField
      purpose={purpose}
      email="alice@example.com"
      value=""
      onChange={() => {}}
      cooldownSeconds={cooldownSeconds}
    />,
    ['common', 'auth'],
  )
  return { ...result, user: userEvent.setup() }
}
