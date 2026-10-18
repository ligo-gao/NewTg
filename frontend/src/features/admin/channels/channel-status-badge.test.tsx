import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { ChannelStatusBadge } from './channel-status-badge'
import { renderWithProviders } from './test-utils'

/**
 * modules-06 §1.3 / modules-00 §4.6.4: three states, dual-encoded dot + text.
 * Manual disable is neutral — danger is reserved for errors and irreversible
 * confirmations.
 */
describe('ChannelStatusBadge', () => {
  it('renders status 1 as a success badge', () => {
    renderWithProviders(<ChannelStatusBadge status={1} />)

    expect(screen.getByText('Enabled')).toBeTruthy()
    expect(screen.getByTestId('status-badge').getAttribute('data-variant')).toBe(
      'success',
    )
    expect(screen.getByTestId('status-badge-dot')).toBeTruthy()
  })

  it('renders status 2 as a neutral badge', () => {
    renderWithProviders(<ChannelStatusBadge status={2} />)

    expect(screen.getByText('Manually Disabled')).toBeTruthy()
    expect(screen.getByTestId('status-badge').getAttribute('data-variant')).toBe(
      'neutral',
    )
  })

  it('renders status 3 as a warning badge', () => {
    renderWithProviders(<ChannelStatusBadge status={3} />)

    expect(screen.getByText('Auto Disabled')).toBeTruthy()
    expect(screen.getByTestId('status-badge').getAttribute('data-variant')).toBe(
      'warning',
    )
  })

  it('renders unknown values as an em dash, not a fourth state', () => {
    renderWithProviders(<ChannelStatusBadge status={9} />)

    expect(screen.getByTestId('channel-status-unknown').textContent).toBe('—')
    expect(screen.queryByTestId('status-badge')).toBeNull()
  })
})
