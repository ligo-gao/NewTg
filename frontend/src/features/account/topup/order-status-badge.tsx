import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import type { OrderStatus } from './types'

export type { OrderStatus } from './types'

/**
 * Five-state order badge (P-1 / modules-04 §5.4). The colour mapping is frozen:
 * pending=warning, succeeded=success, failed=danger, expired=neutral (filled),
 * cancelled=neutral-outline (dashed, no fill). `StatusBadge` always renders the
 * dot plus the text, so the state is never encoded by colour alone.
 */
export const ORDER_STATUS_VARIANTS: Record<OrderStatus, StatusBadgeVariant> = {
  pending: 'warning',
  succeeded: 'success',
  failed: 'danger',
  expired: 'neutral',
  cancelled: 'neutral-outline',
}

export const ORDER_STATUS_KEYS: Record<OrderStatus, string> = {
  pending: 'account.orders.status_pending',
  succeeded: 'account.orders.status_succeeded',
  failed: 'account.orders.status_failed',
  expired: 'account.orders.status_expired',
  cancelled: 'account.orders.status_cancelled',
}

/** All statuses in lifecycle order, reused by the list filter. */
export const ORDER_STATUSES: readonly OrderStatus[] = [
  'pending',
  'succeeded',
  'failed',
  'expired',
  'cancelled',
]

export interface OrderStatusBadgeProps {
  status: OrderStatus
}

export function OrderStatusBadge({ status }: OrderStatusBadgeProps) {
  const { t } = useI18n()
  const variant = ORDER_STATUS_VARIANTS[status]
  const key = ORDER_STATUS_KEYS[status]

  if (variant === undefined || key === undefined) {
    // Unknown values are rendered as an em dash rather than inventing a sixth
    // state (same fail-closed rule as the channel badge).
    return <span className="text-ink-3">—</span>
  }

  return <StatusBadge variant={variant}>{t(key)}</StatusBadge>
}
