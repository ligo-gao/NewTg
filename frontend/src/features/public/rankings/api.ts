/**
 * 公开排行榜接口（D6a/D6b 已交付，契约见 schema.d.ts 的 RankingsResponse）。
 *
 * 【为什么这里用原始 fetch 而不是 apiClient.GET】D6a 的缓存契约是有 ETag
 * 的条件请求：命中时服务端返回 **304 无 body**。openapi-fetch 的中间件把
 * 所有非 2xx 统一抛成 ApiError，304 会在到达调用方之前被吞掉；这里需要
 * 读取响应头并把 304 映射回上一次的 body，所以直接使用 `fetch`。请求与
 * 错误仍然经过 `toApiError` / `parseProblem`，与契约客户端共用同一套
 * ApiError 词汇（web-00 §5.2），不会出现第二套错误模型。
 *
 * 【内存缓存】四周期各自缓存一份 ETag + body。React Query 负责组件级缓存，
 * 这里额外保留 ETag 是为了手动刷新（Hero Refresh）走条件请求；切换周期
 * 本身由 queryClient 的 staleTime=Infinity 保证不再发请求。
 */

import { toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'
import type { components } from '@/api/schema'

export type RankingsPeriod = components['schemas']['RankingsPeriod']
export type RankingsResponse = components['schemas']['RankingsResponse']
export type RankingsModel = components['schemas']['RankingsModel']
export type RankingsMover = components['schemas']['RankingsMover']
export type RankingsMovers = components['schemas']['RankingsMovers']
export type RankingsProvider = components['schemas']['RankingsProvider']
export type RankingsTrend = components['schemas']['RankingsTrend']
export type RankingsTrendSeries = components['schemas']['RankingsTrendSeries']
export type RankingsGranularity = components['schemas']['RankingsGranularity']

export const RANKINGS_PERIODS: readonly RankingsPeriod[] = [
  'today',
  'week',
  'month',
  'year',
]

export const DEFAULT_RANKINGS_PERIOD: RankingsPeriod = 'week'

/**
 * worker 的刷新间隔（后端 rankings.Period.RefreshInterval 的冻结值）：
 * today 5m、week/month 30m、year 6h。前端用它推导「新鲜度」三态，避免
 * 把 5 分钟周期的旧快照与 6 小时周期的旧快照按同一阈值判断。
 */
const REFRESH_INTERVAL_MS: Record<RankingsPeriod, number> = {
  today: 5 * 60 * 1000,
  week: 30 * 60 * 1000,
  month: 30 * 60 * 1000,
  year: 6 * 60 * 60 * 1000,
}

export type FreshnessState = 'fresh' | 'stale' | 'expired'

/**
 * 把冻结的刷新间隔折成「每 N 分钟 / 每 N 小时」，供「统计详情」的更新
 * 频率一行使用。页面不许写死「每日凌晨 2 点」这类与 worker 无关的文案。
 */
export function refreshCadence(period: RankingsPeriod): {
  unit: 'minutes' | 'hours'
  count: number
} {
  const minutes = REFRESH_INTERVAL_MS[period] / 60_000
  return minutes >= 60
    ? { unit: 'hours', count: minutes / 60 }
    : { unit: 'minutes', count: minutes }
}

/** fresh ≤ 2×刷新间隔；stale ≤ 24h；超过 24h 为 expired（UI §F-09-29）。 */
export function freshnessState(
  freshnessSeconds: number | null,
  period: RankingsPeriod,
): FreshnessState | null {
  if (freshnessSeconds === null || !Number.isFinite(freshnessSeconds)) return null
  if (freshnessSeconds <= (2 * REFRESH_INTERVAL_MS[period]) / 1000) return 'fresh'
  if (freshnessSeconds <= 24 * 60 * 60) return 'stale'
  return 'expired'
}

export function isRankingsPeriod(value: unknown): value is RankingsPeriod {
  return (
    typeof value === 'string' &&
    (RANKINGS_PERIODS as readonly string[]).includes(value)
  )
}

export function normalizeRankingsPeriod(value: unknown): RankingsPeriod {
  return isRankingsPeriod(value) ? value : DEFAULT_RANKINGS_PERIOD
}

export const rankingsQueryKey = (period: RankingsPeriod) =>
  ['public', 'rankings', period] as const

function apiUrl(): string {
  const base = typeof location !== 'undefined' ? location.origin : 'http://localhost'
  return base
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function requireString(value: unknown, field: string): string {
  if (typeof value !== 'string' || value === '') {
    throw new Error(`invalid rankings field: ${field}`)
  }
  return value
}

function requireNumber(value: unknown, field: string): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error(`invalid rankings field: ${field}`)
  }
  return value
}

function optionalNumber(value: unknown, field: string): number | null {
  if (value === null || value === undefined) return null
  return requireNumber(value, field)
}

function requireArray(value: unknown, field: string): unknown[] {
  if (!Array.isArray(value)) throw new Error(`invalid rankings field: ${field}`)
  return value
}

/**
 * 作者/厂商名允许为空：部署里渠道档案大量未填，服务端可能下发 `''`。
 * 进入视图前统一折成 `Unknown`（与派生模块同一契约），既不渲染空串，
 * 也不用模型名反推厂商。
 */
function requireAuthor(value: unknown, field: string): string {
  if (typeof value !== 'string') {
    throw new Error(`invalid rankings field: ${field}`)
  }
  const trimmed = value.trim()
  return trimmed === '' ? 'Unknown' : trimmed
}

/**
 * 网络边界校验：生成类型只在编译期有效，部署漂移时一个缺失的数组会让
 * 图表在渲染中崩溃（整页白屏）。这里把不合约的响应折成查询错误，由页面
 * 渲染可重试的错误态。
 */
export function normalizeRankings(body: unknown): RankingsResponse {
  if (!isRecord(body)) throw new Error('invalid rankings envelope')
  const status = body.status
  if (status !== 'ok' && status !== 'partial' && status !== 'empty') {
    throw new Error('invalid rankings field: status')
  }
  const period = normalizeRankingsPeriod(body.period)
  const window = body.window
  if (!isRecord(window)) throw new Error('invalid rankings field: window')
  const trend = body.trend
  if (!isRecord(trend)) throw new Error('invalid rankings field: trend')
  const granularity = trend.granularity
  if (granularity !== 'hour' && granularity !== 'day' && granularity !== 'week') {
    throw new Error('invalid rankings field: trend.granularity')
  }

  const models: RankingsModel[] = requireArray(body.models, 'models').map(
    (entry, index) => {
      if (!isRecord(entry)) throw new Error(`invalid models[${index}]`)
      return {
        rank: requireNumber(entry.rank, `models[${index}].rank`),
        model: requireString(entry.model, `models[${index}].model`),
        display_name: requireString(
          entry.display_name,
          `models[${index}].display_name`,
        ),
        author: requireAuthor(entry.author, `models[${index}].author`),
        total_tokens: requireNumber(
          entry.total_tokens,
          `models[${index}].total_tokens`,
        ),
        requests: requireNumber(entry.requests, `models[${index}].requests`),
        delta_tokens: optionalNumber(
          entry.delta_tokens,
          `models[${index}].delta_tokens`,
        ),
        delta_pct: optionalNumber(entry.delta_pct, `models[${index}].delta_pct`),
        prev_rank: optionalNumber(entry.prev_rank, `models[${index}].prev_rank`),
      }
    },
  )

  const movers = body.movers
  if (!isRecord(movers)) throw new Error('invalid rankings field: movers')
  const normalizeMovers = (value: unknown, field: string): RankingsMover[] =>
    requireArray(value, field).map((entry, index) => {
      if (!isRecord(entry)) throw new Error(`invalid ${field}[${index}]`)
      return {
        model: requireString(entry.model, `${field}[${index}].model`),
        author: requireAuthor(entry.author, `${field}[${index}].author`),
        delta_pct: requireNumber(entry.delta_pct, `${field}[${index}].delta_pct`),
        cur_tokens: requireNumber(
          entry.cur_tokens,
          `${field}[${index}].cur_tokens`,
        ),
        prev_tokens: requireNumber(
          entry.prev_tokens,
          `${field}[${index}].prev_tokens`,
        ),
      }
    })

  const providers: RankingsProvider[] = requireArray(
    body.providers,
    'providers',
  ).map((entry, index) => {
    if (!isRecord(entry)) throw new Error(`invalid providers[${index}]`)
    return {
      rank: requireNumber(entry.rank, `providers[${index}].rank`),
      author: requireAuthor(entry.author, `providers[${index}].author`),
      total_tokens: requireNumber(
        entry.total_tokens,
        `providers[${index}].total_tokens`,
      ),
      share_pct: requireNumber(entry.share_pct, `providers[${index}].share_pct`),
      slot: optionalNumber(entry.slot, `providers[${index}].slot`),
    }
  })

  const buckets = requireArray(trend.buckets, 'trend.buckets').map(
    (entry, index) => requireString(entry, `trend.buckets[${index}]`),
  )
  const series: RankingsTrendSeries[] = requireArray(
    trend.series,
    'trend.series',
  ).map((entry, index) => {
    if (!isRecord(entry)) throw new Error(`invalid trend.series[${index}]`)
    const tokens = requireArray(
      entry.tokens,
      `trend.series[${index}].tokens`,
    ).map((value, tokenIndex) =>
      requireNumber(value, `trend.series[${index}].tokens[${tokenIndex}]`),
    )
    if (tokens.length !== buckets.length) {
      throw new Error(`invalid trend.series[${index}].tokens length`)
    }
    return {
      model: requireString(entry.model, `trend.series[${index}].model`),
      slot: optionalNumber(entry.slot, `trend.series[${index}].slot`),
      tokens,
    }
  })

  return {
    period,
    timezone: requireString(body.timezone, 'timezone'),
    window: {
      start: requireString(window.start, 'window.start'),
      end: requireString(window.end, 'window.end'),
    },
    computed_at:
      body.computed_at === null || body.computed_at === undefined
        ? null
        : requireString(body.computed_at, 'computed_at'),
    freshness_seconds: optionalNumber(
      body.freshness_seconds,
      'freshness_seconds',
    ),
    sample_threshold: requireNumber(body.sample_threshold, 'sample_threshold'),
    status,
    models,
    movers: {
      up: normalizeMovers(movers.up, 'movers.up'),
      down: normalizeMovers(movers.down, 'movers.down'),
    },
    providers,
    trend: { granularity, buckets, series },
  }
}

interface CachedRankings {
  etag: string
  data: RankingsResponse
}

/**
 * 304 复用时服务端不会重发 body，缓存的 `freshness_seconds` 停留在第一次
 * 响应时刻。若不按 `computed_at` 与本地 now 重算，页面会把一份几小时前的
 * 快照一直标成 fresh（freshness 三态永远不推进）。重算失败（无
 * computed_at 或非法时间）时保留服务端原值，不猜造读数。
 */
export function recomputeFreshness(data: RankingsResponse): RankingsResponse {
  if (data.computed_at === null) return data
  const computedAt = Date.parse(data.computed_at)
  if (Number.isNaN(computedAt)) return data
  const seconds = Math.max(0, Math.floor((Date.now() - computedAt) / 1000))
  return { ...data, freshness_seconds: seconds }
}

/** 每个周期只保留最新一份条件缓存；页面生命周期内有效。 */
const etagCache = new Map<RankingsPeriod, CachedRankings>()

/** 测试钩子：模块级缓存在用例之间必须清空。 */
export function clearRankingsEtagCache(): void {
  etagCache.clear()
}

export async function fetchRankings(
  period: RankingsPeriod,
  signal?: AbortSignal,
): Promise<RankingsResponse> {
  const cached = etagCache.get(period)
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (cached !== undefined) headers['If-None-Match'] = cached.etag

  let response: Response
  try {
    response = await fetch(
      new URL(`/api/rankings?period=${period}`, apiUrl()).toString(),
      { credentials: 'same-origin', headers, signal },
    )
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }

  if (response.status === 304) {
    // 304 没有 body；只有本地确实有同一周期的缓存时才可能发生。
    if (cached === undefined) {
      throw parseProblem({
        status: 500,
        body: { title: 'Unexpected 304 for rankings' },
      })
    }
    const data = recomputeFreshness(cached.data)
    // 缓存里也写回重算后的读数，下一次 304 基于同一份基准继续推进。
    etagCache.set(period, { etag: cached.etag, data })
    return data
  }
  if (!response.ok) throw await toApiError(response)

  const data = normalizeRankings(await response.json().catch(() => undefined))
  const etag = response.headers.get('ETag')
  if (etag !== null && etag !== '') {
    etagCache.set(period, { etag, data })
  } else {
    // 冷启动快照没有 ETag（契约：无快照不下发），下一次刷新必须发完整请求。
    etagCache.delete(period)
  }
  return data
}

export function rankingsQueryOptions(period: RankingsPeriod) {
  return {
    queryKey: rankingsQueryKey(period),
    queryFn: ({ signal }: { signal: AbortSignal }) => fetchRankings(period, signal),
    // 切回已加载周期不重复请求（F-09-29 状态机）。手动刷新由 refetch 触发，
    // staleTime 不参与该判断。
    staleTime: Infinity,
    gcTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnMount: false,
    retry: false,
  }
}
