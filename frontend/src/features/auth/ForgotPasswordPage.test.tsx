import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ForgotPasswordPage } from './ForgotPasswordPage'

const ORIGIN = 'http://localhost:3000'

const STATUS_BODY = {
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
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS_BODY)),
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

    const { findByLabelText, getByRole, findByRole, user } = {
      ...(await renderWithProviders(<ForgotPasswordPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(await findByLabelText('Email'), 'alice@example.com')
    await user.click(getByRole('button', { name: 'Send reset email' }))

    const resend = await findByRole('button', { name: 'Resend in 60s' })
    expect((resend as HTMLButtonElement).disabled).toBe(true)
  })

  it('disables the first request button while the 429 countdown runs', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/password-reset/request`, () =>
        HttpResponse.json(
          { type: 'rate_limited', title: 'Too many requests' },
          { status: 429, headers: { 'Retry-After': '30' } },
        ),
      ),
    )

    const { findByLabelText, getByRole, findByText, user } = {
      ...(await renderWithProviders(<ForgotPasswordPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(await findByLabelText('Email'), 'alice@example.com')
    await user.click(getByRole('button', { name: 'Send reset email' }))

    expect(await findByText('Too many requests. Please try again later.')).toBeTruthy()
    const button = getByRole('button', { name: 'Send reset email' })
    expect((button as HTMLButtonElement).disabled).toBe(true)
  })
})

describe('ForgotPasswordPage status gate (N5)', () => {
  it('shows a retryable panel when the public status is unavailable', async () => {
    let failing = true
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        failing
          ? HttpResponse.json({ title: 'unavailable' }, { status: 500 })
          : HttpResponse.json(STATUS_BODY),
      ),
    )

    const { findByLabelText, findByText, getByRole, queryByLabelText, user } = {
      ...(await renderWithProviders(<ForgotPasswordPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    expect(await findByText('Could not load reset settings.')).toBeTruthy()
    expect(queryByLabelText('Email')).toBeNull()

    failing = false
    await user.click(getByRole('button', { name: 'Retry' }))
    expect(await findByLabelText('Email')).toBeTruthy()
  })
})
