import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { ApiError } from '@/api/errors'

import { fetchPublicAnnouncement } from './api'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const ANNOUNCEMENT = {
  id: '0b6f3b1e-2d3c-4f0a-9a6d-1f2e3d4c5b6a',
  title: 'Scheduled maintenance',
  category: 'maintenance',
  content_html: '<p>Restart at 02:00 UTC.</p>',
  published_at: '2026-10-01T10:00:00Z',
  expire_at: null,
}

describe('fetchPublicAnnouncement (D7b contract client)', () => {
  it('calls the generated path through apiClient and returns the typed body', async () => {
    let requestedPath = ''
    server.use(
      http.get(`${ORIGIN}/api/announcements/public/:id`, ({ request }) => {
        requestedPath = new URL(request.url).pathname
        return HttpResponse.json(ANNOUNCEMENT)
      }),
    )

    const result = await fetchPublicAnnouncement(ANNOUNCEMENT.id)
    expect(requestedPath).toBe(`/api/announcements/public/${ANNOUNCEMENT.id}`)
    expect(result.title).toBe('Scheduled maintenance')
    expect(result.expire_at).toBeNull()
  })

  it('surfaces the 404 problem as a not_found ApiError', async () => {
    server.use(
      http.get(`${ORIGIN}/api/announcements/public/:id`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Announcement not found' },
          { status: 404, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )

    await expect(fetchPublicAnnouncement('missing')).rejects.toMatchObject({
      kind: 'not_found',
    })
    await expect(fetchPublicAnnouncement('missing')).rejects.toBeInstanceOf(ApiError)
  })
})
