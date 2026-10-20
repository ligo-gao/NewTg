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

  it('reads problem.fields and drops non-string or empty entries', () => {
    const error = parseProblem({
      status: 422,
      body: {
        title: 'Validation failed',
        fields: ['prompt_price', 3, '', 'billing_expr'],
      },
    })

    expect(error.fields).toEqual(['prompt_price', 'billing_expr'])
  })

  it('parses whitelisted catalog extension members', () => {
    const error = parseProblem({
      status: 422,
      body: {
        title: 'Channel models cannot be emptied',
        channel_ids: [12, 19],
        reference_count: 3,
        conflicts: [
          { vendor_id: 4, current_version: 'sha256:abc', ignored: 'x' },
          { vendor_id: 'bad' },
        ],
      },
    })

    expect(error.channel_ids).toEqual([12, 19])
    expect(error.reference_count).toBe(3)
    expect(error.conflicts).toEqual([{ vendor_id: 4, current_version: 'sha256:abc' }])
  })

  it('drops malformed channel_ids instead of surfacing partial values', () => {
    const error = parseProblem({
      status: 422,
      body: { channel_ids: [1, '2'], reference_count: 'many' },
    })

    expect(error.channel_ids).toBeUndefined()
    expect(error.reference_count).toBeUndefined()
  })

  it('rejects negative or fractional counts on reference_count and conflicts', () => {
    for (const referenceCount of [-1, 1.5]) {
      const error = parseProblem({
        status: 409,
        body: { reference_count: referenceCount },
      })
      expect(error.reference_count).toBeUndefined()
    }

    const error = parseProblem({
      status: 409,
      body: {
        conflicts: [
          { vendor_id: -2 },
          { vendor_id: 3.5 },
          { vendor_id: 4, current_version: 'sha256:abc' },
        ],
      },
    })

    expect(error.conflicts).toEqual([{ vendor_id: 4, current_version: 'sha256:abc' }])
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
