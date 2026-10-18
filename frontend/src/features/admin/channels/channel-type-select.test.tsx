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
      'Anthropic native',
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

  it('shows the selected type', () => {
    renderWithProviders(
      <ChannelTypeSelect value="azure_openai" onChange={() => {}} />,
    )

    expect(
      (screen.getByRole('combobox') as HTMLSelectElement).value,
    ).toBe('azure_openai')
  })
})
