import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'

import { StatusBadge, type StatusBadgeVariant } from './status-badge'

/**
 * modules-00 §4.6.4: eight variants, each dual-encoded (dot + text). A status
 * badge must never rely on colour alone, and a disabled state uses `neutral`,
 * never `danger`.
 */
const VARIANTS: StatusBadgeVariant[] = [
  'success',
  'warning',
  'danger',
  'info',
  'neutral',
  'neutral-outline',
  'danger-outline',
  'brand',
]

afterEach(cleanup)

describe('StatusBadge', () => {
  it.each(VARIANTS)('renders a dot and its text for the %s variant', (variant) => {
    render(<StatusBadge variant={variant}>Active</StatusBadge>)

    const badge = screen.getByTestId('status-badge')
    expect(badge.getAttribute('data-variant')).toBe(variant)
    expect(screen.getByTestId('status-badge-dot')).toBeTruthy()
    expect(badge.textContent).toContain('Active')
  })

  it('uses a dotted outline for neutral-outline and danger-outline', () => {
    const { rerender } = render(
      <StatusBadge variant="neutral-outline">Cancelled</StatusBadge>,
    )
    expect(
      screen.getByTestId('status-badge').className.includes('border-dashed'),
    ).toBe(true)

    rerender(<StatusBadge variant="danger-outline">Refused</StatusBadge>)
    expect(
      screen.getByTestId('status-badge').className.includes('border-dashed'),
    ).toBe(true)
  })
})
