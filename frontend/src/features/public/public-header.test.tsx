import { fireEvent, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { THEME_STORAGE_KEY, ThemeProvider } from '@/app/providers/theme'
import { renderWithProviders } from '@/test-support/providers'

import { PublicHeader } from './public-header'
import { PublicLayout } from './public-layout'

const ORIGIN = 'http://localhost:3000'

const PUBLIC_STATUS = {
  system_initialized: true,
  auth: {
    signup_enabled: true,
    password_login_enabled: true,
    email_verification_required: false,
    turnstile: { enabled: false, site_key: '' },
  },
  password_login_encryption_enabled: false,
  languages: ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi'],
  default_language: 'en',
  site: {
    title: 'Acme Gateway',
    logo_url: '',
    server_address: 'https://api.example.com',
    docs_url: 'https://docs.example.com',
    footer_html: '',
    about_html: '',
    filing_no: '',
    status_page_url: '',
    seo: { description: 'Acme', og_image_url: '' },
    home_custom_enabled: false,
    home_blocks: { hero: true, live_feed: false, features: true, stats: true, onboarding: true },
  },
  nav: {
    home: { enabled: true, require_auth: false },
    console: { enabled: true, require_auth: true },
    pricing: { enabled: true, require_auth: false },
    rankings: { enabled: true, require_auth: false },
    docs: { enabled: true, require_auth: false },
    about: { enabled: true, require_auth: false },
  },
  billing: { quota_per_unit: 500000, display_unit: 1, currency_symbol: '$' },
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('PublicHeader (F-09-34)', () => {
  beforeEach(() => {
    globalThis.localStorage?.removeItem(THEME_STORAGE_KEY)
  })
  afterEach(() => {
    globalThis.localStorage?.removeItem(THEME_STORAGE_KEY)
    delete document.documentElement.dataset.theme
  })

  function guest(): void {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )
  }

  it('renders the approved nav items in order and no Home item', async () => {
    guest()

    const { findByRole } = await renderWithProviders(
      <PublicHeader activePath="/models" />,
      ['public'],
    )

    const nav = within(await findByRole('navigation', { name: 'Public pages' }))
    expect([...nav.queryAllByRole('link')].map((link) => link.textContent)).toEqual([
      'Models',
      'Pricing',
      'Rankings',
      'About',
      'Docs',
    ])
  })

  it('adds the console entry once a session exists', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({ id: 1, username: 'ada' })),
    )

    const { findByRole } = await renderWithProviders(
      <PublicHeader activePath="/dashboard" />,
      ['public'],
    )

    const nav = within(await findByRole('navigation', { name: 'Public pages' }))
    const links = nav.queryAllByRole('link')
    expect(links.map((link) => link.textContent)).toEqual([
      'Models',
      'Pricing',
      'Rankings',
      'About',
      'Docs',
      'Console',
    ])
    expect(links.at(-1)?.getAttribute('href')).toBe('/dashboard')
  })

  it('omits a disabled nav entry without leaving a gap', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({
          ...PUBLIC_STATUS,
          nav: { ...PUBLIC_STATUS.nav, about: { enabled: false, require_auth: false } },
        }),
      ),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )

    const { findByRole, getByText } = await renderWithProviders(
      <PublicLayout activePath="/about">
        <p>content</p>
      </PublicLayout>,
      ['public'],
    )

    const nav = within(await findByRole('navigation', { name: 'Public pages' }))
    expect(nav.queryByRole('link', { name: 'About' })).toBeNull()
    expect(getByText('content')).not.toBeNull()
  })

  it('toggles the theme from the header', async () => {
    guest()

    const { findByRole } = await renderWithProviders(
      <ThemeProvider>
        <PublicHeader activePath="/about" />
      </ThemeProvider>,
      ['public'],
    )

    fireEvent.click(await findByRole('button', { name: 'Switch to dark theme' }))

    await waitFor(() => {
      expect(document.documentElement.dataset.theme).toBe('dark')
    })
    expect(globalThis.localStorage?.getItem(THEME_STORAGE_KEY)).toBe('dark')
  })

  it('renders the sign-in and sign-up calls to action for a guest', async () => {
    guest()

    const { findByRole } = await renderWithProviders(
      <PublicHeader activePath="/about" />,
      ['public'],
    )

    const signIn = await findByRole('link', { name: 'Sign in' })
    expect(signIn.getAttribute('href')).toBe('/sign-in?redirect=%2Fabout')

    const cta = await findByRole('link', { name: 'Get started free' })
    expect(cta.getAttribute('href')).toBe('/sign-up')
  })
})
