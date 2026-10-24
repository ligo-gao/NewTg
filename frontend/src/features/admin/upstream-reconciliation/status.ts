/**
 * 对账明细的展示映射（严重度、处置状态）。
 *
 * 契约里的两个枚举都是闭集，但服务端演进可能先于前端新增取值；这两个
 * 映射都按「未知值 → neutral + 原样文本」兜底，页面永远不因一个新枚举
 * 值崩溃或渲染空白。文案键集中在这里，避免表格与详情抽屉各写一份。
 */
import type { TranslateFn } from '@/shared/lib/i18n-context'
import type { StatusBadgeVariant } from '@/shared/ui/status-badge'

import type {
  ReconcileItemStatus,
  ReconcileRun,
  ReconcileSeverity,
} from './api'

/** 运行的生命周期状态（契约闭集 `ReconcileRun.status`）。 */
export type ReconcileRunStatus = ReconcileRun['status']

/** 严重度筛选条的固定顺序（也是计数请求的顺序）。 */
export const SEVERITY_ORDER = [
  'ok',
  'warn',
  'alert',
  'partial_snapshot',
  'missing_snapshot',
  'missing_local',
  'local_expired',
] as const satisfies readonly ReconcileSeverity[]

export const SEVERITY_VARIANTS: Record<ReconcileSeverity, StatusBadgeVariant> = {
  ok: 'success',
  warn: 'warning',
  alert: 'danger',
  partial_snapshot: 'info',
  missing_snapshot: 'warning',
  missing_local: 'warning',
  local_expired: 'neutral',
}

export const SEVERITY_LABEL_KEYS: Record<ReconcileSeverity, string> = {
  ok: 'admin.upstream_reconciliation.results.severity_ok',
  warn: 'admin.upstream_reconciliation.results.severity_warn',
  alert: 'admin.upstream_reconciliation.results.severity_alert',
  partial_snapshot:
    'admin.upstream_reconciliation.results.severity_partial_snapshot',
  missing_snapshot:
    'admin.upstream_reconciliation.results.severity_missing_snapshot',
  missing_local:
    'admin.upstream_reconciliation.results.severity_missing_local',
  local_expired:
    'admin.upstream_reconciliation.results.severity_local_expired',
}

export const ITEM_STATUS_VARIANTS: Record<
  ReconcileItemStatus,
  StatusBadgeVariant
> = {
  open: 'warning',
  acknowledged: 'info',
  resolved: 'success',
}

export const ITEM_STATUS_LABEL_KEYS: Record<ReconcileItemStatus, string> = {
  open: 'admin.upstream_reconciliation.results.status_open',
  acknowledged: 'admin.upstream_reconciliation.results.status_acknowledged',
  resolved: 'admin.upstream_reconciliation.results.status_resolved',
}

/**
 * 运行状态与明细状态是两套闭集：`running` 不属于明细，`open` 也不属于运行。
 * 未知运行状态同样按 neutral + 原样文本兜底。
 */
export const RUN_STATUS_VARIANTS: Record<
  ReconcileRunStatus,
  StatusBadgeVariant
> = {
  running: 'info',
  succeeded: 'success',
  partial: 'warning',
  failed: 'danger',
}

export const RUN_STATUS_LABEL_KEYS: Record<ReconcileRunStatus, string> = {
  running: 'admin.upstream_reconciliation.results.run_status_running',
  succeeded: 'admin.upstream_reconciliation.results.run_status_succeeded',
  partial: 'admin.upstream_reconciliation.results.run_status_partial',
  failed: 'admin.upstream_reconciliation.results.run_status_failed',
}

function isSeverity(value: string): value is ReconcileSeverity {
  return (SEVERITY_ORDER as readonly string[]).includes(value)
}

function isItemStatus(value: string): value is ReconcileItemStatus {
  return Object.hasOwn(ITEM_STATUS_VARIANTS, value)
}

function isRunStatus(value: string): value is ReconcileRunStatus {
  return Object.hasOwn(RUN_STATUS_VARIANTS, value)
}

export function severityVariant(value: string): StatusBadgeVariant {
  return isSeverity(value) ? SEVERITY_VARIANTS[value] : 'neutral'
}

/** 未知严重度原样显示（而不是把 i18n key 暴露给管理员）。 */
export function severityLabel(value: string, t: TranslateFn): string {
  return isSeverity(value) ? t(SEVERITY_LABEL_KEYS[value]) : value
}

export function itemStatusVariant(value: string): StatusBadgeVariant {
  return isItemStatus(value) ? ITEM_STATUS_VARIANTS[value] : 'neutral'
}

export function itemStatusLabel(value: string, t: TranslateFn): string {
  return isItemStatus(value) ? t(ITEM_STATUS_LABEL_KEYS[value]) : value
}

export function runStatusVariant(value: string): StatusBadgeVariant {
  return isRunStatus(value) ? RUN_STATUS_VARIANTS[value] : 'neutral'
}

/** 未知运行状态原样显示（而不是把 i18n key 暴露给管理员）。 */
export function runStatusLabel(value: string, t: TranslateFn): string {
  return isRunStatus(value) ? t(RUN_STATUS_LABEL_KEYS[value]) : value
}
