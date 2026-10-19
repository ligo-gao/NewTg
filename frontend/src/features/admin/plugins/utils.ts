import { ApiError } from '@/api/errors'
import type { StatusBadgeVariant } from '@/shared/ui/status-badge'

import type {
  MarketplacePlugin,
  PluginApiError,
  PluginIssue,
  TaskPluginRuntimeStatus,
} from './api'

/** 五态徽章映射（§6.10 裁决 08-6：运行状态是状态，不是分类）。 */
export const RUNTIME_STATUS_VARIANTS: Record<
  TaskPluginRuntimeStatus,
  StatusBadgeVariant
> = {
  registered: 'success',
  disabled_fallback: 'warning',
  disabled: 'neutral',
  not_registered: 'danger',
  compile_failed: 'danger',
}

export const RUNTIME_STATUSES: readonly TaskPluginRuntimeStatus[] = [
  'registered',
  'disabled_fallback',
  'disabled',
  'not_registered',
  'compile_failed',
]

export function runtimeStatusKey(status: TaskPluginRuntimeStatus): string {
  return `admin.task_plugins.status.${status}`
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/** 编译/校验问题按「文件:行 + 消息」一行一条（错误文本必须可复制）。 */
export function issueMessages(error: unknown): string[] {
  const issues = (error as PluginApiError | undefined)?.issues
  if (issues === undefined) {
    const message = errorMessage(error)
    return message === '' ? [] : [message]
  }
  return issues.map((issue: PluginIssue) => {
    const location =
      issue.fieldPath === ''
        ? issue.key
        : `${issue.fieldPath}${issue.key === '' ? '' : ` (${issue.key})`}`
    return location === '' ? issue.message : `${location}: ${issue.message}`
  })
}

/** Problem `type` 的 slug（`https://newtg.dev/problems/<slug>`）。 */
export function problemSlug(error: unknown): string {
  if (!(error instanceof ApiError)) return ''
  const type = error.type
  const index = type.lastIndexOf('/')
  return index >= 0 ? type.slice(index + 1) : type
}

/**
 * 插件图标占位：缺图标时用 key 首字母（视觉 §6.10：48×48 圆角 8）。
 * 只取可打印字符，避免把未知 key 直接塞进布局。
 */
export function pluginInitial(key: string): string {
  const first = key.replace(/[^a-z0-9]/gi, '').charAt(0)
  return first === '' ? '?' : first.toUpperCase()
}

export function prettyJson(value: unknown): string {
  if (typeof value === 'string') return value
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}

/** `source_hash` 只在窄列展示前 12 位；完整值留给完整性页签。 */
export function shortHash(hash: string): string {
  return hash.length > 12 ? `${hash.slice(0, 12)}…` : hash
}

export type InstallState = 'installed' | 'upgrade' | 'available'

export function installState(plugin: MarketplacePlugin): InstallState {
  if (plugin.upgrade_available) return 'upgrade'
  return plugin.installed ? 'installed' : 'available'
}

/**
 * 沙箱「宿主解析结果」的最小投影：dryrun 直接返回钩子返回值，其中可能带
 * `status` 字段。契约任务状态词汇表为 7 态（relay-06 §3.7），不在表内时
 * 界面必须高亮而不是静默归类（F-08-35）。
 */
export const TASK_STATES = [
  'SUCCESS',
  'FAILURE',
  'QUEUED',
  'IN_PROGRESS',
  'NOT_START',
  'SUBMITTED',
  'UNKNOWN',
] as const

export function statusValueOf(result: unknown): string | undefined {
  if (typeof result === 'string') return result
  if (typeof result !== 'object' || result === null) return undefined
  const value = (result as Record<string, unknown>).status
  return typeof value === 'string' ? value : undefined
}

export interface DiffLine {
  left: string | null
  right: string | null
  /** `same` 未变更；`changed` 两侧不同；`added`/`removed` 仅一侧存在。 */
  kind: 'same' | 'changed' | 'added' | 'removed'
}

/**
 * 行级并排比对（不做 LCS）：按索引对齐两侧行，长度差补空行。
 *
 * 设计允许「默认折叠未变更段」，这里保持全部行可见——插件源通常几百行，
 * 折叠逻辑会引入可访问性负担；行级三态色标与 §6.8 同口径（新增绿/删除红）。
 */
export function diffLines(left: string, right: string): DiffLine[] {
  const a = left.split('\n')
  const b = right.split('\n')
  const rows: DiffLine[] = []
  for (let index = 0; index < Math.max(a.length, b.length); index += 1) {
    const l = index < a.length ? (a[index] ?? '') : null
    const r = index < b.length ? (b[index] ?? '') : null
    const kind =
      l === null ? 'added' : r === null ? 'removed' : l === r ? 'same' : 'changed'
    rows.push({ left: l, right: r, kind })
  }
  return rows
}
