/**
 * 性能管理传输层（UI §6.12 / modules-08 §3.19，F-08-48～F-08-50）。
 *
 * 全部端点走生成的 `apiClient`，所以错误统一是 `ApiError`，写请求自动带
 * CSRF，响应形状在编译期与 `api/schema.d.ts` 绑定。
 *
 * 三个写操作在契约里把 `X-Confirm-Action` 声明为**必填头**：前端必须显式
 * 传 "true"。它和页面里的 DangerConfirm 是两道独立的门——弹窗只是体验层，
 * 服务端收到的头才是授权门（缺失时 422 confirm_required）。因此这里的写
 * 函数只接受「已确认」的调用，不提供默认值或可选头。
 */
import { apiClient, toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import {
  createIdempotencyKey,
  fetchSettingsSection,
  saveSettingsSection,
  type SettingsSectionResponse,
  type SettingsValuesResponse,
} from '@/features/admin/settings/api'
import {
  triggerSystemTask,
  type SystemTaskTriggerResult,
} from '@/features/admin/system/system-tasks-api'

export type PerformanceStats = components['schemas']['PerformanceStats']
export type PerformanceDiskCacheClearResult =
  components['schemas']['PerformanceDiskCacheClearResult']
export type PerformanceGCResult = components['schemas']['PerformanceGCResult']
export type PerformanceLogs = components['schemas']['PerformanceLogs']
export type PerformanceLogFile = components['schemas']['PerformanceLogFile']
export type PerformanceLogCleanupResult =
  components['schemas']['PerformanceLogCleanupResult']
export type PerformanceRecentOperations =
  components['schemas']['PerformanceRecentOperations']
export type PerformanceRecentOperation =
  components['schemas']['PerformanceRecentOperation']
export type CleanupMode = 'by_count' | 'by_days'

/**
 * 手动触发一轮测速。走的是既有的任务触发端点
 * `POST /api/system/tasks/model_perf_test:trigger`（spec §6.5 第 6 条：
 * 手动触发不取时段锁，直接入队由 worker 按租约执行），本页不新增后端动作。
 *
 * 返回 202 只代表**已入队**：一轮要覆盖全部公开对话模型，通常 1～2 分钟才
 * 出结果，所以调用方应提示「已排队」并等下一次 models 轮询，不能立刻断言
 * 表格已更新。
 */
export const MODEL_PERF_TASK = 'model_perf_test'

export async function triggerModelPerfRun(
  idempotencyKey: string,
): Promise<SystemTaskTriggerResult> {
  return triggerSystemTask(MODEL_PERF_TASK, idempotencyKey)
}

/**
 * 三个观测端点（Task 8–10 后端 / Task 11 契约）的生成类型别名。
 *
 * 页面只引用别名，不再逐个写 `components['schemas'][…]`；形状以
 * `api/schema.d.ts` 为唯一来源，后端加字段时类型自动跟上。
 */
export type PerfOverview = components['schemas']['PerformanceOverview']
export type PerfModels = components['schemas']['PerformanceModels']
export type PerfConfig = components['schemas']['PerformanceConfig']

/** 总览的时间窗；与契约的 `range` 枚举逐字一致（大小写敏感）。 */
export type OverviewRange = '15m' | '1h' | '6h' | '24h'

/** 三个写操作共用的服务端强制确认头。 */
const CONFIRM_HEADER = { 'X-Confirm-Action': 'true' as const }

export async function fetchPerformanceStats(
  signal?: AbortSignal,
): Promise<PerformanceStats> {
  return requireData(
    await apiClient.GET('/api/performance/stats', { signal }),
  )
}

export async function fetchPerformanceLogs(
  signal?: AbortSignal,
): Promise<PerformanceLogs> {
  return requireData(await apiClient.GET('/api/performance/logs', { signal }))
}

export async function fetchRecentOperations(
  signal?: AbortSignal,
): Promise<PerformanceRecentOperations> {
  return requireData(
    await apiClient.GET('/api/performance/recent-operations', {
      params: { query: { limit: 20 } },
      signal,
    }),
  )
}

/**
 * 写请求回传当前实例（契约：磁盘缓存/日志清理用 query，GC 用 body）。
 *
 * 页面展示的统计来自某一次 `GET /stats`，写操作却可能落在另一个副本上。
 * 把页面钉住的实例标识随写请求回传，服务端就能在实例漂移时返回 409
 * `instance_mismatch` 而不是静默操作错误的节点。没有 stats（首次加载失败）
 * 时不传：此时页面没有可回传的实例，伪造一个反而会引入误判。
 */
export async function clearDiskCache(
  instanceId?: string,
): Promise<PerformanceDiskCacheClearResult> {
  return requireData(
    await apiClient.DELETE('/api/performance/disk-cache', {
      params: {
        query:
          instanceId === undefined ? {} : { instance_id: instanceId },
        header: CONFIRM_HEADER,
      },
    }),
  )
}

export async function runGarbageCollection(
  instanceId?: string,
): Promise<PerformanceGCResult> {
  return requireData(
    await apiClient.POST('/api/performance/gc', {
      params: { header: CONFIRM_HEADER },
      body: instanceId === undefined ? {} : { instance_id: instanceId },
    }),
  )
}

export async function cleanupLogs(
  mode: CleanupMode,
  value: number,
  instanceId?: string,
): Promise<PerformanceLogCleanupResult> {
  return requireData(
    await apiClient.DELETE('/api/performance/logs', {
      params: {
        query: {
          mode,
          value,
          ...(instanceId === undefined ? {} : { instance_id: instanceId }),
        },
        header: CONFIRM_HEADER,
      },
    }),
  )
}

/** 总览：range 是页面时间窗，缺省由服务端取 `1h`。 */
export async function fetchPerfOverview(
  range: OverviewRange,
  signal?: AbortSignal,
): Promise<PerfOverview> {
  return requireData(
    await apiClient.GET('/api/performance/overview', {
      params: { query: { range } },
      signal,
    }),
  )
}

/**
 * 模型汇总。表格窗口固定 24h（契约只接受空或 `24h`），调用方没有窗口参数，
 * 所以这里不传 query：让服务端使用默认值，避免把「默认」在客户端再写一遍。
 */
export async function fetchPerfModels(
  signal?: AbortSignal,
): Promise<PerfModels> {
  return requireData(
    await apiClient.GET('/api/performance/models', { signal }),
  )
}

export async function fetchPerfConfig(
  signal?: AbortSignal,
): Promise<PerfConfig> {
  return requireData(
    await apiClient.GET('/api/performance/config', { signal }),
  )
}

/**
 * 测速四项（G 键）走既有 `/api/settings?section=perf`，不新增读写端点；
 * 这里只是把 section 名收敛到一处，返回既有的 `SettingsSectionResponse`。
 */
export async function fetchPerfSettings(
  signal?: AbortSignal,
): Promise<SettingsSectionResponse> {
  return fetchSettingsSection('perf', signal)
}

/**
 * 保存测速参数。`expectUpdatedAt` 是上次 GET 的 `updated_at`；服务端 409 表示
 * 他人先改，调用方应重拉而不是覆盖。`saveSettingsSection` 自己带 CSRF，
 * 本函数只负责 mint 幂等键（一次保存意图一个键，重试不会重复写）。
 */
export async function savePerfSettings(
  values: Record<string, unknown>,
  expectUpdatedAt: string | null,
): Promise<SettingsValuesResponse> {
  return saveSettingsSection(
    { section: 'perf', values, expect_updated_at: expectUpdatedAt },
    createIdempotencyKey(),
  )
}

/* ------------------------------------------------------------------------- *
 * 单模型 24h 分桶序列（手写类型）
 * ------------------------------------------------------------------------- */

/**
 * `GET /api/model-perf` **尚无 OpenAPI 契约**，所以这里不能引用
 * `schema.d.ts` 的生成类型。字段以后端结构体为准：
 * `backend/internal/httpapi/handlers_model_perf.go` 的 `modelPerfBucketPoint`
 * / `modelPerfPipeline`（`modelPerfResponse` 的 `buckets` 与 `pipeline`）。
 * 契约落地后应改为 `components['schemas'][…]`，并删掉手写定义。
 *
 * `start` 是桶起点的 RFC3339 时间戳；`*_over_30s` 为真时同级 `*_ms` 只是
 * 「已知 ≥30s」的下界，不是有限延迟。
 */
export interface ModelPerfBucket {
  start: string
  model: string
  requests: number
  ttft_p50_ms: number | null
  ttft_p95_ms: number | null
  ttft_p50_over_30s: boolean
  ttft_p95_over_30s: boolean
}

/**
 * 下钻只需要 24h 分桶与采集管道闸门；`modelPerfResponse` 的其余字段
 * （window / summary / incidents / models / comparison / dimensions）在此丢弃。
 */
export interface ModelPerfSeries {
  buckets: ModelPerfBucket[]
  pipeline: { enabled: boolean; stale: boolean; reason?: string }
}

interface ModelPerfSeriesBody {
  buckets?: ModelPerfBucket[]
  pipeline?: ModelPerfSeries['pipeline']
}

/** jsdom/Node 的 fetch 拒绝相对 URL；浏览器里与同源相对路径等价。 */
function absoluteUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

/**
 * 单模型下钻序列。端点不在契约里，所以绕过 `apiClient` 直接 `fetch`（与
 * `features/admin/channels/api.ts` 的 `dispatch` 同一写法）；错误仍归一成
 * 统一形状：非 2xx 走 `toApiError`，传输层失败 throw
 * `parseProblem({status: 0})`（`kind: 'offline'`），AbortError 原样抛出。
 *
 * `range=24h` 是这一屏的固定口径（与模型表 24h 窗口、demo 副标题一致），
 * 调用方不接受窗口参数。响应只保留 `buckets` 与 `pipeline`：管道缺失时
 * 按「未启用」处理，绝不把空对象当成健康。
 */
export async function fetchModelPerfSeries(
  model: string,
  signal?: AbortSignal,
): Promise<ModelPerfSeries> {
  const url = absoluteUrl(
    `/api/model-perf?range=24h&model=${encodeURIComponent(model)}`,
  )
  let response: Response
  try {
    response = await fetch(url, {
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (error) {
    // 调用方取消不是传输失败：让 abort 语义原样向上传递。
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
  if (!response.ok) throw await toApiError(response)

  const body = (await response.json()) as ModelPerfSeriesBody
  return {
    // 响应来自无契约端点：非对象项直接丢弃，避免坏数据把渲染器打崩。
    buckets: Array.isArray(body.buckets)
      ? body.buckets.filter(
          (bucket): bucket is ModelPerfBucket =>
            typeof bucket === 'object' && bucket !== null,
        )
      : [],
    pipeline: body.pipeline ?? { enabled: false, stale: true },
  }
}
