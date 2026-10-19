import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { AboutPage } from './about-page'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function publicStatus(languages: string[]) {
  return {
    system_initialized: true,
    auth: {
      signup_enabled: true,
      password_login_enabled: true,
      email_verification_required: false,
      turnstile: { enabled: false, site_key: '' },
    },
    password_login_encryption_enabled: false,
    languages,
    default_language: 'en',
  }
}

const SEVEN_LANGUAGES = ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi']

describe('AboutPage (F-09-07)', () => {
  it('renders the tagline, tech stack and open-source attribution table', async () => {
    server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES))))

    const { getByRole, getByText, getAllByText, getAllByRole } = await renderWithProviders(<AboutPage />, [
      'public',
    ])

    expect(getByRole('heading', { level: 1 }).textContent).toContain('About NewTG')
    expect(getByText('A commercial AI API gateway built for developers.')).toBeTruthy()
    expect(getByText('Tech stack')).toBeTruthy()
    expect(getByText('Backend')).toBeTruthy()
    // PostgreSQL appears in both the tech-stack list and the attribution table.
    expect(getAllByText('PostgreSQL').length).toBeGreaterThan(0)

    const table = getByRole('table')
    expect(table.textContent).toContain('Go')
    expect(table.textContent).toContain('MIT')
    expect(table.textContent).toContain('Component')
    expect(table.textContent).toContain('License')

    // 外链必须带 target/rel，且可访问名称说明会在新标签打开（UI 设计 §7）。
    const attributionLinks = getAllByRole('link', { name: /Opens in a new tab/ })
    expect(attributionLinks.length).toBeGreaterThan(0)
    for (const link of attributionLinks) {
      expect(link.getAttribute('target')).toBe('_blank')
      expect(link.getAttribute('rel')).toBe('noopener noreferrer')
    }
  })

  it('uses the real language count from /api/status and never invents other statistics', async () => {
    server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES))))

    const { findByText, getByText } = await renderWithProviders(<AboutPage />, ['public'])

    expect(await findByText('7')).toBeTruthy()
    expect(getByText('Interface languages')).toBeTruthy()
  })

  it('hides the docs block when no docs URL is configured', async () => {
    server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES))))

    const { queryByText } = await renderWithProviders(<AboutPage />, ['public'])

    // 后端目前没有 site.docs_url 字段（PublicStatus 未下发），因此整区隐藏，
    // 不用静态 URL 冒充配置值。
    expect(queryByText('Docs home')).toBeNull()
    expect(queryByText('Integration guide')).toBeNull()
  })

  it('renders the docs links when a docs URL is supplied', async () => {
    server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES))))

    const { getByText, getAllByRole } = await renderWithProviders(
      <AboutPage docsUrl="https://docs.example.com" />,
      ['public'],
    )

    expect(getByText('Docs home')).toBeTruthy()
    const links = getAllByRole('link', { name: /Docs home/ })
    expect(links[0]?.getAttribute('href')).toBe('https://docs.example.com')
  })

  it('shows the empty attribution note when the credits list is empty', async () => {
    server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES))))

    const { getByText } = await renderWithProviders(<AboutPage credits={[]} />, ['public'])

    expect(getByText('No attributions listed')).toBeTruthy()
  })

  it('falls back to static content when /api/status fails, without an inline error', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({ type: 'about:blank', title: 'Unavailable' }, { status: 503 }),
      ),
    )

    const { getByText, queryByText } = await renderWithProviders(<AboutPage />, ['public'])

    expect(getByText('Tech stack')).toBeTruthy()
    expect(getByText('Open-source attributions')).toBeTruthy()
    await waitFor(() => {
      expect(queryByText('Interface languages')).toBeNull()
    })
  })

  it('does not render rankings or the live feed (M7 boundary, R-03)', async () => {
    server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES))))

    const { queryByText } = await renderWithProviders(<AboutPage />, ['public'])

    expect(queryByText(/Rankings/i)).toBeNull()
    expect(queryByText(/Live call feed/i)).toBeNull()
  })
})
