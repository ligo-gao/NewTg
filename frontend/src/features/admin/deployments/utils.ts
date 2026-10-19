import { ApiError } from '@/api/errors'

/** 行内/抽屉共用的错误文案（RFC 7807 的 detail 优先于 title）。 */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

export function isForbidden(error: unknown): boolean {
  return error instanceof ApiError && error.kind === 'forbidden'
}

/**
 * 504 = provider 超时：写操作结果未知（§3.20.9）。页面展示「结果未知，
 * 请先刷新」且**不自动重试**；用户刷新确认 provider 状态后再用新 key。
 */
export function isUnknownResult(error: unknown): boolean {
  return error instanceof ApiError && error.status === 504
}

export function isRateLimited(error: unknown): boolean {
  return error instanceof ApiError && error.status === 429
}

export function retryAfterOf(error: unknown): number | undefined {
  return error instanceof ApiError ? error.retry_after : undefined
}

export function isOffline(error: unknown): boolean {
  return error instanceof ApiError && error.kind === 'offline'
}

/** 剩余计算分钟 → `12h 30m`；未知返回 `—`。 */
export function formatRemainingMinutes(
  minutes: number | null | undefined,
): string {
  if (minutes === null || minutes === undefined || !Number.isFinite(minutes)) {
    return '—'
  }
  const safe = Math.max(0, Math.floor(minutes))
  const hours = Math.floor(safe / 60)
  const rest = safe % 60
  if (hours === 0) return `${rest}m`
  if (rest === 0) return `${hours}h`
  return `${hours}h ${rest}m`
}

/** 容器运行秒数 → `3h 05m`。 */
export function formatRuntimeSeconds(seconds: number | undefined): string {
  if (seconds === undefined || !Number.isFinite(seconds) || seconds < 0) {
    return '—'
  }
  const total = Math.floor(seconds)
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  return hours > 0 ? `${hours}h ${String(minutes).padStart(2, '0')}m` : `${minutes}m`
}

export function formatDateTime(
  value: string | null | undefined,
  locale: string,
): string {
  if (value === null || value === undefined || value === '') return '—'
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return value
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(timestamp))
}

/**
 * provider 金额与币种**原样**展示（§3.20.9：不换算成积分、不擅自格式化
 * 币种符号）。浮点仅做常规两位显示，币种串始终来自 provider。
 */
export function formatAmount(amount: number, currency: string): string {
  const value = Number.isFinite(amount) ? amount.toFixed(2) : String(amount)
  return currency === '' ? value : `${value} ${currency}`
}

export function positiveInteger(value: string): number | null {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 1) return null
  return parsed
}

/** 普通环境变量行 ↔ 对象；空键丢弃，值原样保留。 */
export interface EnvRow {
  key: string
  value: string
}

export function envToRows(record: Record<string, unknown> | undefined): EnvRow[] {
  if (record === undefined) return []
  return Object.entries(record).map(([key, value]) => ({
    key,
    value: typeof value === 'string' ? value : String(value),
  }))
}

export function rowsToEnv(rows: EnvRow[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const row of rows) {
    const key = row.key.trim()
    if (key !== '') out[key] = row.value
  }
  return out
}

/** 逗号/空白分隔的入口命令与参数输入 → 字符串数组。 */
export function splitCommand(value: string): string[] {
  return value
    .split(/\s+/)
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '')
}

export function anyProviderField(
  record: Record<string, unknown>,
  keys: string[],
): string | undefined {
  for (const key of keys) {
    const value = record[key]
    if (typeof value === 'string' && value !== '') return value
    if (typeof value === 'number' && Number.isFinite(value)) return String(value)
  }
  return undefined
}
