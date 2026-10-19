import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { ChannelTypeSelect } from './channel-type-select'
import { CHANNEL_TYPES } from './types'
import { renderWithProviders } from './test-utils'

/**
 * R-P20: 59 named types (contract.ChannelTypes) grouped by protocol family.
 * The empty sentinel is not a selectable type, so no option may have value "".
 */
describe('ChannelTypeSelect', () => {
  it('renders exactly 59 named options and no empty sentinel', () => {
    renderWithProviders(<ChannelTypeSelect value="openai" onChange={() => {}} />)

    const options = screen.getAllByRole('option') as HTMLOptionElement[]
    expect(options).toHaveLength(59)
    expect(CHANNEL_TYPES).toHaveLength(59)
    expect(options.every((option) => option.value !== '')).toBe(true)
    expect(options.map((option) => option.value)).not.toContain('')
  })

  it('groups the options under the nine protocol families', () => {
    const { container } = renderWithProviders(
      <ChannelTypeSelect value="openai" onChange={() => {}} />,
    )

    const groups = Array.from(container.querySelectorAll('optgroup'))
    expect(groups).toHaveLength(9)
    expect(groups.map((group) => group.getAttribute('label'))).toEqual([
      'OpenAI compatible',
      'Anthropic native (incl. Moonshot)',
      'Gemini native',
      'Google PaLM legacy',
      'Proprietary protocols',
      'Task platforms',
      'Flexible integration',
      'Gateway compatible',
      'Legacy / unregistered',
    ])
    const total = groups.reduce(
      (count, group) => count + group.querySelectorAll('option').length,
      0,
    )
    expect(total).toBe(59)
  })

  it('labels native-protocol types and explains the selected one', () => {
    renderWithProviders(<ChannelTypeSelect value="moonshot" onChange={() => {}} />)

    // Raw slugs (`moonshot`, `vertex_ai`) hide the protocol they speak; the
    // native family gets a human label while non-native types stay as-is.
    expect(
      screen.getByRole('option', { name: 'Moonshot (Anthropic-compatible)' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('option', { name: 'Anthropic (native Messages API)' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('option', { name: 'Vertex AI (Gemini native)' }),
    ).toBeTruthy()
    expect(screen.getByRole('option', { name: 'openai' })).toBeTruthy()

    const description = screen.getByText(
      'Moonshot exposes an Anthropic-compatible Messages endpoint, so it uses the native Anthropic adapter.',
    )
    const combobox = screen.getByRole('combobox')
    expect(combobox.getAttribute('aria-describedby')).toBe(description.id)
  })

  it('does not render a native description for non-native types', () => {
    renderWithProviders(<ChannelTypeSelect value="openai" onChange={() => {}} />)

    expect(screen.getByRole('combobox').getAttribute('aria-describedby')).toBeNull()
    expect(
      screen.queryByText(/The upstream speaks the .* protocol/),
    ).toBeNull()
  })

  it('shows the selected type', () => {
    renderWithProviders(
      <ChannelTypeSelect value="azure_openai" onChange={() => {}} />,
    )

    expect(
      (screen.getByRole('combobox') as HTMLSelectElement).value,
    ).toBe('azure_openai')
  })
})
