import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError } from '@/api/errors'

import { fetchModelPerfSeries, type ModelPerfBucket } from './api'

/**
 * `fetchModelPerfSeries` 绕过 `apiClient` 直接 `fetch`（端点尚无契约），所以
 * 这里要钉住它自己维护的三条错误口径与「坏桶丢弃」的防御，而不是只测成功
 * 路径。请求层用 `vi.stubGlobal('fetch')` 替换，不经过 MSW：要区分 TypeError
 * 与 AbortError 的原始实例，必须在 fetch 处精确注入。
 */

const BUCKET: ModelPerfBucket = {
  start: '2026-10-28T09:00:00Z',
  model: 'gpt-4o',
  requests: 12,
  ttft_p50_ms: 420,
  ttft_p95_ms: 910,
  ttft_p50_over_30s: false,
  ttft_p95_over_30s: false,
}

afterEach(() => {
  vi.unstubAllGlobals()
})

function stubFetch(impl: () => Promise<Response>): void {
  vi.stubGlobal('fetch', vi.fn(impl))
}

async function rejection(promise: Promise<unknown>): Promise<unknown> {
  try {
    await promise
  } catch (error) {
    return error
  }
  throw new Error('expected the promise to reject')
}

describe('fetchModelPerfSeries', () => {
  it('normalizes a non-2xx response into an ApiError', async () => {
    stubFetch(
      async () =>
        new Response(
          JSON.stringify({
            type: 'https://newtg.dev/problems/internal_error',
            title: 'Monitoring unavailable',
            status: 503,
          }),
          {
            status: 503,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
    )

    const error = await rejection(fetchModelPerfSeries('gpt-4o'))

    expect(error).toBeInstanceOf(ApiError)
    const apiError = error as ApiError
    expect(apiError.status).toBe(503)
    expect(apiError.kind).toBe('server')
    expect(apiError.title).toBe('Monitoring unavailable')
  })

  it('maps a transport TypeError to the offline kind', async () => {
    stubFetch(async () => {
      throw new TypeError('fetch failed')
    })

    const error = await rejection(fetchModelPerfSeries('gpt-4o'))

    expect(error).toBeInstanceOf(ApiError)
    const apiError = error as ApiError
    expect(apiError.status).toBe(0)
    expect(apiError.kind).toBe('offline')
  })

  it('rethrows AbortError unchanged instead of turning it into an ApiError', async () => {
    const abort = new DOMException('The operation was aborted.', 'AbortError')
    stubFetch(async () => {
      throw abort
    })

    const error = await rejection(fetchModelPerfSeries('gpt-4o'))

    // 取消不是传输失败：调用方要能按原始 abort 语义处理。
    expect(error).toBe(abort)
    expect(error).not.toBeInstanceOf(ApiError)
  })

  it('drops non-object entries from buckets and keeps the pipeline gate', async () => {
    stubFetch(
      async () =>
        new Response(
          JSON.stringify({
            buckets: [BUCKET, 'nope', null, 42],
            pipeline: { enabled: true, stale: false },
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
    )

    const series = await fetchModelPerfSeries('gpt-4o')

    expect(series.buckets).toEqual([BUCKET])
    expect(series.pipeline).toEqual({ enabled: true, stale: false })
  })
})
