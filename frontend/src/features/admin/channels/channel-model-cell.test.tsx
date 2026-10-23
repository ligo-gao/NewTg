import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ChannelModelCell } from './channel-model-cell'
import { renderWithProviders } from './test-utils'

/**
 * The models column only folds a count; the full list lives in the expanded
 * dossier. These tests pin the fold affordance and its accessibility wiring.
 */

describe('ChannelModelCell', () => {
  it('renders the folded model count and never the model names', () => {
    renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4', 'gpt-4o']}
        expanded={false}
        onToggle={() => undefined}
      />,
    )

    expect(screen.getByText('Models: 2')).toBeTruthy()
    expect(screen.queryByText('gpt-4')).toBeNull()
    expect(screen.queryByText('gpt-4o')).toBeNull()
  })

  it('renders count-neutral copy for a single model', () => {
    renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4']}
        expanded={false}
        onToggle={() => undefined}
      />,
    )

    expect(screen.getByText('Models: 1')).toBeTruthy()
    expect(screen.queryByText('1 models')).toBeNull()
  })

  it('renders an em dash when the channel has no models', () => {
    renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={[]}
        expanded={false}
        onToggle={() => undefined}
      />,
    )

    expect(screen.getByText('—')).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('names the fold button with the count and the channel name', () => {
    renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4', 'gpt-4o']}
        expanded={false}
        onToggle={() => undefined}
      />,
    )

    expect(
      screen.getByRole('button', { name: 'Models: 2 · alpha' }),
    ).toBeTruthy()
  })

  it('points aria-controls at the detail row only while expanded', () => {
    const { rerender } = renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4']}
        expanded={false}
        onToggle={() => undefined}
      />,
    )

    const button = screen.getByRole('button', { name: 'Models: 1 · alpha' })
    expect(button.getAttribute('aria-expanded')).toBe('false')
    // Collapsed, the detail row does not exist, so no dangling reference.
    expect(button.getAttribute('aria-controls')).toBeNull()

    rerender(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4']}
        expanded
        onToggle={() => undefined}
      />,
    )

    const expandedButton = screen.getByRole('button', {
      name: 'Models: 1 · alpha',
    })
    expect(expandedButton.getAttribute('aria-expanded')).toBe('true')
    expect(expandedButton.getAttribute('aria-controls')).toBe('channel-detail-7')
  })

  it('calls onToggle when clicked', async () => {
    const user = userEvent.setup()
    const onToggle = vi.fn()
    renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4']}
        expanded={false}
        onToggle={onToggle}
      />,
    )

    await user.click(screen.getByRole('button', { name: 'Models: 1 · alpha' }))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('rotates the chevron when expanded', () => {
    const { rerender } = renderWithProviders(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4']}
        expanded={false}
        onToggle={() => undefined}
      />,
    )

    const collapsed = screen.getByTestId('channel-model-chevron-7')
    expect(collapsed.classList.contains('rotate-90')).toBe(false)

    rerender(
      <ChannelModelCell
        channelId={7}
        channelName="alpha"
        models={['gpt-4']}
        expanded
        onToggle={() => undefined}
      />,
    )

    expect(
      screen.getByTestId('channel-model-chevron-7').classList.contains('rotate-90'),
    ).toBe(true)
  })
})
