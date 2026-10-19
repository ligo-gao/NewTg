import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { OrderStatusBadge, type OrderStatus } from './order-status-badge'

const CASES: ReadonlyArray<{ status: OrderStatus; variant: string; label: string }> = [
  { status: 'pending', variant: 'warning', label: 'Pending' },
  { status: 'succeeded', variant: 'success', label: 'Succeeded' },
  { status: 'failed', variant: 'danger', label: 'Failed' },
  { status: 'expired', variant: 'neutral', label: 'Expired' },
  { status: 'cancelled', variant: 'neutral-outline', label: 'Cancelled' },
]

describe('OrderStatusBadge (P-1 five states)', () => {
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
