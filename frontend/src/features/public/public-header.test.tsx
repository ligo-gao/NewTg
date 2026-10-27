import { fireEvent, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { THEME_STORAGE_KEY, ThemeProvider } from '@/app/providers/theme'
import { renderWithProviders } from '@/test-support/providers'

import { PublicHeader } from './public-header'
import { PublicLayout } from './public-layout'

const ORIGIN = 'http://localhost:3000'

const nativeMatchMedia = window.matchMedia

/**
 * jsdom has no `matchMedia`; the header test needs the narrow-panel fixture,
 * and the theme provider reads the colour-scheme query. Restored after each
 * case so neither test inherits the other's viewport.
 */
function installMatchMedia(matches: (query: string) => boolean): void {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      matches: matches(query),
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  })
}

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
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      writable: true,
      value: nativeMatchMedia,
    })
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
      'API Docs',
    ])
  })

  it('adds the console entry once a session exists', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({ id: 1, username: 'ada' })),
    )

    const { findByRole, queryByRole } = await renderWithProviders(
      <PublicHeader activePath="/dashboard" />,
      ['public'],
    )

    const nav = within(await findByRole('navigation', { name: 'Public pages' }))
    const links = nav.queryAllByRole('link')
    expect(links.map((link) => link.textContent)).toEqual([
      'Console',
      'Models',
      'Pricing',
      'Rankings',
      'About',
      'API Docs',
    ])
    expect(links.at(-1)?.getAttribute('href')).toBe('https://docs.example.com')
    // No sign-up CTA in the header for any session state.
    expect(queryByRole('link', { name: 'Get started free' })).toBeNull()
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

  it('renders the sign-in hand-off for a guest and no sign-up CTA', async () => {
    guest()

    const { findByRole, queryByRole } = await renderWithProviders(
      <PublicHeader activePath="/about" />,
      ['public'],
    )

    const signIn = await findByRole('link', { name: 'Sign in' })
    expect(signIn.getAttribute('href')).toBe('/sign-in?redirect=%2Fabout')

    // 顶栏只保留 导航 / 语言 / 明暗 / 登录；注册入口由登录页承担。
    expect(queryByRole('link', { name: 'Get started free' })).toBeNull()
  })

  it('opens the mobile panel and closes it on Escape or a link click', async () => {
    guest()
    installMatchMedia((query) => query === '(max-width: 1023px)')

    const { findByRole } = await renderWithProviders(
      <PublicHeader activePath="/about" />,
      ['public'],
    )

    const trigger = await findByRole('button', { name: 'Open navigation' })
    expect(trigger.getAttribute('aria-controls')).toBe('mobile-menu')
    expect(trigger.getAttribute('aria-expanded')).toBe('false')

    fireEvent.click(trigger)
    expect(trigger.getAttribute('aria-expanded')).toBe('true')

    let panel = document.getElementById('mobile-menu')
    expect(panel).not.toBeNull()
    // Narrow guests reach the same sign-in hand-off as desktop visitors.
    expect(
      within(panel as HTMLElement)
        .getByRole('link', { name: 'Sign in' })
        .getAttribute('href'),
    ).toBe('/sign-in?redirect=%2Fabout')

    fireEvent.keyDown(document, { key: 'Escape' })
    await waitFor(() => expect(document.getElementById('mobile-menu')).toBeNull())
    expect(document.activeElement).toBe(trigger)

    fireEvent.click(trigger)
    panel = document.getElementById('mobile-menu')
    expect(panel).not.toBeNull()
    // jsdom cannot navigate; cancelling the default keeps the suite quiet while
    // the panel's own click handler still runs and closes the panel.
    document.addEventListener('click', (event) => event.preventDefault(), {
      capture: true,
      once: true,
    })
    fireEvent.click(within(panel as HTMLElement).getByRole('link', { name: 'Pricing' }))
    await waitFor(() => expect(document.getElementById('mobile-menu')).toBeNull())
    expect(document.activeElement).toBe(trigger)
  })

  it('keeps the panel sign-in entry and no sign-up CTA', async () => {
    guest()
    installMatchMedia((query) => query === '(max-width: 1023px)')

    const { findByRole } = await renderWithProviders(
      <PublicHeader activePath="/about" />,
      ['public'],
    )

    fireEvent.click(await findByRole('button', { name: 'Open navigation' }))
    const panel = document.getElementById('mobile-menu')
    expect(panel).not.toBeNull()
    expect(within(panel as HTMLElement).getByRole('link', { name: 'Sign in' })).toBeTruthy()
    expect(
      within(panel as HTMLElement).queryByRole('link', { name: 'Get started free' }),
    ).toBeNull()
  })
})
