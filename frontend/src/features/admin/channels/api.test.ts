import { describe, expect, it, vi } from 'vitest'

import { createChannel, updateChannel } from './api'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeChannel,
  problemResponse,
} from './test-utils'

const INPUT = {
  name: 'ci',
  type: 'openai',
  credentials: { api_key: 'sk-12345678' },
  supported_models: ['gpt-4'],
}

function mockCsrfCookie(read: () => string): void {
  vi.spyOn(document, 'cookie', 'get').mockImplementation(
    () => `__Host-nkt_csrf=${read()}`,
  )
}

describe('channel API safety headers', () => {
  it('sends X-CSRF-Token on writes and the caller-owned idempotency key', async () => {
    mockCsrfCookie(() => 'cookie-token')
    const requests = installFetch((request) => {
      if (request.method === 'POST' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeChannel(), 201)
      }
      if (
        request.method === 'PATCH' &&
        request.url.pathname === '/api/channels/1'
      ) {
        return jsonResponse(makeChannel())
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await createChannel(INPUT, 'intent-key-1')
    await updateChannel(1, { expected_version: 'v1', name: 'renamed' })

    const post = findRequest(requests, 'POST', '/api/channels')
    expect(post?.headers.get('X-CSRF-Token')).toBe('cookie-token')
    expect(post?.headers.get('Idempotency-Key')).toBe('intent-key-1')

    const patch = findRequest(requests, 'PATCH', '/api/channels/1')
    expect(patch?.headers.get('X-CSRF-Token')).toBe('cookie-token')
  })
})

describe('channel API 403 csrf_failed recovery', () => {
  it('refreshes the cookie via GET /api/me and replays a stale write exactly once', async () => {
    let token = 'stale-token'
    mockCsrfCookie(() => token)
    let postCalls = 0
    let meCalls = 0
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/me') {
        meCalls += 1
        token = 'fresh-token'
        return jsonResponse({ id: 'me' })
      }
      if (request.method === 'POST' && request.url.pathname === '/api/channels') {
        postCalls += 1
        if (postCalls === 1) {
          return problemResponse(403, {
            type: 'https://newtg.dev/problems/csrf_failed',
            title: 'CSRF validation failed',
          })
        }
        return jsonResponse(makeChannel(), 201)
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    const created = await createChannel(INPUT, 'intent-key-1')

    expect(created.id).toBe(1)
    // One rejected write + one replay, plus the single cookie refresh.
    expect(postCalls).toBe(2)
    expect(meCalls).toBe(1)

    const posts = requests.filter(
      (request) =>
        request.method === 'POST' && request.url.pathname === '/api/channels',
    )
    expect(posts[0]?.headers.get('X-CSRF-Token')).toBe('stale-token')
    // The replay drops the rejected token and uses the refreshed one.
    expect(posts[1]?.headers.get('X-CSRF-Token')).toBe('fresh-token')
    // The retry is still the same user intent: same key, same body.
    expect(posts[1]?.headers.get('Idempotency-Key')).toBe('intent-key-1')
    expect(posts[1]?.body).toEqual(posts[0]?.body)
  })

  it('surfaces a second csrf failure as forbidden without a third attempt', async () => {
    mockCsrfCookie(() => 'stale-token')
    let postCalls = 0
    let meCalls = 0
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/me') {
        meCalls += 1
        return jsonResponse({ id: 'me' })
      }
      if (request.method === 'POST' && request.url.pathname === '/api/channels') {
        postCalls += 1
        return problemResponse(403, {
          type: 'https://newtg.dev/problems/csrf_failed',
          title: 'CSRF validation failed',
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await expect(createChannel(INPUT, 'intent-key-1')).rejects.toMatchObject({
      kind: 'forbidden',
      status: 403,
    })

    expect(postCalls).toBe(2)
    expect(meCalls).toBe(1)
  })
})
