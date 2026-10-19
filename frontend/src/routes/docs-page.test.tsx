import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { DocsRoute } from './docs'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('/docs page states (F-09-38)', () => {
  it('reports "status unavailable" instead of "docs not configured" on a status outage', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json({}, { status: 500 })),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )

    const { findByText, queryByText, findByRole } = await renderWithProviders(
      <DocsRoute />,
      ['public'],
    )

    expect(await findByText('Status unavailable')).toBeTruthy()
    expect(queryByText('Docs are not available yet')).toBeNull()
    // Retryable state: the probe can be re-issued.
    expect(await findByRole('button', { name: 'Retry' })).toBeTruthy()
  })
})
