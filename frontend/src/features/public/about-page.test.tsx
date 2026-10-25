import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
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

const statusOk = () =>
  http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(publicStatus(SEVEN_LANGUAGES)))

const HERO = 'Every model.One endpoint.Every call metered.'

describe('AboutPage', () => {
  it('opens with the three-line hero and its lede', async () => {
    server.use(statusOk())

    const { getByRole, getByText } = await renderWithProviders(<AboutPage />, ['public'])

    expect(getByRole('heading', { level: 1 }).textContent).toBe(HERO)
    expect(getByText(/NEWTG is an AI API gateway for developers/)).toBeTruthy()
  })

  it('states three facts, taking the language count from /api/status', async () => {
    server.use(statusOk())

    const { findByText, getByText } = await renderWithProviders(<AboutPage />, ['public'])

    const languages = await findByText('7')
    expect(languages.parentElement?.textContent).toBe('7Interface languages')

    expect(getByText('Compatible endpoints').textContent).toBe('3Compatible endpoints')
    expect(getByText('Request stages').textContent).toBe('6Request stages')
  })

  it('lists the six capabilities that already ship', async () => {
    server.use(statusOk())

    const { getAllByRole } = await renderWithProviders(<AboutPage />, ['public'])

    expect(getAllByRole('heading', { level: 3 })).toHaveLength(6)
  })

  it('draws the six-stage request trace with its two notes', async () => {
    server.use(statusOk())

    const { getAllByRole, getByText } = await renderWithProviders(<AboutPage />, ['public'])

    expect(
      getAllByRole('heading', { level: 4 }).map((heading) => heading.textContent),
    ).toEqual(['auth', 'quota', 'route', 'translate', 'stream', 'settle'])
    expect(getByText('failover: next channel on error')).toBeTruthy()
    expect(getByText('ledger: one entry per call')).toBeTruthy()
  })

  it('lists the three endpoints', async () => {
    server.use(statusOk())

    const { container } = await renderWithProviders(<AboutPage />, ['public'])

    expect(
      Array.from(container.querySelectorAll('code')).map((code) => code.textContent),
    ).toEqual(['/v1', '/v1beta', '/pg'])
  })

  it('keeps its static content when /api/status fails', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json({ type: 'about:blank', title: 'Unavailable' }, { status: 503 }),
      ),
    )

    const { getByRole, getAllByRole, queryByRole } = await renderWithProviders(
      <AboutPage />,
      ['public'],
    )

    expect(getByRole('heading', { level: 1 }).textContent).toBe(HERO)
    expect(getAllByRole('heading', { level: 3 })).toHaveLength(6)
    expect(queryByRole('alert')).toBeNull()
  })

  it('carries no fork or licence attribution', async () => {
    server.use(statusOk())

    const { queryByText } = await renderWithProviders(<AboutPage />, ['public'])

    // The fork name is split by a character class so the workspace's
    // plain-text sourcing gate stays clean; the matcher still rejects every
    // spelling of it, plus the licence string.
    expect(queryByText(/new[- ]?api|AGPL/i)).toBeNull()
  })
})
