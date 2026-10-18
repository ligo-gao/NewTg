import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { CSRF_COOKIE, CSRF_HEADER, csrfFetch, getCsrfToken } from './csrf'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()
let csrfCookie: string | undefined

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
})

beforeEach(() => {
  csrfCookie = 'token-abc'
  vi.spyOn(document, 'cookie', 'get').mockImplementation(() =>
    csrfCookie === undefined ? '' : `${CSRF_COOKIE}=${csrfCookie}`,
  )
})

afterEach(() => {
  server.resetHandlers()
})

afterAll(() => {
  server.close()
})

describe('getCsrfToken', () => {
  it('reads the __Host-nkt_csrf cookie and decodes its value', () => {
    expect(getCsrfToken()).toBe('token-abc')
    csrfCookie = 'a%20b'
    expect(getCsrfToken()).toBe('a b')
  })

  it('returns undefined when the cookie is absent', () => {
    csrfCookie = undefined
    expect(getCsrfToken()).toBeUndefined()
  })
})

describe('csrfFetch', () => {
  it('injects X-CSRF-Token on writes but not on reads', async () => {
    let writeHeader: string | null = null
    let readHeader: string | null = null

    server.use(
      http.post(`${ORIGIN}/api/echo`, ({ request }) => {
        writeHeader = request.headers.get(CSRF_HEADER)
        return HttpResponse.json({ ok: true })
      }),
      http.get(`${ORIGIN}/api/echo`, ({ request }) => {
        readHeader = request.headers.get(CSRF_HEADER)
        return HttpResponse.json({ ok: true })
      }),
    )

    await csrfFetch('/api/echo', { method: 'POST', body: '{}' })
    await csrfFetch('/api/echo')

    expect(writeHeader).toBe('token-abc')
    expect(readHeader).toBeNull()
  })

  it('preserves method, body, headers and credentials from a Request input', async () => {
    let seenMethod: string | undefined
    let seenBody: string | undefined
    let seenCustom: string | null = null
    let seenCsrf: string | null = null

    server.use(
      http.post(`${ORIGIN}/api/echo`, async ({ request }) => {
        seenMethod = request.method
        seenBody = await request.text()
        seenCustom = request.headers.get('X-Custom')
        seenCsrf = request.headers.get(CSRF_HEADER)
        return HttpResponse.json({ ok: true })
      }),
    )

    const request = new Request(`${ORIGIN}/api/echo`, {
      method: 'POST',
      body: 'payload',
      headers: { 'Content-Type': 'text/plain', 'X-Custom': 'kept' },
    })

    const response = await csrfFetch(request)

    expect(response.status).toBe(200)
    expect(seenMethod).toBe('POST')
    expect(seenBody).toBe('payload')
    expect(seenCustom).toBe('kept')
    expect(seenCsrf).toBe('token-abc')
  })

  it('refreshes the session via GET /api/me and replays a 401 exactly once', async () => {
    let writeCalls = 0
    let refreshCalls = 0
    const seenTokens: Array<string | null> = []

    server.use(
      http.post(`${ORIGIN}/api/echo`, ({ request }) => {
        writeCalls += 1
        seenTokens.push(request.headers.get(CSRF_HEADER))
        if (writeCalls === 1) {
          return HttpResponse.json({ type: 'unauthorized' }, { status: 401 })
        }
        return HttpResponse.json({ ok: true })
      }),
      http.get(`${ORIGIN}/api/me`, () => {
        refreshCalls += 1
        csrfCookie = 'fresh-token'
        return HttpResponse.json({ username: 'me' })
      }),
    )

    const response = await csrfFetch('/api/echo', { method: 'POST', body: '{}' })

    expect(response.status).toBe(200)
    expect(writeCalls).toBe(2)
    expect(refreshCalls).toBe(1)
    // The stale value must not be replayed: the refreshed cookie wins.
    expect(seenTokens).toEqual(['token-abc', 'fresh-token'])
  })

  it('replays a 403 csrf_failed once as well and surfaces a second failure', async () => {
    let writeCalls = 0
    let refreshCalls = 0

    server.use(
      http.post(`${ORIGIN}/api/echo`, () => {
        writeCalls += 1
        return HttpResponse.json(
          { type: 'https://newtg.dev/problems/csrf_failed' },
          { status: 403 },
        )
      }),
      http.get(`${ORIGIN}/api/me`, () => {
        refreshCalls += 1
        return HttpResponse.json({ username: 'me' })
      }),
    )

    const response = await csrfFetch('/api/echo', { method: 'POST', body: '{}' })

    expect(response.status).toBe(403)
    expect(writeCalls).toBe(2)
    expect(refreshCalls).toBe(1)
  })
})
