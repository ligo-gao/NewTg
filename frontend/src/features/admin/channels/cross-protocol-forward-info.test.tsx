import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { CrossProtocolForwardInfo } from './cross-protocol-forward-info'
import { renderWithProviders } from './test-utils'

/**
 * Task 6 scope: the panel explains the M4 Task 5 contract and is read-only.
 * M4 has no settings API for `relay.cross_protocol_forward_enabled`, so the
 * off/on variants are exercised through the presentational `enabled` prop
 * (module 08 will supply the live value); production renders the "unknown"
 * variant.
 */
describe('CrossProtocolForwardInfo', () => {
  it('documents the header and the switch key', () => {
    renderWithProviders(<CrossProtocolForwardInfo />)

    const panel = screen.getByRole('note')
    expect(panel.textContent).toContain('x-newtg-protocol: openai | anthropic | gemini')
    expect(panel.textContent).toContain('relay.cross_protocol_forward_enabled')
    expect(
      screen.getByText(/choose the upstream protocol/i),
    ).toBeTruthy()
  })

  it('reports the switch as unavailable when no value can be read', () => {
    renderWithProviders(<CrossProtocolForwardInfo />)

    expect(screen.getByTestId('cross-protocol-status').textContent).toBe(
      'Not readable from this console — maintained in site settings (module 08).',
    )
    expect(screen.getByText(/M4 ships no settings API/)).toBeTruthy()
  })

  it('explains the 403 behaviour while the switch is off', () => {
    renderWithProviders(<CrossProtocolForwardInfo enabled={false} />)

    expect(screen.getByTestId('cross-protocol-status').textContent).toBe(
      'Off — requests that carry the header are rejected with 403.',
    )
    expect(screen.getByText(/fails with 403 instead of being relayed/)).toBeTruthy()
  })

  it('explains forwarding while the switch is on', () => {
    renderWithProviders(<CrossProtocolForwardInfo enabled />)

    expect(screen.getByTestId('cross-protocol-status').textContent).toBe(
      'On — requests that carry the header are forwarded to the selected upstream protocol.',
    )
  })

  it('renders no editable control, because the settings API does not exist yet', () => {
    const { container } = renderWithProviders(<CrossProtocolForwardInfo />)

    expect(screen.queryByRole('switch')).toBeNull()
    expect(screen.queryByRole('checkbox')).toBeNull()
    expect(screen.queryByRole('combobox')).toBeNull()
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.queryByRole('button')).toBeNull()
    expect(container.querySelector('input, select, textarea, button')).toBeNull()
    // The deferral is stated in the UI rather than hidden from the operator.
    expect(screen.getByText(/deferred until module 08's settings API/)).toBeTruthy()
  })
})
