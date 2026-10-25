import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { within } from '@testing-library/react'

import { renderWithProviders } from '@/test-support/providers'

import { PublicFooter } from './public-footer'
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
    rankings: { enabled: false, require_auth: false },
    docs: { enabled: true, require_auth: false },
    about: { enabled: true, require_auth: false },
  },
  billing: { quota_per_unit: 500000, display_unit: 1, currency_symbol: '$' },
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('PublicLayout shell (F-09-34/37)', () => {
  it('wires the operator nav, marks the active page and hides disabled entries', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )

    const { findByRole, getByRole, queryByRole } = await renderWithProviders(
      <PublicLayout activePath="/about">
        <p>content</p>
      </PublicLayout>,
      ['public'],
    )

    const nav = await findByRole('navigation', { name: 'Public pages' })
    expect(nav.textContent).toContain('Models')
    expect(nav.textContent).toContain('Pricing')
    expect(nav.textContent).toContain('About')
    // The brand mark is the home link; the header no longer lists Home.
    expect(nav.textContent).not.toContain('Home')
    // rankings.enabled=false — hidden without leaving an empty slot.
    expect(nav.textContent).not.toContain('Rankings')
    // console.require_auth is locked true and this visitor is a guest.
    expect(nav.textContent).not.toContain('Console')

    const about = within(nav).getByRole('link', { name: 'About' })
    expect(about.getAttribute('href')).toBe('/about')
    expect(about.getAttribute('aria-current')).toBe('page')
    expect(getByRole('link', { name: 'Sign in' }).getAttribute('href')).toBe(
      '/sign-in?redirect=%2Fabout',
    )

    // Footer ships in the same shell.
    expect(queryByRole('contentinfo')).not.toBeNull()
  })

  it('renders the docs entry as an external link with rel protection', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )

    const { findAllByRole } = await renderWithProviders(
      <PublicLayout activePath="/">content</PublicLayout>,
      ['public'],
    )

    const docsLinks = await findAllByRole('link', { name: /Docs \(Opens in a new tab\)/ })
    expect(docsLinks.length).toBeGreaterThan(0)
    for (const docs of docsLinks) {
      expect(docs.getAttribute('href')).toBe('https://docs.example.com')
      expect(docs.getAttribute('target')).toBe('_blank')
      expect(docs.getAttribute('rel')).toBe('noopener noreferrer')
    }
  })
})

describe('PublicFooter URL safety (F-09-37)', () => {
  it('never renders javascript:/data:/protocol-relative targets from site config', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({
          ...PUBLIC_STATUS,
          site: {
            ...PUBLIC_STATUS.site,
            docs_url: 'javascript:alert(1)',
            status_page_url: 'data:text/html,hi',
          },
        }),
      ),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )

    const { findByRole, container } = await renderWithProviders(<PublicFooter />, ['public'])
    await findByRole('contentinfo')

    expect(container.querySelector('a[href^="javascript:"]')).toBeNull()
    expect(container.querySelector('a[href^="data:"]')).toBeNull()
    expect(container.querySelector('a[href="//evil.example"]')).toBeNull()
  })
})
