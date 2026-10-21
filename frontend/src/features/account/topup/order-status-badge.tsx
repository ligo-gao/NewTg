import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import type { OrderStatus } from './types'

export type { OrderStatus } from './types'

/**
 * Order badge (P-1 five states + S2/R-29② `refund_required` + S3 refunds).
 * The colour mapping is frozen: pending=warning, succeeded=success, failed=danger,
 * expired=neutral (filled), cancelled=neutral-outline (dashed, no fill),
 * refund_required=danger-outline (dashed, hollow).
 *
 * `refund_required` is "money received, fulfilment failed": an error the user
 * cannot fix by paying again, so it borrows danger while the dashed/hollow shape
 * keeps it visually distinct from `failed` (a filled danger badge) and from
 * `cancelled` (dashed neutral). `refunded` is a closed, successful outcome so it
 * uses neutral like expired; `refund_failed` means the provider refund did not
 * go through and is retryable, so it keeps the danger-outline shape next to
 * refund_required. `StatusBadge` always renders the dot plus the text, so the
 * state is never encoded by colour alone.
 */
export const ORDER_STATUS_VARIANTS: Record<OrderStatus, StatusBadgeVariant> = {
  pending: 'warning',
  succeeded: 'success',
  refund_required: 'danger-outline',
  refunded: 'neutral',
  refund_failed: 'danger-outline',
  failed: 'danger',
  expired: 'neutral',
  cancelled: 'neutral-outline',
}

export const ORDER_STATUS_KEYS: Record<OrderStatus, string> = {
  pending: 'account.orders.status_pending',
  succeeded: 'account.orders.status_succeeded',
  refund_required: 'account.orders.status_refund_required',
  refunded: 'account.orders.status_refunded',
  refund_failed: 'account.orders.status_refund_failed',
  failed: 'account.orders.status_failed',
  expired: 'account.orders.status_expired',
  cancelled: 'account.orders.status_cancelled',
}

/** All statuses in lifecycle order, reused by the list filter. */
export const ORDER_STATUSES: readonly OrderStatus[] = [
  'pending',
  'succeeded',
  // Derives from succeeded: paid, but the fulfilment step failed and a refund
  // is owed. Placed next to its predecessor so the filter reads as a lifecycle.
  'refund_required',
  // S3 terminal refund states: refunded is final, refund_failed is retryable.
  'refunded',
  'refund_failed',
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
