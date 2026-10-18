import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { CSRF_COOKIE, CSRF_HEADER, createApiClient } from './client'

/**
 * The M0 `paths` type is the frozen empty placeholder, so these tests call the
 * client through a loose structural facade; the assertions target runtime
 * behaviour (headers, replay, normalization), not generated types.
 */
const BASE_URL = 'http://localhost'

interface LooseResponse {
  data?: unknown
  error?: unknown
  response: Response
}

interface LooseClient {
  GET: (path: string) => Promise<LooseResponse>
  POST: (path: string, init: { body?: unknown }) => Promise<LooseResponse>
}

function looseClient(): LooseClient {
  return createApiClient({ baseUrl: BASE_URL }) as unknown as LooseClient
}

const server = setupServer()

let csrfCookie: string | undefined

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
})

beforeEach(() => {
  csrfCookie = 'stale-token'
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

describe('contract client headers', () => {
  it('adds Idempotency-Key and X-CSRF-Token to writes but not to GET', async () => {
    let postHeaders: Headers | undefined
    let getHeaders: Headers | undefined
    csrfCookie = 'token-abc'

    server.use(
      http.post(`${BASE_URL}/api/tokens`, ({ request }) => {
        postHeaders = new Headers(request.headers)
        return HttpResponse.json({ ok: true })
      }),
      http.get(`${BASE_URL}/api/me`, ({ request }) => {
        getHeaders = new Headers(request.headers)
        return HttpResponse.json({ id: 'me' })
      }),
    )

    const client = looseClient()
    await client.POST('/api/tokens', { body: { name: 'ci' } })
    await client.GET('/api/me')

    expect(postHeaders?.get('Idempotency-Key')).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    )
    expect(postHeaders?.get(CSRF_HEADER)).toBe('token-abc')

    expect(getHeaders?.get('Idempotency-Key')).toBeNull()
    expect(getHeaders?.get(CSRF_HEADER)).toBeNull()
  })
})

describe('403 csrf_failed recovery', () => {
  it('refreshes the session cookie via GET /api/me and replays a body-bearing write exactly once', async () => {
    let writeCalls = 0
    let meCalls = 0
    const bodies: unknown[] = []
    const csrfHeaders: Array<string | null> = []
    const idempotencyKeys: Array<string | null> = []

    server.use(
      http.post(`${BASE_URL}/api/tokens`, async ({ request }) => {
        writeCalls += 1
        bodies.push(await request.json())
        csrfHeaders.push(request.headers.get(CSRF_HEADER))
        idempotencyKeys.push(request.headers.get('Idempotency-Key'))

        if (writeCalls === 1) {
          return HttpResponse.json(
            {
              type: 'https://newtg.example/problems/csrf_failed',
              title: 'CSRF validation failed',
            },
            { status: 403 },
          )
        }
        return HttpResponse.json({ ok: true })
      }),
      http.get(`${BASE_URL}/api/me`, () => {
        meCalls += 1
        csrfCookie = 'fresh-token'
        return HttpResponse.json({ id: 'me' })
      }),
    )

    const client = looseClient()
    await client.POST('/api/tokens', { body: { name: 'ci' } })

    // One rejected write + one replay = two server calls, plus the refresh.
    expect(writeCalls).toBe(2)
    expect(meCalls).toBe(1)

    // The replay carries the same request body (the regression under review:
    // replaying a consumed Request used to throw and surface as `offline`).
    expect(bodies).toEqual([{ name: 'ci' }, { name: 'ci' }])
    // The stale token is replaced by the one re-issued by GET /api/me.
    expect(csrfHeaders).toEqual(['stale-token', 'fresh-token'])
    // The same intent keeps the same idempotency key across the replay.
    expect(idempotencyKeys[0]).not.toBeNull()
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0])
  })

  it('invokes the configured fetch unbound for dispatch, refresh and replay', async () => {
    const observedThis: unknown[] = []
    let writeCalls = 0
    let meCalls = 0
    const csrfHeaders: Array<string | null> = []

    async function observedFetch(this: unknown, input: Request): Promise<Response> {
      observedThis.push(this)

      if (input.url === `${BASE_URL}/api/me` && input.method === 'GET') {
        meCalls += 1
        csrfCookie = 'fresh-token'
        return new Response(JSON.stringify({ id: 'me' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }

      if (input.url === `${BASE_URL}/api/tokens` && input.method === 'POST') {
        writeCalls += 1
        csrfHeaders.push(input.headers.get(CSRF_HEADER))
        if (writeCalls === 1) {
          return new Response(
            JSON.stringify({ type: 'csrf_failed', title: 'CSRF validation failed' }),
            {
              status: 403,
              headers: { 'Content-Type': 'application/problem+json' },
            },
          )
        }
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }

      throw new Error(`unexpected request: ${input.method} ${input.url}`)
    }

    const client = createApiClient({
      baseUrl: BASE_URL,
      fetch: observedFetch,
    }) as unknown as LooseClient

    await client.POST('/api/tokens', { body: { name: 'ci' } })

    expect(writeCalls).toBe(2)
    expect(meCalls).toBe(1)
    expect(csrfHeaders).toEqual(['stale-token', 'fresh-token'])

    // Native browser fetch brand-checks its receiver: `options.fetch(...)`
    // passes the frozen options object as `this` and throws "Illegal
    // invocation". Node/undici ignores `this`, so assert it directly here.
    expect(observedThis).toHaveLength(3)
    expect(observedThis.every((thisArg) => thisArg === undefined)).toBe(true)
  })

  it('surfaces a second csrf failure as kind="forbidden"', async () => {
    let writeCalls = 0
    let meCalls = 0

    server.use(
      http.post(`${BASE_URL}/api/tokens`, () => {
        writeCalls += 1
        return HttpResponse.json(
          { type: 'csrf_failed', title: 'CSRF validation failed' },
          { status: 403 },
        )
      }),
      http.get(`${BASE_URL}/api/me`, () => {
        meCalls += 1
        return HttpResponse.json({ id: 'me' })
      }),
    )

    const client = looseClient()
    await expect(
      client.POST('/api/tokens', { body: { name: 'ci' } }),
    ).rejects.toMatchObject({ kind: 'forbidden', status: 403 })

    expect(writeCalls).toBe(2)
    expect(meCalls).toBe(1)
  })
})

describe('transport failures', () => {
  it('normalizes a fetch TypeError to kind="offline"', async () => {
    server.use(http.get(`${BASE_URL}/api/offline`, () => HttpResponse.error()))

    const client = looseClient()
    await expect(client.GET('/api/offline')).rejects.toMatchObject({
      kind: 'offline',
      status: 0,
    })
  })
})
