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

  it('does not retry a 429: the Retry-After countdown is the only response', async () => {
    const client = createQueryClient()
    let calls = 0

    await expect(
      client.fetchQuery({
        queryKey: ['rate-limited'],
        queryFn: async () => {
          calls += 1
          throw parseProblem({
            status: 429,
            retryAfter: '5',
            body: { type: 'about:blank', title: 'Too Many Requests' },
          })
        },
      }),
    ).rejects.toThrow()

    // 自动重试会在服务端刚要求降速时放大压力；必须恰好一次请求。
    expect(calls).toBe(1)
  })
})

describe('MFA enrollment required handling (R-04)', () => {
  const mfaProblem = () =>
    parseProblem({
      status: 403,
      body: {
        type: 'https://newtg.dev/problems/mfa_enrollment_required',
        title: 'Forbidden',
        detail: 'Enroll in two-factor authentication to continue',
      },
    })

  it('clears the me cache and redirects exactly once for concurrent 403s', async () => {
    const client = createQueryClient()
    client.setQueryData(['me'], { username: 'admin', role: 'admin' })
    const mfaEnrollmentRequired = vi.fn()
    setAuthFailureHandlers({ mfaEnrollmentRequired })

    await Promise.allSettled([
      client.fetchQuery({
        queryKey: ['admin-users', 'page'],
        queryFn: async () => {
          throw mfaProblem()
        },
      }),
      client.fetchQuery({
        queryKey: ['admin-channels', 'page'],
        queryFn: async () => {
          throw mfaProblem()
        },
      }),
    ])

    // A burst of 403s must collapse into a single navigation; the cached
    // identity must not survive as if the session were still usable.
    expect(mfaEnrollmentRequired).toHaveBeenCalledTimes(1)
    expect(client.getQueryState(['me'])).toBeUndefined()
  })

  it('does not react to an ordinary 403', async () => {
    const client = createQueryClient()
    client.setQueryData(['me'], { username: 'admin', role: 'admin' })
    const mfaEnrollmentRequired = vi.fn()
    setAuthFailureHandlers({ mfaEnrollmentRequired })

    await expect(
      client.fetchQuery({
        queryKey: ['admin-users', 'page'],
        queryFn: async () => {
          throw parseProblem({ status: 403 })
        },
      }),
    ).rejects.toThrow()

    expect(mfaEnrollmentRequired).not.toHaveBeenCalled()
    expect(client.getQueryState(['me'])?.data).toBeDefined()
  })

  it('does not redirect again from the sign-in page itself', async () => {
    const previous = `${location.pathname}${location.search}`
    window.history.replaceState({}, '', '/sign-in?reason=mfa_enrollment_required')
    try {
      const client = createQueryClient()
      client.setQueryData(['me'], { username: 'admin', role: 'admin' })
      const mfaEnrollmentRequired = vi.fn()
      setAuthFailureHandlers({ mfaEnrollmentRequired })

      await expect(
        client.fetchQuery({
          queryKey: ['status-widget'],
          queryFn: async () => {
            throw mfaProblem()
          },
        }),
      ).rejects.toThrow()

      // The landing page's own requests must not bounce back onto it.
      expect(mfaEnrollmentRequired).not.toHaveBeenCalled()
    } finally {
      window.history.replaceState({}, '', previous)
    }
  })
})
