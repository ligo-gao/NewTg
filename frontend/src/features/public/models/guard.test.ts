import { QueryClient } from '@tanstack/react-query'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { modelsAccessRedirect } from './guard'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function statusFixture(nav: Record<string, unknown>) {
  return {
    system_initialized: true,
    auth: {
      signup_enabled: true,
      password_login_enabled: true,
      email_verification_required: false,
      turnstile: { enabled: false, site_key: '' },
    },
    languages: ['en'],
    default_language: 'en',
    nav,
  }
}

function queryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

describe('models route guard', () => {
  it('silently redirects home when the model library is disabled', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ models: { enabled: false, require_auth: false } }),
        ),
      ),
    )
    expect(await modelsAccessRedirect(queryClient(), '/models', '?cap=vision')).toBe(
      '/',
    )
  })

  it('redirects guests to sign-in when require_auth is on', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ models: { enabled: true, require_auth: true } }),
        ),
      ),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )
    expect(await modelsAccessRedirect(queryClient(), '/models', '?cap=vision')).toBe(
      '/sign-in?redirect=%2Fmodels%3Fcap%3Dvision',
    )
  })

  it('allows the visitor when require_auth is off', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ models: { enabled: true, require_auth: false } }),
        ),
      ),
    )
    expect(await modelsAccessRedirect(queryClient(), '/models', '')).toBeNull()
  })

  it('fails open when /api/status is unavailable', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    )
    expect(await modelsAccessRedirect(queryClient(), '/models', '')).toBeNull()
  })
})
