import { afterEach, describe, expect, it, vi } from 'vitest'

import { parseProblem } from '@/api/errors'

import {
  UNAUTHORIZED_REQUEST_ID_KEY,
  resetAuthFailureHandlers,
  setAuthFailureHandlers,
  takeUnauthorizedRequestId,
} from './auth-failures'
import { createQueryClient } from './query'

afterEach(() => {
  resetAuthFailureHandlers()
  globalThis.sessionStorage?.clear()
})

describe('global auth failure handling (web-00 §9.6)', () => {
  it('invalidates the me cache and reports a runtime 401', async () => {
    const client = createQueryClient()
    client.setQueryData(['me'], { username: 'alice', role: 'user' })
    const unauthorized = vi.fn()
    setAuthFailureHandlers({ unauthorized })

    await expect(
      client.fetchQuery({
        queryKey: ['admin-users', 'page'],
        queryFn: async () => {
          throw parseProblem({ status: 401 })
        },
      }),
    ).rejects.toThrow()

    expect(unauthorized).toHaveBeenCalledTimes(1)
    expect(client.getQueryState(['me'])?.isInvalidated).toBe(true)
  })

  it('treats a 401 from the me probe as a guest, without navigating', async () => {
    const client = createQueryClient()
    const unauthorized = vi.fn()
    setAuthFailureHandlers({ unauthorized })

    await expect(
      client.fetchQuery({
        queryKey: ['me'],
        queryFn: async () => {
          throw parseProblem({ status: 401 })
        },
      }),
    ).rejects.toThrow()

    expect(unauthorized).not.toHaveBeenCalled()
  })

  it('hard-routes password_change_required regardless of the endpoint', async () => {
    const client = createQueryClient()
    const passwordChangeRequired = vi.fn()
    setAuthFailureHandlers({ passwordChangeRequired })

    await expect(
      client.fetchQuery({
        queryKey: ['console'],
        queryFn: async () => {
          throw parseProblem({
            status: 403,
            body: {
              type: 'https://newtg.dev/problems/password_change_required',
              title: 'Forbidden',
            },
          })
        },
      }),
    ).rejects.toThrow()

    expect(passwordChangeRequired).toHaveBeenCalledTimes(1)
  })

  it('stashes the runtime 401 request_id for the /401 page and hands it over once (P1-5)', async () => {
    const client = createQueryClient()
    const unauthorized = vi.fn()
    setAuthFailureHandlers({ unauthorized })

    await expect(
      client.fetchQuery({
        queryKey: ['admin-users', 'page'],
        queryFn: async () => {
          // The transport reads X-Request-ID into this allowlisted field.
          throw parseProblem({
            status: 401,
            body: { type: 'about:blank', title: 'Unauthorized' },
            requestId: 'req_expired',
          })
        },
      }),
    ).rejects.toThrow()

    expect(unauthorized).toHaveBeenCalledTimes(1)
    expect(
      globalThis.sessionStorage?.getItem(UNAUTHORIZED_REQUEST_ID_KEY),
    ).toBe('req_expired')
    // Read-once: the error page consumes and clears it.
    expect(takeUnauthorizedRequestId()).toBe('req_expired')
    expect(takeUnauthorizedRequestId()).toBeUndefined()
  })

  it('stashes nothing when the 401 carries no request_id', async () => {
    const client = createQueryClient()
    setAuthFailureHandlers({ unauthorized: vi.fn() })

    await expect(
      client.fetchQuery({
        queryKey: ['tokens'],
        queryFn: async () => {
          throw parseProblem({ status: 401 })
        },
      }),
    ).rejects.toThrow()

    expect(
      globalThis.sessionStorage?.getItem(UNAUTHORIZED_REQUEST_ID_KEY),
    ).toBeNull()
    expect(takeUnauthorizedRequestId()).toBeUndefined()
  })

  it('does not retry deterministic failures (auth/forbidden/404/validation)', async () => {
    const client = createQueryClient()
    let calls = 0

    await expect(
      client.fetchQuery({
        queryKey: ['deterministic'],
        queryFn: async () => {
          calls += 1
          throw parseProblem({ status: 403 })
        },
      }),
    ).rejects.toThrow()

    expect(calls).toBe(1)
  })
})
