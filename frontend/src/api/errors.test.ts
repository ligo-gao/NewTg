import { describe, expect, it } from 'vitest'

import { ApiError, kindForStatus, parseProblem } from './errors'

const PROBLEM_402 = {
  type: 'https://newtg.example/problems/insufficient-credits',
  title: 'Insufficient credits',
  detail: 'The request requires 250 credits but only 100 are available.',
  instance: '/api/tokens',
  balance: 100,
  required: 250,
  currency: 'credits',
  topup_url: '/wallet/topup',
}

describe('parseProblem', () => {
  it('maps 402 to kind="quota" and passes problem402 through', () => {
    const error = parseProblem({
      status: 402,
      body: PROBLEM_402,
      requestId: 'req-0001',
    })

    expect(error).toBeInstanceOf(ApiError)
    expect(error.kind).toBe('quota')
    expect(error.status).toBe(402)
    expect(error.problem402?.balance).toBe(100)
    expect(error.problem402?.required).toBe(250)
    expect(error.problem402?.currency).toBe('credits')
    expect(error.problem402?.topup_url).toBe('/wallet/topup')
    expect(error.request_id).toBe('req-0001')
  })

  it('leaves request_id undefined when the X-Request-ID header is absent', () => {
    const error = parseProblem({
      status: 500,
      body: { title: 'Internal error' },
    })

    expect(error.request_id).toBeUndefined()
  })

  it('normalizes the RFC 7807 fields and ignores unknown body fields', () => {
    const error = parseProblem({
      status: 400,
      body: {
        type: 'https://newtg.example/problems/validation',
        title: 'Validation failed',
        detail: 'page_size must be one of 10, 50, 100',
        instance: '/api/usage-logs',
        upstream_key: 'sk-secret',
        prompt: 'do not surface this',
      },
    })

    expect(error.type).toBe('https://newtg.example/problems/validation')
    expect(error.title).toBe('Validation failed')
    expect(error.detail).toBe('page_size must be one of 10, 50, 100')
    expect(error.instance).toBe('/api/usage-logs')
    expect(error.kind).toBe('validation')
    // GC-2: upstream credentials and prompt plaintext must never be surfaced.
    expect(JSON.stringify(error)).not.toContain('sk-secret')
    expect(JSON.stringify(error)).not.toContain('do not surface this')
  })

  it('treats a network failure (status 0) as offline', () => {
    const error = parseProblem({ status: 0 })

    expect(error.kind).toBe('offline')
  })
})

describe('kindForStatus', () => {
  it.each([
    [401, 'auth'],
    [402, 'quota'],
    [403, 'forbidden'],
    [404, 'not_found'],
    [409, 'conflict'],
    [400, 'validation'],
    [422, 'validation'],
    [429, 'rate_limited'],
    [500, 'server'],
    [503, 'server'],
    [0, 'offline'],
  ] as const)('maps %i to %s', (status, kind) => {
    expect(kindForStatus(status)).toBe(kind)
  })
})
