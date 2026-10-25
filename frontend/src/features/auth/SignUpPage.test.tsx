import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SignUpPage } from './SignUpPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function statusBody(signupEnabled: boolean) {
  return {
    auth: {
      signup_enabled: signupEnabled,
      password_login_enabled: true,
      email_verification_required: true,
      turnstile: { enabled: false, site_key: '' },
    },
    password_login_encryption_enabled: false,
    languages: ['en'],
    default_language: 'en',
  }
}

describe('SignUpPage hall', () => {
  it('renders the brand statement and line fields, with the switch under the form', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
    )

    const result = await renderWithProviders(
      <SignUpPage onNavigate={() => {}} />,
      ['common', 'auth'],
    )

    const username = await result.findByLabelText('Username')
    expect(result.container.querySelector('.auth-hall')).not.toBeNull()
    expect(username.closest('[data-variant="line"]')).not.toBeNull()
    expect(result.getByText('Every model,')).toBeTruthy()

    const stage = result.container.querySelector('.auth-hall-stage')
    expect(stage?.getAttribute('aria-hidden')).toBe('true')
    expect(stage?.querySelectorAll('.auth-hall-clients li')).toHaveLength(6)
    expect(stage?.textContent).toBe('')

    // The resend control joins the code field's label row instead of sitting
    // beside the input, so the column keeps one rhythm.
    const code = result.getByLabelText('Verification code')
    expect(code.closest('[data-variant="line"]')?.contains(
      result.getByRole('button', { name: 'Send code' }),
    )).toBe(true)

    const signIn = result.getByRole('link', { name: 'Sign in' })
    expect(signIn.getAttribute('href')).toBe('/sign-in')
  })
})

describe('SignUpPage status gate', () => {
  it('does not render the form before /api/status resolves', async () => {
    let release: (() => void) | undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })

    server.use(
      http.get(`${ORIGIN}/api/status`, async () => {
        await gate
        return HttpResponse.json(statusBody(true))
      }),
    )

    const result = await renderWithProviders(
      <SignUpPage onNavigate={() => {}} />,
      ['common', 'auth'],
    )

    // Rendering the form early would let a fast submit omit `email_code`.
    expect(result.queryByLabelText('Username')).toBeNull()
    release?.()

    expect(await result.findByLabelText('Username')).toBeTruthy()
  })

  it('links a disabled registration page to /sign-in', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(false))),
    )

    const result = await renderWithProviders(
      <SignUpPage onNavigate={() => {}} />,
      ['common', 'auth'],
    )

    const link = await result.findByRole('link', { name: /Sign in to/ })
    expect(link.getAttribute('href')).toBe('/sign-in')
  })
})
