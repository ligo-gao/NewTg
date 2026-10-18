import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { act, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { resetTurnstileLoader, type TurnstileRenderOptions } from '@/lib/turnstile'
import { renderWithProviders } from '@/test-support/providers'

import { EmailCodeField } from './EmailCodeField'
import { ForgotPasswordPage } from './ForgotPasswordPage'
import { SignInPage } from './SignInPage'
import { SignUpPage } from './SignUpPage'
import { TurnstileWidget } from './TurnstileWidget'

const ORIGIN = 'http://localhost:3000'
const KEY_URL = `${ORIGIN}/api/auth/login/encryption-key`
const LOGIN_URL = `${ORIGIN}/api/auth/login`

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  delete window.turnstile
  resetTurnstileLoader()
  document
    .querySelectorAll('script[src*="challenges.cloudflare.com"]')
    .forEach((script) => script.remove())
})
afterAll(() => server.close())

function statusBody(turnstileEnabled: boolean) {
  return {
    system_initialized: true,
    auth: {
      signup_enabled: true,
      password_login_enabled: true,
      email_verification_required: false,
      turnstile: {
        enabled: turnstileEnabled,
        site_key: turnstileEnabled ? 'site-key-test' : '',
      },
    },
    password_login_encryption_enabled: false,
    languages: ['en'],
    default_language: 'en',
  }
}

function guestMe() {
  return http.get(`${ORIGIN}/api/me`, () =>
    HttpResponse.json(
      { type: 'unauthorized', title: 'Unauthorized' },
      { status: 401 },
    ),
  )
}

/**
 * Installs a turnstile API that behaves like the real widget: `render` returns
 * a widget id and (optionally) hands the page a solved token. `deliver=false`
 * simulates a challenge the visitor never completed.
 */
function installTurnstileMock(deliver = true) {
  const render = vi.fn((_container: HTMLElement, options: TurnstileRenderOptions) => {
    if (deliver) options.callback('turnstile-token-1')
    return 'widget-1'
  })
  const reset = vi.fn()
  const remove = vi.fn()
  window.turnstile = { render, reset, remove }
  return { render, reset, remove }
}

async function waitForWidget(mock: { render: ReturnType<typeof vi.fn> }) {
  await waitFor(() => {
    expect(mock.render).toHaveBeenCalled()
  })
}

describe('Turnstile disabled', () => {
  it('renders no widget and sends no token', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(false))),
      guestMe(),
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
    )
    let loginBody: Record<string, unknown> | undefined
    server.use(
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

    const { findByLabelText, getByLabelText, getByRole, user } = {
      ...(await renderWithProviders(<SignInPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(await findByLabelText('Username or email'), 'alice')
    await user.type(getByLabelText('Password'), 'correct horse')
    await user.click(getByRole('button', { name: 'Sign in' }))

    await waitFor(() => {
      expect(loginBody).toBeDefined()
    })
    expect(loginBody).not.toHaveProperty('turnstile_token')
    expect(
      document.querySelector('script[src*="challenges.cloudflare.com"]'),
    ).toBeNull()
  })
})

describe('Turnstile enabled', () => {
  it('sends the token on sign-in', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
      guestMe(),
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
    )
    let loginBody: Record<string, unknown> | undefined
    server.use(
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

    const mock = installTurnstileMock()
    const { findByLabelText, getByLabelText, getByRole, user } = {
      ...(await renderWithProviders(<SignInPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(await findByLabelText('Username or email'), 'alice')
    await user.type(getByLabelText('Password'), 'correct horse')
    await waitForWidget(mock)
    await user.click(getByRole('button', { name: 'Sign in' }))

    await waitFor(() => {
      expect(loginBody?.turnstile_token).toBe('turnstile-token-1')
    })
    expect(mock.render).toHaveBeenCalledWith(
      expect.any(HTMLElement),
      expect.objectContaining({ sitekey: 'site-key-test' }),
    )
  })

  it('sends the token on registration', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
    )
    let registerBody: Record<string, unknown> | undefined
    server.use(
      http.post(`${ORIGIN}/api/auth/register`, async ({ request }) => {
        registerBody = (await request.json()) as Record<string, unknown>
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

    const mock = installTurnstileMock()
    const { findByLabelText, getByLabelText, getByRole, user } = {
      ...(await renderWithProviders(<SignUpPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(await findByLabelText('Username'), 'alice')
    await user.type(getByLabelText('Email'), 'alice@example.com')
    await user.type(getByLabelText('Password'), 'correct horse 1')
    await user.type(getByLabelText('Confirm password'), 'correct horse 1')
    await user.click(getByLabelText(/I agree to the/))
    await waitForWidget(mock)
    await user.click(getByRole('button', { name: 'Create account' }))

    await waitFor(() => {
      expect(registerBody?.turnstile_token).toBe('turnstile-token-1')
    })
  })

  it('sends the token with the email-code request', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
    )
    let codeBody: Record<string, unknown> | undefined
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, async ({ request }) => {
        codeBody = (await request.json()) as Record<string, unknown>
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const mock = installTurnstileMock()
    const { getByRole, user } = {
      ...(await renderWithProviders(
        <EmailCodeField
          purpose="register"
          email="alice@example.com"
          value=""
          onChange={() => {}}
        />,
        ['common', 'auth'],
      )),
      user: userEvent.setup(),
    }

    await waitForWidget(mock)
    await user.click(getByRole('button', { name: 'Send code' }))

    await waitFor(() => {
      expect(codeBody?.turnstile_token).toBe('turnstile-token-1')
    })
  })

  it('sends the token with the password-reset request', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
    )
    let resetBody: Record<string, unknown> | undefined
    server.use(
      http.post(`${ORIGIN}/api/auth/password-reset/request`, async ({ request }) => {
        resetBody = (await request.json()) as Record<string, unknown>
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const mock = installTurnstileMock()
    const { findByLabelText, getByRole, user } = {
      ...(await renderWithProviders(<ForgotPasswordPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    // The page now waits for /api/status before rendering its form.
    await user.type(await findByLabelText('Email'), 'alice@example.com')
    await waitForWidget(mock)
    await user.click(getByRole('button', { name: 'Send reset email' }))

    await waitFor(() => {
      expect(resetBody?.turnstile_token).toBe('turnstile-token-1')
    })
  })

  it('blocks sign-in until the challenge is solved', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(statusBody(true))),
      guestMe(),
      http.get(KEY_URL, () => HttpResponse.json({ enabled: false })),
    )
    let loginCalls = 0
    server.use(
      http.post(LOGIN_URL, () => {
        loginCalls += 1
        return HttpResponse.json({})
      }),
    )

    const mock = installTurnstileMock(false)
    const { findByLabelText, findByRole, getByLabelText, getByRole, user } = {
      ...(await renderWithProviders(<SignInPage onNavigate={() => {}} />, [
        'common',
        'auth',
      ])),
      user: userEvent.setup(),
    }

    await user.type(await findByLabelText('Username or email'), 'alice')
    await user.type(getByLabelText('Password'), 'correct horse')
    await waitForWidget(mock)
    await user.click(getByRole('button', { name: 'Sign in' }))

    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('Please complete the verification challenge')
    expect(loginCalls).toBe(0)
  })
})

describe('Turnstile widget failures (N6/N8)', () => {
  it('reports a script load failure and clears the token', async () => {
    const onToken = vi.fn()
    const { findByText } = await renderWithProviders(
      <TurnstileWidget siteKey="site-key-test" onToken={onToken} />,
      ['common', 'auth'],
    )

    const script = document.querySelector(
      'script[src*="challenges.cloudflare.com"]',
    )
    expect(script).not.toBeNull()
    script?.dispatchEvent(new Event('error'))

    expect(
      await findByText(
        'The verification challenge could not be loaded. Refresh the page and try again.',
      ),
    ).toBeTruthy()
    expect(onToken).toHaveBeenCalledWith(undefined)
  })

  it('reports a challenge error instead of silently clearing the token', async () => {
    let options: TurnstileRenderOptions | undefined
    window.turnstile = {
      render: vi.fn((_container: HTMLElement, renderOptions: TurnstileRenderOptions) => {
        options = renderOptions
        return 'widget-1'
      }),
      reset: vi.fn(),
      remove: vi.fn(),
    }
    const onToken = vi.fn()
    const { findByText } = await renderWithProviders(
      <TurnstileWidget siteKey="site-key-test" onToken={onToken} />,
      ['common', 'auth'],
    )

    await waitFor(() => {
      expect(options).toBeDefined()
    })
    act(() => {
      options?.['error-callback']?.()
    })

    expect(
      await findByText('Verification failed. Please try again.'),
    ).toBeTruthy()
    expect(onToken).toHaveBeenCalledWith(undefined)
  })
})
