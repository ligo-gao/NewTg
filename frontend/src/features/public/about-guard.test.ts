import { QueryClient } from '@tanstack/react-query'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { aboutAccessRedirect } from './about-guard'

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

const SIGNED_IN = {
  username: 'alice',
  email: null,
  email_verified: true,
  role: 'user',
  status: 'enabled',
  avatar_url: null,
  display_name: null,
  created_at: '2026-01-01T00:00:00Z',
  permissions: [],
}

describe('about route guard', () => {
  it('silently redirects home when the about page is disabled', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ about: { enabled: false, require_auth: false } }),
        ),
      ),
    )
    expect(await aboutAccessRedirect(queryClient(), '/about', '?tab=oss')).toBe('/')
  })

  it('redirects guests to sign-in when require_auth is on', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ about: { enabled: true, require_auth: true } }),
        ),
      ),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )
    expect(await aboutAccessRedirect(queryClient(), '/about', '?tab=oss')).toBe(
      '/sign-in?redirect=%2Fabout%3Ftab%3Doss',
    )
  })

  it('allows the visitor when require_auth is off', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ about: { enabled: true, require_auth: false } }),
        ),
      ),
    )
    expect(await aboutAccessRedirect(queryClient(), '/about', '')).toBeNull()
  })

  it('passes a signed-in visitor straight through', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(
          statusFixture({ about: { enabled: true, require_auth: true } }),
        ),
      ),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(SIGNED_IN)),
    )
    expect(await aboutAccessRedirect(queryClient(), '/about', '')).toBeNull()
  })

  it('fails open when /api/status is unavailable', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    )
    expect(await aboutAccessRedirect(queryClient(), '/about', '')).toBeNull()
  })
})
