import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { LegalPage } from './LegalPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer(
  http.get(`${ORIGIN}/api/status`, () =>
    HttpResponse.json({
      languages: ['en'],
      default_language: 'en',
      site: { title: 'Acme Gateway' },
    }),
  ),
  http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
)

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('LegalPage', () => {
  it('renders the document and the shared public footer', async () => {
    const result = await renderWithProviders(<LegalPage doc="terms" />, [
      'common',
      'auth',
    ])

    const footer = await result.findByRole('contentinfo')
    expect(result.getAllByRole('contentinfo')).toHaveLength(1)
    const openai = await within(footer).findByRole('link', {
      name: 'OpenAI (Opens in a new tab)',
    })
    expect(openai.getAttribute('href')).toBe('https://openai.com')
    expect(
      within(footer)
        .getByRole('link', { name: 'Terms of service' })
        .getAttribute('href'),
    ).toBe('/legal/terms')
  })
})
