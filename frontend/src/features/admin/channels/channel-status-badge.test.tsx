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

  it('localises the auto-disable reason in the tooltip (no raw code)', () => {
    renderWithProviders(
      <ChannelStatusBadge
        status={3}
        reason="channel_auth_error"
        updatedAt="2026-10-01T00:00:00Z"
      />,
    )

    const title = screen.getByTestId('channel-status-badge').getAttribute('title')
    expect(title).toContain('Authentication failed')
    expect(title).toContain('Last updated')
    expect(title).not.toContain('channel_auth_error')
  })

  it('falls back to a generic label for unknown reason codes', () => {
    renderWithProviders(
      <ChannelStatusBadge status={3} reason="channel_future_reason" updatedAt={null} />,
    )

    const title = screen.getByTestId('channel-status-badge').getAttribute('title')
    expect(title).toBe('Automatically disabled')
    expect(title).not.toContain('channel_future_reason')
  })

  it('hides the tooltip when the auto-disable reason is empty', () => {
    renderWithProviders(
      <ChannelStatusBadge status={3} reason={null} updatedAt="2026-10-01T00:00:00Z" />,
    )

    expect(screen.getByTestId('channel-status-badge').getAttribute('title')).toBeNull()
  })
})
