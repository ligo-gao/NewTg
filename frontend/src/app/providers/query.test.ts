import { afterEach, describe, expect, it, vi } from 'vitest'

import { parseProblem } from '@/api/errors'

import {
  resetAuthFailureHandlers,
  setAuthFailureHandlers,
} from './auth-failures'
import { createQueryClient } from './query'

afterEach(() => {
  resetAuthFailureHandlers()
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
