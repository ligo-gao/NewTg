import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import type { UsageLogStatus } from './search'

/**
 * Four-state usage-log badge (modules-05 §1.4). The refusal state is a hollow
 * dashed outline on purpose: it happens before any funds move, so it must not
 * read as an error of the same severity.
 */
export const USAGE_STATUS_VARIANTS: Record<UsageLogStatus, StatusBadgeVariant> = {
  success: 'success',
  error: 'danger',
  refunded: 'warning',
  refused: 'danger-outline',
}

export const USAGE_STATUS_KEYS: Record<UsageLogStatus, string> = {
  success: 'logs.usage.status.success',
  error: 'logs.usage.status.error',
  refunded: 'logs.usage.status.refunded',
  refused: 'logs.usage.status.refused',
}

export const USAGE_STATUSES: readonly UsageLogStatus[] = [
  'success',
  'error',
  'refunded',
  'refused',
]

export function UsageStatusBadge({ status }: { status: UsageLogStatus }) {
  const { t } = useI18n()
  const variant = USAGE_STATUS_VARIANTS[status]
  const key = USAGE_STATUS_KEYS[status]
  if (variant === undefined || key === undefined) {
    return <span className="text-ink-3">—</span>
  }
  return <StatusBadge variant={variant}>{t(key)}</StatusBadge>
}
