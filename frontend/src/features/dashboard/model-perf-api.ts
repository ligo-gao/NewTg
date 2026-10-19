import { toApiError } from '@/api/client'
import { ApiError, parseProblem } from '@/api/errors'

/**
 * F-05-08 模型性能端点（modules-05 UI §8.9）。
 *
 * OpenAPI 冻结稿尚未收录 `GET /api/model-perf`（与日志导出同一处置），
 * 因此走受控原始 fetch；失败仍归一为 `ApiError`。看板只消费这一个聚合读
 * 接口：浏览器不直连指标库、不接触 `METRICS_TOKEN`。
 *
 * 空值语义是契约的一部分：无样本字段为 `null`（前端渲染 `—`），绝不能在
 * 客户端把它们补成 0。
 */

export type ModelPerfRange = '24h' | '7d'
export type ModelPerfDimension = 'channel' | 'upstream'
export type ModelPerfBadge =
  | 'fast'
  | 'healthy'
  | 'slow'
  | 'degraded'
  | 'incident'
  | 'no_data'

export interface ModelPerfWindow {
  range: string
  start: string
  end: string
  granularity: string
}

export interface ModelPerfSummary {
  ttft_p50_ms: number | null
  ttft_p95_ms: number | null
  latency_p50_ms: number | null
  latency_p95_ms: number | null
  success_rate: number | null
  refused: number
  uptime: number | null
  incidents: number | null
  tps: number | null
  sample_size: number | null
  ttft_p95_over_30s: boolean
  latency_p95_over_30s: boolean
}

export interface ModelPerfBucketPoint {
  start: string
  model: string
  requests: number
  ttft_p50_ms: number | null
  ttft_p95_ms: number | null
  latency_p50_ms: number | null
  latency_p95_ms: number | null
  success_rate: number | null
  uptime: number | null
  tps: number | null
  /**
   * +Inf 桶标记：值为 30000 时表示「下界 >30s」，不是伪造的 30.0s。
   * summary / bucket / model 行均可能下发（handlers_model_perf.go）。
   */
  ttft_p95_over_30s?: boolean
  ttft_p50_over_30s?: boolean
  latency_p95_over_30s?: boolean
  latency_p50_over_30s?: boolean
}

export interface ModelPerfIncident {
  start: string
  end: string
  duration_seconds: number
  severity: string
  reason: string
}

export interface ModelPerfModelRow {
  model: string
  ttft_p50_ms: number | null
  ttft_p95_ms: number | null
  latency_p95_ms: number | null
  success_rate: number | null
  uptime: number | null
  incidents: number
  sample_size: number
  badge: ModelPerfBadge
  ttft_p95_over_30s?: boolean
  ttft_p50_over_30s?: boolean
  latency_p95_over_30s?: boolean
  latency_p50_over_30s?: boolean
}

export interface ModelPerfPipeline {
  enabled: boolean
  updated_at: string | null
  stale: boolean
}

export interface ModelPerfComparison {
  pipeline_success_rate: number | null
  usage_logs_success_rate: number | null
  delta: number | null
}

export interface ModelPerfDimensionRow {
  dimension: ModelPerfDimension
  key: string
  requests: number
  success_rate: number | null
  latency_p95_ms: number | null
  ttft_p95_ms: number | null
  uptime: number | null
}

export interface ModelPerfResponse {
  window: ModelPerfWindow
  buckets: ModelPerfBucketPoint[]
  summary: ModelPerfSummary
  incidents: ModelPerfIncident[]
  models: ModelPerfModelRow[]
  pipeline: ModelPerfPipeline
  comparison: ModelPerfComparison | null
  dimensions?: ModelPerfDimensionRow[]
}

export interface ModelPerfQuery {
  range: ModelPerfRange
  model?: string
  /** admin-only；user 传该参数后端返回 403（前端默认不渲染维度切换）。 */
  dimension?: ModelPerfDimension
  signal?: AbortSignal
}

function apiUrl(path: string): string {
  const base =
    typeof location !== 'undefined' ? location.origin : 'http://localhost'
  return new URL(path, base).toString()
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null
    ? (value as Record<string, unknown>)
    : undefined
}

function numberOrNull(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function numberOrZero(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

function booleanOrFalse(value: unknown): boolean {
  return value === true
}

function parseSummary(raw: unknown): ModelPerfSummary {
  const source = asRecord(raw) ?? {}
  return {
    ttft_p50_ms: numberOrNull(source.ttft_p50_ms),
    ttft_p95_ms: numberOrNull(source.ttft_p95_ms),
    latency_p50_ms: numberOrNull(source.latency_p50_ms),
    latency_p95_ms: numberOrNull(source.latency_p95_ms),
    success_rate: numberOrNull(source.success_rate),
    refused: numberOrZero(source.refused),
    uptime: numberOrNull(source.uptime),
    incidents: numberOrNull(source.incidents),
    tps: numberOrNull(source.tps),
    sample_size: numberOrNull(source.sample_size),
    ttft_p95_over_30s: booleanOrFalse(source.ttft_p95_over_30s),
    latency_p95_over_30s: booleanOrFalse(source.latency_p95_over_30s),
  }
}

/**
 * 边界校验只做「不崩溃」级别：缺字段按空值处理，非数字按 null/0 处理。
 * 指标库的一条坏行不能让整块看板白屏，但也绝不把坏值渲染成看似真实的
 * 数字（GC-2 数据面纪律）。
 */
export function parseModelPerfResponse(raw: unknown): ModelPerfResponse {
  const source = asRecord(raw)
  if (source === undefined) {
    throw new Error('model perf: response is not an object')
  }
  const pipeline = asRecord(source.pipeline) ?? {}
  const window = asRecord(source.window) ?? {}

  const buckets = Array.isArray(source.buckets)
    ? source.buckets.flatMap((item): ModelPerfBucketPoint[] => {
        const bucket = asRecord(item)
        if (bucket === undefined || typeof bucket.start !== 'string') return []
        return [
          {
            start: bucket.start,
            model: typeof bucket.model === 'string' ? bucket.model : '',
            requests: numberOrZero(bucket.requests),
            ttft_p50_ms: numberOrNull(bucket.ttft_p50_ms),
            ttft_p95_ms: numberOrNull(bucket.ttft_p95_ms),
            latency_p50_ms: numberOrNull(bucket.latency_p50_ms),
            latency_p95_ms: numberOrNull(bucket.latency_p95_ms),
            success_rate: numberOrNull(bucket.success_rate),
            uptime: numberOrNull(bucket.uptime),
            tps: numberOrNull(bucket.tps),
            ttft_p95_over_30s: booleanOrFalse(bucket.ttft_p95_over_30s),
            ttft_p50_over_30s: booleanOrFalse(bucket.ttft_p50_over_30s),
            latency_p95_over_30s: booleanOrFalse(bucket.latency_p95_over_30s),
            latency_p50_over_30s: booleanOrFalse(bucket.latency_p50_over_30s),
          },
        ]
      })
    : []

  const models = Array.isArray(source.models)
    ? source.models.flatMap((item): ModelPerfModelRow[] => {
        const model = asRecord(item)
        if (model === undefined || typeof model.model !== 'string') return []
        return [
          {
            model: model.model,
            ttft_p50_ms: numberOrNull(model.ttft_p50_ms),
            ttft_p95_ms: numberOrNull(model.ttft_p95_ms),
            latency_p95_ms: numberOrNull(model.latency_p95_ms),
            success_rate: numberOrNull(model.success_rate),
            uptime: numberOrNull(model.uptime),
            incidents: numberOrZero(model.incidents),
            sample_size: numberOrZero(model.sample_size),
            badge: typeof model.badge === 'string'
              ? (model.badge as ModelPerfBadge)
              : 'no_data',
            ttft_p95_over_30s: booleanOrFalse(model.ttft_p95_over_30s),
            ttft_p50_over_30s: booleanOrFalse(model.ttft_p50_over_30s),
            latency_p95_over_30s: booleanOrFalse(model.latency_p95_over_30s),
            latency_p50_over_30s: booleanOrFalse(model.latency_p50_over_30s),
          },
        ]
      })
    : []

  const incidents = Array.isArray(source.incidents)
    ? source.incidents.flatMap((item): ModelPerfIncident[] => {
        const incident = asRecord(item)
        if (
          incident === undefined ||
          typeof incident.start !== 'string' ||
          typeof incident.end !== 'string'
        ) {
          return []
        }
        return [
          {
            start: incident.start,
            end: incident.end,
            duration_seconds: numberOrZero(incident.duration_seconds),
            severity:
              typeof incident.severity === 'string'
                ? incident.severity
                : 'minor',
            reason: typeof incident.reason === 'string' ? incident.reason : '',
          },
        ]
      })
    : []

  const dimensions = Array.isArray(source.dimensions)
    ? source.dimensions.flatMap((item): ModelPerfDimensionRow[] => {
        const dimension = asRecord(item)
        if (dimension === undefined || typeof dimension.key !== 'string') {
          return []
        }
        return [
          {
            dimension:
              dimension.dimension === 'upstream' ? 'upstream' : 'channel',
            key: dimension.key,
            requests: numberOrZero(dimension.requests),
            success_rate: numberOrNull(dimension.success_rate),
            latency_p95_ms: numberOrNull(dimension.latency_p95_ms),
            ttft_p95_ms: numberOrNull(dimension.ttft_p95_ms),
            uptime: numberOrNull(dimension.uptime),
          },
        ]
      })
    : undefined

  const comparisonRaw = asRecord(source.comparison)
  const comparison =
    comparisonRaw === undefined
      ? null
      : {
          pipeline_success_rate: numberOrNull(
            comparisonRaw.pipeline_success_rate,
          ),
          usage_logs_success_rate: numberOrNull(
            comparisonRaw.usage_logs_success_rate,
          ),
          delta: numberOrNull(comparisonRaw.delta),
        }

  return {
    window: {
      range: typeof window.range === 'string' ? window.range : '24h',
      start: typeof window.start === 'string' ? window.start : '',
      end: typeof window.end === 'string' ? window.end : '',
      granularity:
        typeof window.granularity === 'string' ? window.granularity : '1m',
    },
    buckets,
    summary: parseSummary(source.summary),
    incidents,
    models,
    pipeline: {
      enabled: pipeline.enabled === true,
      updated_at:
        typeof pipeline.updated_at === 'string' ? pipeline.updated_at : null,
      stale: pipeline.stale === true,
    },
    comparison,
    dimensions,
  }
}

export async function fetchModelPerf(
  query: ModelPerfQuery,
): Promise<ModelPerfResponse> {
  const params = new URLSearchParams({ range: query.range })
  const model = query.model?.trim()
  if (model !== undefined && model !== '') params.set('model', model)
  if (query.dimension !== undefined) params.set('dimension', query.dimension)

  let response: Response
  try {
    response = await fetch(apiUrl(`/api/model-perf?${params.toString()}`), {
      method: 'GET',
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal: query.signal,
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }

  if (!response.ok) throw await toApiError(response)

  const body: unknown = await response.json()
  return parseModelPerfResponse(body)
}

/** 维度请求被服务端拒绝时，区块隐藏并提示管理员权限（UI §8.7）。 */
export function isForbiddenDimensionError(error: unknown): boolean {
  return error instanceof ApiError && error.kind === 'forbidden'
}
