import { toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'

/**
 * Public read-only contracts that the backend does not expose yet.
 *
 * `api/schema.d.ts` is generated from the frozen OpenAPI document, and the M6
 * backend has no public model-metrics route, so this module cannot go through
 * `apiClient.GET` without inventing a schema entry. The request/render shapes
 * below are transcribed from modules-09 F-09-25; the endpoints are registered
 * as gaps in the Task 6 report instead of being faked in production. When M7
 * lands `internal/publicapi`, this file is the single place to swap over to
 * the generated contract.
 *
 * Every call still funnels through `toApiError`/`parseProblem`, so failures
 * arrive in the same `ApiError` vocabulary as the contract client (web-00
 * §5.2) — no second error model leaks into the pages.
 */

export type PerfRange = '24h' | '7d'

export interface ModelMetricsSummary {
  avg_latency_ms: number | null
  success_rate: number | null
  tps: number | null
  failed_24h: number | null
  sample_size: number
}

export interface ModelMetricsGroup {
  group: string
  avg_ttft_ms: number | null
  avg_latency_ms: number | null
  success_rate: number | null
}

export interface ModelMetrics {
  summary: ModelMetricsSummary
  per_group: ModelMetricsGroup[]
}

export interface ReadyzProbe {
  status: string
  checks: Record<string, string>
  version?: string
  node?: string
}

/** Same-origin absolute URL; jsdom's Node fetch rejects relative URLs in tests. */
function apiUrl(path: string): string {
  const base = typeof location !== 'undefined' ? location.origin : 'http://localhost'
  return new URL(path, base).toString()
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function nullableNumber(value: unknown, field: string): number | null {
  if (value === null || value === undefined) return null
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error(`invalid metrics field: ${field}`)
  }
  return value
}

/**
 * Validates at the network boundary rather than inside the component: one bad
 * upstream row must degrade the section to its error state, never render
 * `NaN` into a statistic.
 */
export function normalizeModelMetrics(body: unknown): ModelMetrics {
  if (!isRecord(body) || !isRecord(body.summary) || !Array.isArray(body.per_group)) {
    throw new Error('invalid metrics response envelope')
  }
  const summary = body.summary
  const sampleSize = summary.sample_size
  if (typeof sampleSize !== 'number' || !Number.isFinite(sampleSize)) {
    throw new Error('invalid metrics field: summary.sample_size')
  }

  return {
    summary: {
      avg_latency_ms: nullableNumber(summary.avg_latency_ms, 'summary.avg_latency_ms'),
      success_rate: nullableNumber(summary.success_rate, 'summary.success_rate'),
      tps: nullableNumber(summary.tps, 'summary.tps'),
      failed_24h: nullableNumber(summary.failed_24h, 'summary.failed_24h'),
      sample_size: sampleSize,
    },
    per_group: body.per_group.map((entry, index) => {
      if (!isRecord(entry) || typeof entry.group !== 'string') {
        throw new Error(`invalid metrics field: per_group[${index}].group`)
      }
      return {
        group: entry.group,
        avg_ttft_ms: nullableNumber(entry.avg_ttft_ms, `per_group[${index}].avg_ttft_ms`),
        avg_latency_ms: nullableNumber(
          entry.avg_latency_ms,
          `per_group[${index}].avg_latency_ms`,
        ),
        success_rate: nullableNumber(entry.success_rate, `per_group[${index}].success_rate`),
      }
    }),
  }
}

/**
 * `GET /api/pricing/{model_id}/metrics?range=24h|7d` (modules-09 F-09-25).
 *
 * The documented route is used verbatim. The M7 plan currently sketches a
 * different aggregate path (`/api/public/pricing-metrics`); the mismatch is a
 * registered gap, not something this page may paper over with static numbers.
 */
export async function fetchModelMetrics(
  modelId: string,
  range: PerfRange,
  signal?: AbortSignal,
): Promise<ModelMetrics> {
  const path = `/api/pricing/${encodeURIComponent(modelId)}/metrics?range=${range}`
  let response: Response
  try {
    response = await fetch(apiUrl(path), {
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (error) {
    // Abort is a caller cancellation, not an offline condition.
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
  if (!response.ok) throw await toApiError(response)
  const body: unknown = await response.json().catch(() => undefined)
  return normalizeModelMetrics(body)
}

/**
 * `GET /readyz` (架构 §5.4) — a 503 still carries the probe envelope
 * (`status: "degraded"`), which is data the card must show rather than a
 * transport error. Only an unparseable body becomes an `ApiError`.
 */
export async function fetchReadyz(signal?: AbortSignal): Promise<ReadyzProbe> {
  let response: Response
  try {
    response = await fetch(apiUrl('/readyz'), {
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }

  const body: unknown = await response.json().catch(() => undefined)
  if (
    isRecord(body) &&
    typeof body.status === 'string' &&
    isRecord(body.checks)
  ) {
    const checks: Record<string, string> = {}
    for (const [key, value] of Object.entries(body.checks)) {
      if (typeof value === 'string') checks[key] = value
    }
    return {
      status: body.status,
      checks,
      version: typeof body.version === 'string' ? body.version : undefined,
      node: typeof body.node === 'string' ? body.node : undefined,
    }
  }

  throw response.ok
    ? parseProblem({ status: 500, body: { title: 'Invalid readiness response' } })
    : await toApiError(response)
}
