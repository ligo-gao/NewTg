/**
 * 性能管理的纯函数：格式化、阈值分级、日志清理预览。
 *
 * 把这些逻辑留在 React 之外，是为了让「预览只计算不删除」可以被单测直接
 * 钉住——预览若走接口就会产生副作用，而设计稿明确要求预览纯粹是本地计算
 * （UI §6.12 日志卡）。
 */
import type { ApiError } from '@/api/errors'

import type { CleanupMode, PerformanceLogFile, PerfOverview } from './api'

/** 与后端 performanceMaxCleanupValue 一致的冻结上限（100 年）。 */
export const MAX_CLEANUP_VALUE = 36_500

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB'] as const

/**
 * 字节展示：B 取整，KB 及以上保留 1 位小数（UI §6.12「字节数等宽并保留
 * 1 位小数」）。null 表示无样本、非有限/负数返回占位符，避免把 NaN 或
 * 伪造的 0 写进统计行。
 */
export function formatBytes(bytes: number | null, locale: string): string {
  if (bytes === null || !Number.isFinite(bytes) || bytes < 0) return '—'
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

/** 计数列（文件数/GC 次数/协程数）用分组格式，不做缩写。null = 未知。 */
export function formatCount(value: number | null, locale: string): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}

export function formatDecimal(value: number | null, locale: string): string {
  if (value === null || !Number.isFinite(value)) return '—'
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

  // 排序口径必须与服务端 sortPerfFilesDesc 完全一致：mtime 倒序，mtime 相同
  // 时按文件名倒序。少了 tie-break，前端「活动文件」与 by_count 的删除集在
  // 同一秒写入多个文件的场景下会和服务端不同，预览数量随之失真。
  const sorted = [...entries].sort((left, right) => {
    if (left.time !== right.time) return right.time - left.time
    if (left.file.name === right.file.name) return 0
    return left.file.name < right.file.name ? 1 : -1
  })

  // 活动文件 = 排序后的首元素（最新 mtime；并列时文件名最大者）。
  const active = sorted[0]

  let candidates: typeof entries
  if (mode === 'by_count') {
    candidates = sorted.slice(value)
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

/* ------------------------------------------------------------------------- *
 * 模型状态与体检格状态 → 徽标色调 / 文案键
 * ------------------------------------------------------------------------- */

/**
 * 模型状态（契约枚举；判定顺序由服务端固定为 incident → degraded →
 * no_sample → normal，这里的数组只用于遍历/筛选，不承载判定逻辑）。
 */
export const PERF_STATUSES = [
  'normal',
  'degraded',
  'incident',
  'no_sample',
] as const

export type PerfStatus = (typeof PERF_STATUSES)[number]
export type PerfTone = 'success' | 'warning' | 'danger' | 'neutral'

const PERF_STATUS_TONES: Record<PerfStatus, PerfTone> = {
  normal: 'success',
  degraded: 'warning',
  incident: 'danger',
  no_sample: 'neutral',
}

/**
 * 徽标色调。参数取 `string` 而不是联合类型：后端将来新增状态时，未知值退回
 * `neutral` 而不是让整页在运行时炸掉（契约演进不应把页面打白屏）。
 */
export function perfStatusTone(status: string): PerfTone {
  return PERF_STATUS_TONES[status as PerfStatus] ?? 'neutral'
}

/** 状态文案的 i18n 绝对键（与页面其它 `admin.*` 键一致）。 */
export function perfStatusKey(status: string): string {
  return `admin.performance.models.status_${status}`
}

/** 体检格状态：ok / warn / crit / unknown（unknown 不等于异常）。 */
export type VitalStatus = PerfOverview['vitals'][number]['status']

const VITAL_TONES: Record<VitalStatus, PerfTone> = {
  ok: 'success',
  warn: 'warning',
  crit: 'danger',
  unknown: 'neutral',
}

/** 与 `perfStatusTone` 同一策略：未知值退回 `neutral`，不抛错。 */
export function vitalTone(status: string): PerfTone {
  return VITAL_TONES[status as VitalStatus] ?? 'neutral'
}

/* ------------------------------------------------------------------------- *
 * 读数格式化
 * ------------------------------------------------------------------------- */

/**
 * 比例的无单位读数（如 CPU 0.213 → `0.21`），小数位由调用方决定。
 * 无样本/非有限值为 `—`，绝不当作 0。
 */
export function formatRatio(value: number | null, digits = 2): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return value.toFixed(digits)
}

/**
 * 毫秒读数：<1000 取整加 `ms`，≥1000 换成秒并保留 1 位小数（1840 → `1.8s`），
 * 与看板的模型性能表同一口径。无样本（null/NaN/负数）为 `—`，不是 0。
 *
 * 注意与既有 `formatMilliseconds` 的区别：后者是「分组数字 + `ms`」的日志
 * 结果行格式，本函数是性能表的分位读数格式，两者并存、语义不同。
 */
export function formatMs(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '—'
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}s`
  return `${Math.round(value)}ms`
}

/**
 * `*_over_30s` 为真时，同级的 `*_ms` 只是「已知 ≥30s、真实值未知」的下界
 * （+Inf 桶），不得被当成有限延迟格式化——这里先于数值判断 over30s。
 */
export function formatLatencyMs(
  value: number | null,
  over30s: boolean,
): string {
  if (over30s) return '>30s'
  if (value === null) return '—'
  return formatMs(value)
}

/** 0–1 比例 → 百分数（0.962 → `96.2%`）；无样本为 `—`。 */
export function formatPercent(value: number | null, digits = 1): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return `${(value * 100).toFixed(digits)}%`
}

/* ------------------------------------------------------------------------- *
 * Sparkline 路径
 * ------------------------------------------------------------------------- */

/**
 * 把 24 点序列折成 SVG path（视口坐标，配合 `viewBox="0 0 100 24"` 与
 * `vector-effect="non-scaling-stroke"` 使用）。
 *
 * `null` 表示「该小时无样本」或「p95 越过 30s」，必须在 null 处断线：
 * 每个连续的非 null 段各起一条 `M…` 子路径，不插值、不补 0、不跨空洞连线。
 * 全部为 null（或空数组）返回空串；只有一个非 null 点时输出单个 `M x,y`。
 */
export function sparkPath(
  values: readonly (number | null)[],
  width: number,
  height: number,
): string {
  if (
    values.length === 0 ||
    !Number.isFinite(width) ||
    !Number.isFinite(height)
  ) {
    return ''
  }

  const finite = values.map((value) =>
    typeof value === 'number' && Number.isFinite(value) ? value : null,
  )
  const present = finite.filter((value): value is number => value !== null)
  if (present.length === 0) return ''

  const min = Math.min(...present)
  const max = Math.max(...present)
  // 全等序列（含单点）的 span 取 1，让 y 恒定落在基线上，避免 0 除。
  const span = max - min || 1
  const stepX = values.length > 1 ? width / (values.length - 1) : width / 2

  const commands: string[] = []
  let drawing = false
  finite.forEach((value, index) => {
    if (value === null) {
      drawing = false
      return
    }
    const x = values.length > 1 ? index * stepX : width / 2
    const y = height - 2 - ((value - min) / span) * (height - 4)
    commands.push(`${drawing ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`)
    drawing = true
  })
  return commands.join(' ')
}

