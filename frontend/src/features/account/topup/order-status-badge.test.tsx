import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { OrderStatusBadge, type OrderStatus } from './order-status-badge'

const CASES: ReadonlyArray<{ status: OrderStatus; variant: string; label: string }> = [
  { status: 'pending', variant: 'warning', label: 'Pending' },
  { status: 'succeeded', variant: 'success', label: 'Succeeded' },
  // S2/R-29②: money received, fulfilment failed → dashed danger badge.
  { status: 'refund_required', variant: 'danger-outline', label: 'Refund required' },
  // S3: refund completed (closed, neutral) vs provider refund failed (retryable).
  { status: 'refunded', variant: 'neutral', label: 'Refunded' },
  { status: 'refund_failed', variant: 'danger-outline', label: 'Refund failed' },
  { status: 'failed', variant: 'danger', label: 'Failed' },
  { status: 'expired', variant: 'neutral', label: 'Expired' },
  { status: 'cancelled', variant: 'neutral-outline', label: 'Cancelled' },
]

describe('OrderStatusBadge (P-1 five states + refund_required + S3 refunds)', () => {
  it.each(CASES)(
    'renders $status as $variant with text and a dot',
    async ({ status, variant, label }) => {
      await renderWithProviders(<OrderStatusBadge status={status} />, [
        'common',
        'account',
      ])

      // Dual encoding: status is never conveyed by colour alone.
      const badge = screen.getByTestId('status-badge')
      expect(badge.getAttribute('data-variant')).toBe(variant)
      expect(screen.getByTestId('status-badge-dot')).toBeTruthy()
      expect(badge.textContent).toBe(label)
    },
  )
})
