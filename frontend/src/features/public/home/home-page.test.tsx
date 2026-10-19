import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { sanitizeHtmlFragment } from './custom-html'
import { HomePage } from './home-page'
import { buildBaseUrl, sampleCode } from './onboarding'
import {
  HOME_STATS_CACHE_KEY,
  normalizeHomeStats,
  readHomeStatsCache,
  writeHomeStatsCache,
} from './stats'

const ORIGIN = 'http://localhost:3000'

function status(overrides: Record<string, unknown> = {}) {
  return {
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
      server_address: 'https://api.example.com/',
      docs_url: 'https://docs.example.com',
      footer_html: '',
      about_html: '',
      filing_no: '',
      status_page_url: '',
      seo: { description: 'Acme gateway for teams', og_image_url: 'https://cdn.example.com/og.png' },
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
    ...overrides,
  }
}

const STATS = { upstreams: 42, models: 1234, endpoints: 3, routing_dims: 6 }

const server = setupServer()
let requestedPaths: string[] = []

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  requestedPaths = []
  localStorage.clear()
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function useHandlers(
  body: Record<string, unknown>,
  statsHandler: () => Response = () =>
    HttpResponse.json(STATS) as unknown as Response,
) {
  server.use(
    http.get(`${ORIGIN}/api/status`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json(body)
    }),
    http.get(`${ORIGIN}/api/me`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return HttpResponse.json({}, { status: 401 })
    }),
    http.get(`${ORIGIN}/api/home/stats`, ({ request }) => {
      requestedPaths.push(new URL(request.url).pathname)
      return statsHandler()
    }),
  )
}

describe('homepage (F-09-01/03/04/05/06)', () => {
  it('renders hero CTAs, six feature cards, final stats and onboarding for a guest', async () => {
    useHandlers(status())
    const { findByRole, getByRole, getByText } = await renderWithProviders(
      <HomePage />,
      ['public'],
    )

    expect(await findByRole('heading', { level: 1 })).toBeTruthy()
    expect(getByRole('link', { name: 'Get started' }).getAttribute('href')).toBe('/sign-up')
    expect(getByRole('link', { name: 'View pricing' }).getAttribute('href')).toBe('/pricing')

    // Six capability cards, all from the frozen feature set.
    const featureSection = getByRole('heading', { name: 'Core capabilities' }).closest('section')
    expect(featureSection).not.toBeNull()
    const featureHeadings = await within(featureSection as HTMLElement).findAllByRole(
      'heading',
      { level: 3 },
    )
    expect(featureHeadings.map((node) => node.textContent)).toEqual([
      'Multi-upstream aggregation',
      'Transparent credit billing',
      'Streaming and cross-protocol relay',
      'Rate limits and quotas',
      'Stateless cluster',
      'Credential governance',
    ])

    // Stats render the final value; there is no count-up state to wait for.
    await waitFor(() => expect(getByText('1,234')).toBeTruthy())
    expect(getByText('42')).toBeTruthy()

    // Base URL derivation trims the configured trailing slash.
    expect(getByText('https://api.example.com/v1')).toBeTruthy()
    const token = getByText(/<YOUR_TOKEN>/)
    expect(token).toBeTruthy()
    expect(
      getByRole('link', { name: 'Create token' }).getAttribute('href'),
    ).toBe('/sign-up?redirect=%2Ftokens')

    // live_feed is P1: its endpoint must not be probed from the home page.
    expect(requestedPaths.some((path) => path.includes('live-feed'))).toBe(false)
  })

  it('lets custom HTML take over and strips scripts and event handlers', async () => {
    useHandlers(
      status({
        site: {
          ...status().site,
          home_custom_enabled: true,
          home_custom_html:
            '<h1>Operator page</h1><script>alert(1)</script><p onclick="x()">Body</p><a href="https://example.com">link</a><a href="javascript:alert(1)">bad</a>',
        },
      }),
    )
    const { findByRole, queryByText, container } = await renderWithProviders(
      <HomePage />,
      ['public'],
    )

    expect((await findByRole('heading', { level: 1 })).textContent).toBe('Operator page')
    expect(container.querySelector('script')).toBeNull()
    expect(container.innerHTML).not.toContain('onclick')
    // The five built-in sections are gone.
    expect(queryByText('Core capabilities')).toBeNull()
    expect(queryByText('Get started')).toBeNull()

    const external = container.querySelector('a[href="https://example.com"]')
    expect(external?.getAttribute('rel')).toBe('noopener noreferrer')
    const bad = container.querySelector('a[href^="javascript:"]')
    expect(bad).toBeNull()
  })

  it('shows the configuring state when every built-in section is off', async () => {
    useHandlers(
      status({
        site: {
          ...status().site,
          home_blocks: {
            hero: false,
            live_feed: false,
            features: false,
            stats: false,
            onboarding: false,
          },
        },
      }),
    )
    const { findByText } = await renderWithProviders(<HomePage />, ['public'])
    expect(await findByText('This site is being configured.')).toBeTruthy()
  })

  it('writes SEO tags from site configuration (F-09-06)', async () => {
    useHandlers(status())
    await renderWithProviders(<HomePage />, ['public'])

    await waitFor(() => {
      expect(document.title).toBe('Acme Gateway')
    })
    expect(
      document.head.querySelector('meta[name="description"]')?.getAttribute('content'),
    ).toBe('Acme gateway for teams')
    expect(
      document.head.querySelector('meta[property="og:image"]')?.getAttribute('content'),
    ).toBe('https://cdn.example.com/og.png')
    expect(
      document.head
        .querySelector('meta[property="og:url"]')
        ?.getAttribute('content'),
    ).toBe('https://api.example.com/')
    expect(
      document.head.querySelector('link[rel="canonical"]')?.getAttribute('href'),
    ).toBe('https://api.example.com/')
    expect(
      document.head
        .querySelector('meta[name="twitter:card"]')
        ?.getAttribute('content'),
    ).toBe('summary_large_image')
  })
})

describe('stats cache and base URL (F-09-04/05)', () => {
  it('validates counters and expires the 24h cache', () => {
    expect(normalizeHomeStats(STATS)).toEqual(STATS)
    expect(() => normalizeHomeStats({ ...STATS, models: -1 })).toThrow()
    expect(() => normalizeHomeStats({ ...STATS, endpoints: 1.5 })).toThrow()

    const now = Date.now()
    writeHomeStatsCache(STATS, now)
    expect(readHomeStatsCache(now + 1000)).toEqual(STATS)
    expect(readHomeStatsCache(now + 24 * 60 * 60 * 1000 + 1)).toBeNull()
    expect(localStorage.getItem(HOME_STATS_CACHE_KEY)).toContain('cached_at')
  })

  it('builds a single-slash base URL and falls back to the placeholder', () => {
    expect(buildBaseUrl('https://api.example.com/')).toEqual({
      url: 'https://api.example.com/v1',
      placeholder: false,
    })
    expect(buildBaseUrl('https://api.example.com')).toEqual({
      url: 'https://api.example.com/v1',
      placeholder: false,
    })
    expect(buildBaseUrl('')).toEqual({
      url: 'https://your-gateway.example.com/v1',
      placeholder: true,
    })
    expect(buildBaseUrl('javascript:alert(1)')).toEqual({
      url: 'https://your-gateway.example.com/v1',
      placeholder: true,
    })
  })

  it('never puts a real credential into a code sample', () => {
    const code = sampleCode('curl', 'https://api.example.com/v1')
    expect(code).toContain('Bearer <YOUR_TOKEN>')
    expect(sampleCode('python', 'https://api.example.com/v1')).toContain(
      'api_key="<YOUR_TOKEN>"',
    )
    expect(sampleCode('node', 'https://api.example.com/v1')).toContain(
      'apiKey: "<YOUR_TOKEN>"',
    )
  })

  it('sanitizes operator markup and reports an empty result', () => {
    expect(sanitizeHtmlFragment('<p>Hello <strong>world</strong></p>')).toBe(
      '<p>Hello <strong>world</strong></p>',
    )
    expect(sanitizeHtmlFragment('<script>alert(1)</script>')).toBe('')
    expect(sanitizeHtmlFragment('<div><p>kept</p></div>')).toBe('<p>kept</p>')
  })
})
