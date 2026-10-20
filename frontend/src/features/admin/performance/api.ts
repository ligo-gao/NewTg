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
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

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
