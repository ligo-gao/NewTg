import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ForgotPasswordPage } from './ForgotPasswordPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  server.use(
    http.get(`${ORIGIN}/api/status`, () =>
      HttpResponse.json({
        system_initialized: true,
        auth: {
          signup_enabled: true,
          password_login_enabled: true,
          email_verification_required: false,
          turnstile: { enabled: false, site_key: '' },
        },
        password_login_encryption_enabled: false,
        languages: ['en'],
        default_language: 'en',
      }),
    ),
  )
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('ForgotPasswordPage resend cooldown (I11)', () => {
  it('starts the resend countdown at the server 60s default', async () => {
    server.use(
      http.post(
        `${ORIGIN}/api/auth/password-reset/request`,
        () => new HttpResponse(null, { status: 204 }),
      ),
    )

    const { getByLabelText, getByRole, findByRole, user } = {
      ...(await renderWithProviders(<ForgotPasswordPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(getByLabelText('Email'), 'alice@example.com')
    await user.click(getByRole('button', { name: 'Send reset email' }))

    const resend = await findByRole('button', { name: 'Resend in 60s' })
    expect((resend as HTMLButtonElement).disabled).toBe(true)
  })
})
