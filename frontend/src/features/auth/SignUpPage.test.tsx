import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SignUpPage } from './SignUpPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  // AuthHallShell 的共享页脚会探测会话；访客答案 401，页面不展示它。
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ type: 'unauthorized', title: 'Unauthorized' }, { status: 401 }),
    ),
  )
})
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

describe('SignUpPage submit errors', { timeout: 20000 }, () => {
  async function fillForm() {
    // Typing six fields through the real event pipeline is slower than the
    // 5s default; the timer delay adds nothing to what these cases assert.
    const user = userEvent.setup({ delay: null })
    const result = await renderWithProviders(
      <SignUpPage onNavigate={() => {}} />,
      ['common', 'auth'],
    )

    await user.type(await result.findByLabelText('Username'), 'tomleo02')
    await user.type(result.getByLabelText('Email'), 'fresh@example.com')
    await user.type(result.getByLabelText('Verification code'), '123456')
    await user.type(result.getByLabelText('Password'), 'Str0ngPass!2026')
    await user.type(result.getByLabelText('Confirm password'), 'Str0ngPass!2026')
    await user.click(result.getByRole('checkbox'))
    return { user, result }
  }

  it('explains an expired verification code instead of the generic failure', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
      http.post(`${ORIGIN}/api/auth/register`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/bad_request',
            title: 'Invalid or expired verification code',
            status: 400,
          },
          { status: 400 },
        ),
      ),
    )

    const { user, result } = await fillForm()
    await user.click(result.getByRole('button', { name: 'Create account' }))

    expect(
      await result.findByText(
        'The verification code is invalid or has expired. Request a new one.',
      ),
    ).toBeTruthy()
  })

  it('keeps the generic message for other 400s', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
      http.post(`${ORIGIN}/api/auth/register`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/bad_request',
            title: 'Password is too weak or has been compromised',
            status: 400,
          },
          { status: 400 },
        ),
      ),
    )

    const { user, result } = await fillForm()
    await user.click(result.getByRole('button', { name: 'Create account' }))

    expect(await result.findByText('Registration failed. Please try again.')).toBeTruthy()
  })
})
