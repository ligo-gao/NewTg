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

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => clearLoginEncryptionKeyCache())
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

async function submitSignIn(redirect: string, onNavigate: (to: string) => void) {
  const { getByLabelText, getByRole } = await renderWithProviders(
    <SignInPage redirect={redirect} onNavigate={onNavigate} />,
    ['common', 'auth'],
  )
  const user = userEvent.setup()
  await user.type(getByLabelText('Username or email'), 'alice')
  await user.type(getByLabelText('Password'), 'correct horse')
  await user.click(getByRole('button', { name: 'Sign in' }))
  return { user, getByLabelText, getByRole }
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

    const { getByLabelText, getByRole, findByRole } = await renderWithProviders(
      <SignInPage onNavigate={() => {}} />,
      ['common', 'auth'],
    )
    const user = userEvent.setup()
    await user.type(getByLabelText('Username or email'), 'alice')
    await user.type(getByLabelText('Password'), 'correct horse')
    await user.click(getByRole('button', { name: 'Sign in' }))

    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('temporarily unavailable')
    expect(loginCalls).toBe(0)
  })
})
