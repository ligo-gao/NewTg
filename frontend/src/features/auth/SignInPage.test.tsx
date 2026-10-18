import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { clearLoginEncryptionKeyCache } from '@/lib/password-encryption'
import { generateLoginKey } from '@/test-support/crypto'
import { renderWithProviders } from '@/test-support/providers'

import { SignInPage } from './SignInPage'

const ORIGIN = 'http://localhost:3000'
const KEY_URL = `${ORIGIN}/api/auth/login/encryption-key`
const LOGIN_URL = `${ORIGIN}/api/auth/login`

const server = setupServer()

const PUBLIC_STATUS = {
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

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  clearLoginEncryptionKeyCache()
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    // The page probes for an existing session; logged-out tests receive 401.
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ type: 'unauthorized', title: 'Unauthorized' }, { status: 401 }),
    ),
  )
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

async function submitSignIn(redirect: string, onNavigate: (to: string) => void) {
  const result = await renderWithProviders(
    <SignInPage redirect={redirect} onNavigate={onNavigate} />,
    ['common', 'auth'],
  )
  const user = userEvent.setup()
  // The page holds the form behind the public-status query; wait for it.
  const identifierInput = await result.findByLabelText('Username or email')
  await user.type(identifierInput, 'alice')
  await user.type(result.getByLabelText('Password'), 'correct horse')
  await user.click(result.getByRole('button', { name: 'Sign in' }))
  return { ...result, user }
}

describe('SignInPage submit path', () => {
  it('encrypts the password with the v2 envelope and applies sanitizeAuthRedirect', async () => {
    const key = await generateLoginKey('kid-signin')
    let loginBody: Record<string, unknown> | undefined

    server.use(
      http.get(KEY_URL, () =>
        HttpResponse.json({ kid: key.kid, public_key: key.publicKeyPEM }),
      ),
      http.post(LOGIN_URL, async ({ request }) => {
        loginBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          user: {
            id: 1,
            username: 'alice',
            role: 'user',
            email_verified: true,
            must_change_password: false,
          },
        })
      }),
    )

    const onNavigate = vi.fn()
    // Protocol-relative redirects must be rejected by sanitizeAuthRedirect.
    await submitSignIn('//evil.example/steal', onNavigate)

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/dashboard')
    })

    expect(loginBody?.identifier).toBe('alice')
    expect(loginBody?.encryption_key_id).toBe(key.kid)
    expect(String(loginBody?.password_encrypted)).toMatch(/^v2\./)
    expect(loginBody).not.toHaveProperty('password')
  })

  it('sends plaintext only when the server reports encryption disabled', async () => {
    let loginBody: Record<string, unknown> | undefined
    server.use(
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
      http.post(LOGIN_URL, async ({ request }) => {
        loginBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          user: {
            id: 1,
            username: 'alice',
            role: 'user',
            email_verified: true,
            must_change_password: false,
          },
        })
      }),
    )

    const onNavigate = vi.fn()
    await submitSignIn('/profile?tab=security', onNavigate)

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/profile?tab=security')
    })
    expect(loginBody?.password).toBe('correct horse')
    expect(loginBody).not.toHaveProperty('password_encrypted')
  })

  it('routes a pending_2fa response to the challenge page with the ticket', async () => {
    server.use(
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
      http.post(LOGIN_URL, () =>
        HttpResponse.json({ status: 'pending_2fa', ticket: 'ticket-42' }),
      ),
    )

    const onNavigate = vi.fn()
    await submitSignIn('/profile', onNavigate)

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith(
        '/two-factor-challenge?ticket=ticket-42&redirect=%2Fprofile',
      )
    })
  })

  it('forces a password change when the session is restricted', async () => {
    server.use(
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
      http.post(LOGIN_URL, () =>
        HttpResponse.json({
          user: {
            id: 7,
            username: 'temp',
            role: 'user',
            email_verified: true,
            must_change_password: true,
          },
        }),
      ),
    )

    const onNavigate = vi.fn()
    await submitSignIn('/dashboard', onNavigate)

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/security?force=password')
    })
  })

  it('never falls back to plaintext when the key endpoint fails', async () => {
    let loginCalls = 0
    server.use(
      http.get(KEY_URL, () =>
        HttpResponse.json({ title: 'unavailable' }, { status: 503 }),
      ),
      http.post(LOGIN_URL, () => {
        loginCalls += 1
        return HttpResponse.json({})
      }),
    )

    const { getByLabelText, getByRole, findByRole, findByLabelText } =
      await renderWithProviders(<SignInPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])
    const user = userEvent.setup()
    await user.type(await findByLabelText('Username or email'), 'alice')
    await user.type(getByLabelText('Password'), 'correct horse')
    await user.click(getByRole('button', { name: 'Sign in' }))

    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('temporarily unavailable')
    expect(loginCalls).toBe(0)
  })

  it('disables the submit button for the server Retry-After window', async () => {
    server.use(
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
      http.post(LOGIN_URL, () =>
        HttpResponse.json(
          { type: 'rate_limited', title: 'Too many requests' },
          { status: 429, headers: { 'Retry-After': '30' } },
        ),
      ),
    )

    const { getByRole, findByRole } = await submitSignIn('/dashboard', vi.fn())

    const button = await getByRole('button', { name: 'Try again in 30s' })
    expect((button as HTMLButtonElement).disabled).toBe(true)
    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('Too many attempts')
  })

  it('redirects an already-authenticated visitor to the dashboard', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ username: 'alice', role: 'user', status: 'enabled' }),
      ),
    )

    const onNavigate = vi.fn()
    await renderWithProviders(<SignInPage onNavigate={onNavigate} />, [
      'common',
      'auth',
    ])

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/dashboard')
    })
  })
})
