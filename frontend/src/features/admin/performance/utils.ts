/**
 * 性能管理的纯函数：格式化、阈值分级、日志清理预览。
 *
 * 把这些逻辑留在 React 之外，是为了让「预览只计算不删除」可以被单测直接
 * 钉住——预览若走接口就会产生副作用，而设计稿明确要求预览纯粹是本地计算
 * （UI §6.12 日志卡）。
 */
import type { ApiError } from '@/api/errors'

import type { CleanupMode, PerformanceLogFile } from './api'

/** 与后端 performanceMaxCleanupValue 一致的冻结上限（100 年）。 */
export const MAX_CLEANUP_VALUE = 36_500

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB'] as const

/**
 * 字节展示：B 取整，KB 及以上保留 1 位小数（UI §6.12「字节数等宽并保留
 * 1 位小数」）。非有限/负数返回占位符，避免把 NaN 写进统计行。
 */
export function formatBytes(bytes: number, locale: string): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes < 1024) {
    return `${new Intl.NumberFormat(locale, {
      maximumFractionDigits: 0,
    }).format(bytes)} ${BYTE_UNITS[0]}`
  }
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(value)} ${BYTE_UNITS[unit] ?? 'B'}`
}

/** 计数列（文件数/GC 次数/协程数）用分组格式，不做缩写。 */
export function formatCount(value: number, locale: string): string {
  if (!Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}

export function formatDecimal(value: number, locale: string): string {
  if (!Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(value)
}

/**
 * 磁盘缓存用量百分比。上限来自部署变量 max_mb，0/缺失表示「未设上限」：
 * 此时没有可比较的分母，按 0% 显示，不伪造 100%。
 */
export function diskUsagePercent(totalBytes: number, maxMb: number): number {
  const limit = maxMb * 1024 * 1024
  if (!Number.isFinite(limit) || limit <= 0 || !Number.isFinite(totalBytes)) {
    return 0
  }
  return Math.min(100, Math.max(0, (totalBytes / limit) * 100))
}

export type UsageTone = 'default' | 'warning' | 'danger'

/**
 * 阈值色（UI §0.5 / §6.12）：<70% 默认、70–90% warning、>90% danger。
 * 90% 整落在 warning 区间内（70–90 为闭区间），只有严格大于 90 才转红。
 */
export function usageTone(percent: number): UsageTone {
  if (percent > 90) return 'danger'
  if (percent >= 70) return 'warning'
  return 'default'
}

export function usageBarClass(tone: UsageTone): string {
  if (tone === 'danger') return 'bg-danger'
  if (tone === 'warning') return 'bg-warning'
  return 'bg-brand'
}

/** 等宽数字列用：时间戳无效时统一显示占位符。 */
export function formatDateTime(iso: string | null | undefined, locale: string): string {
  if (iso === null || iso === undefined || iso === '') return '—'
  const timestamp = Date.parse(iso)
  if (!Number.isFinite(timestamp)) return '—'
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(timestamp)
}

/** 页头「上次更新」只显示本地时钟，日期变化由页面标题上下文提供。 */
export function formatClock(timestamp: number, locale: string): string {
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '—'
  return new Intl.DateTimeFormat(locale, { timeStyle: 'medium' }).format(timestamp)
}

/** 结果行的毫秒耗时。 */
export function formatMilliseconds(ms: number, locale: string): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  return `${formatCount(ms, locale)} ms`
}

export type CleanupValueError = 'required' | 'range' | undefined

/**
 * 清理保留值校验：1–36500 的整数（与后端同一上限）。空输入与越界分开返回，
 * 因为前者是「还没填」、后者是「填错了」，提示文案不同。
 */
export function validateCleanupValue(raw: string): {
  value?: number
  error: CleanupValueError
} {
  const trimmed = raw.trim()
  if (trimmed === '') return { error: 'required' }
  if (!/^\d+$/.test(trimmed)) return { error: 'range' }
  const value = Number(trimmed)
  if (!Number.isSafeInteger(value) || value < 1 || value > MAX_CLEANUP_VALUE) {
    return { error: 'range' }
  }
  return { value, error: undefined }
}

export interface LogCleanupPreview {
  count: number
  bytes: number
  activeSkipped: boolean
}

/**
 * 日志清理的**本地预览**（不调用删除接口）：
 * - by_count：保留 mtime 最新的 value 个文件，其余计入删除集；
 * - by_days：删除 mtime 早于 now - value*24h 的文件；
 * - 当前活动文件（最新 mtime）在两种模式下都永不删除，命中删除集时剔除
 *   并标记 activeSkipped。
 *
 * 与后端同一口径的关键点：活动文件按最新 mtime 判定（mtime 是唯一跨平台
 * 可用的近似），因此预览不会把「即将被保护」的文件算进删除量。
 */
export function previewLogCleanup(
  files: readonly PerformanceLogFile[],
  mode: CleanupMode,
  value: number,
  nowMs: number,
): LogCleanupPreview {
  if (!Number.isSafeInteger(value) || value < 1) {
    return { count: 0, bytes: 0, activeSkipped: false }
  }

  const entries = files
    .map((file) => ({ file, time: Date.parse(file.mod_time) }))
    .filter((entry) => Number.isFinite(entry.time))

  // 活动文件 = 最新 mtime。并列时取第一个，保证预览与后端单选一致。
  let active: (typeof entries)[number] | undefined
  for (const entry of entries) {
    if (active === undefined || entry.time > active.time) active = entry
  }

  let candidates: typeof entries
  if (mode === 'by_count') {
    candidates = [...entries]
      .sort((left, right) => right.time - left.time)
      .slice(value)
  } else {
    const cutoff = nowMs - value * 24 * 60 * 60 * 1000
    candidates = entries.filter((entry) => entry.time < cutoff)
  }

  let count = 0
  let bytes = 0
  let activeSkipped = false
  for (const entry of candidates) {
    if (active !== undefined && entry.file === active.file) {
      activeSkipped = true
      continue
    }
    count += 1
    bytes += Number.isFinite(entry.file.size) ? entry.file.size : 0
  }
  return { count, bytes, activeSkipped }
}

/**
 * 从 Problem 响应提取稳定错误码。后端把码同时放在 `type` 的末段
 * （https://newtg.dev/problems/<code>）与 detail 的 `CODE:` 前缀；两者取
 * 其一即可，优先 detail 前缀以便跨代理改写 type 时仍能识别。
 */
export function problemCode(error: ApiError): string {
  const detailCode = /^([A-Z][A-Z0-9_]*):/.exec(error.detail ?? '')?.[1]
  if (detailCode !== undefined) return detailCode.toLowerCase()
  const type = error.type
  return type.slice(type.lastIndexOf('/') + 1)
}

/** 页面能本地化映射的错误码集合；其余码退回通用文案。 */
export const KNOWN_PROBLEM_CODES = new Set([
  'confirm_required',
  'instance_mismatch',
  'invalid_mode',
  'invalid_value',
  'disk_cache_not_configured',
  'log_dir_not_configured',
])
