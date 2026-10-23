import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { ProfilePage } from './ProfilePage'
import { renderWithProviders } from '@/test-support/providers'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const ME = {
  username: 'alice',
  display_name: 'Alice',
  email: 'alice@example.com',
  email_verified: true,
  role: 'user',
  status: 'enabled',
  avatar_url: null,
  created_at: '2026-03-12T00:00:00Z',
  permissions: [],
}

const SELF = {
  language: 'en',
  sidebar_modules: null,
  record_ip_log: true,
  default_model: 'gpt-4o',
  updated_at: '2026-10-23T10:00:00Z',
}

const WALLET = {
  balance: 12840,
  held_credits: 0,
  total_consumed: 348210,
  total_requests: 128430,
  threshold: 1000,
  health: 'healthy',
  group: 'default',
}

const CONFIG = {
  amount_options: [1000, 5000],
  min_topup: 1000,
  max_topup: 500000,
  quota_per_unit: 100,
  credits_ratio: 1,
  enabled_providers: ['stripe'],
  providers: { stripe: { publishable_key: 'pk_test', min_topup: 1000 } },
}

const STATUS = {
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
  nav: { modules: { chat: { playground: true } } },
  usable_groups: [],
}

function baseHandlers(getMe: () => Record<string, unknown>) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(getMe())),
    http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(SELF)),
    http.get(`${ORIGIN}/api/wallet/summary`, () => HttpResponse.json(WALLET)),
    http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(CONFIG)),
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
    http.get(`${ORIGIN}/api/tokens`, () =>
      HttpResponse.json({ items: [], total: 4, page: 1, page_size: 1 }),
    ),
    http.get(`${ORIGIN}/api/sessions`, () => HttpResponse.json({ sessions: [] })),
    http.get(`${ORIGIN}/api/notification-preferences`, () => HttpResponse.json({})),
  )
}

describe('ProfilePage account center', () => {
  it('renders identity, overview and account settings', async () => {
    baseHandlers(() => ME)
    await renderWithProviders(<ProfilePage />, ['common', 'account'])

    expect((await screen.findAllByText('@alice')).length).toBeGreaterThan(0)
    expect(screen.getAllByText('alice@example.com').length).toBeGreaterThan(0)
    expect(screen.getAllByText('¥128.40').length).toBeGreaterThan(0)
    expect(screen.getAllByText('128,430').length).toBeGreaterThan(0)
    expect(await screen.findByText('4 enabled')).toBeTruthy()
  })

  it('saves a new display name through PATCH /api/me', async () => {
    let current = { ...ME }
    let patched: unknown = null
    baseHandlers(() => current)
    server.use(
      http.patch(`${ORIGIN}/api/me`, async ({ request }) => {
        patched = await request.json()
        current = { ...current, display_name: 'Alice Cooper' }
        return HttpResponse.json(current)
      }),
    )
    await renderWithProviders(<ProfilePage />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    const input = await screen.findByLabelText('Display name')
    await user.clear(input)
    await user.type(input, 'Alice Cooper')
    await user.click(await screen.findByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(patched).toEqual({ display_name: 'Alice Cooper' })
    })
  })
})
