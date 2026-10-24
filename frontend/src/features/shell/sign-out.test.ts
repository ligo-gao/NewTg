import { QueryClient } from '@tanstack/react-query'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { performSignOut } from './sign-out'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function clientWithIdentity(): QueryClient {
  const client = new QueryClient()
  client.setQueryData(['me'], { username: 'alice' })
  return client
}

describe('performSignOut', () => {
  it('revokes the server session and clears cached identity', async () => {
    let calls = 0
    server.use(
      http.post(`${ORIGIN}/api/logout`, () => {
        calls += 1
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const client = clientWithIdentity()
    const navigate = vi.fn()

    await expect(performSignOut(client, navigate)).resolves.toBe(true)

    expect(calls).toBe(1)
    expect(client.getQueryData(['me'])).toBeUndefined()
    expect(navigate).toHaveBeenCalledWith('/')
  })

  it('keeps cached identity when the revoke request fails', async () => {
    server.use(
      http.post(`${ORIGIN}/api/logout`, () =>
        HttpResponse.json(
          { title: 'Service Unavailable', status: 503 },
          { status: 503 },
        ),
      ),
    )
    const client = clientWithIdentity()
    const navigate = vi.fn()

    await expect(performSignOut(client, navigate)).resolves.toBe(false)

    expect(client.getQueryData(['me'])).toEqual({ username: 'alice' })
    expect(navigate).not.toHaveBeenCalled()
  })
})
