import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError } from '@/api/errors'

import { fetchChannelModels } from './api'
import { FetchModelsDialog } from './fetch-models-dialog'
import { renderWithProviders } from './test-utils'

vi.mock('./api', () => ({
  fetchChannelModels: vi.fn(),
}))

const mockFetchChannelModels = vi.mocked(fetchChannelModels)

const REQUEST = {
  type: 'openai',
  base_url: 'https://api.openai.com',
  credentials: { api_key: 'sk-test-1234' },
}

/** The model name lives in the label text, so the checkbox has no name of its own. */
function checkboxFor(model: string): HTMLInputElement {
  const row = screen.getByText(model).closest('label')
  if (row === null) {
    throw new Error(`No model row for "${model}"`)
  }
  return within(row).getByRole('checkbox') as HTMLInputElement
}

function checkedStates(): boolean[] {
  return screen
    .getAllByRole('checkbox')
    .map((checkbox) => (checkbox as HTMLInputElement).checked)
}

function applyButton(): HTMLButtonElement {
  return screen.getByRole('button', { name: /^Apply/ }) as HTMLButtonElement
}

beforeEach(() => {
  mockFetchChannelModels.mockReset()
})

describe('FetchModelsDialog', () => {
  it('preselects every fetched model in create mode and applies all of them', async () => {
    const user = userEvent.setup()
    const onApply = vi.fn()
    const onOpenChange = vi.fn()
    mockFetchChannelModels.mockResolvedValue({
      models: ['gpt-4o', 'gpt-4o-mini'],
    })

    renderWithProviders(
      <FetchModelsDialog
        open
        onOpenChange={onOpenChange}
        request={REQUEST}
        existingModels={[]}
        onApply={onApply}
      />,
    )

    await screen.findByText('gpt-4o')
    expect(mockFetchChannelModels).toHaveBeenCalledTimes(1)
    expect(screen.getByText('gpt-4o-mini')).toBeTruthy()
    // Create mode treats the fetched catalog as the selection.
    expect(checkedStates()).toEqual([true, true])

    await user.click(applyButton())

    expect(onApply).toHaveBeenCalledWith(['gpt-4o', 'gpt-4o-mini'])
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('preselects existing models only in edit mode and applies the union after toggling', async () => {
    const user = userEvent.setup()
    const onApply = vi.fn()
    const onOpenChange = vi.fn()
    mockFetchChannelModels.mockResolvedValue({
      models: ['gpt-4o', 'gpt-4o-mini', 'o3'],
    })

    renderWithProviders(
      <FetchModelsDialog
        open
        onOpenChange={onOpenChange}
        request={{ channel_id: 7, type: 'openai' }}
        existingModels={['gpt-4o']}
        onApply={onApply}
      />,
    )

    await screen.findByText('gpt-4o-mini')
    // The existing model survives; models the upstream advertises but the form
    // does not use yet stay unchecked.
    expect(checkedStates()).toEqual([true, false, false])

    await user.click(checkboxFor('gpt-4o-mini'))
    expect(checkedStates()).toEqual([true, true, false])

    await user.click(screen.getByRole('button', { name: 'Apply (2)' }))

    expect(onApply).toHaveBeenCalledWith(['gpt-4o', 'gpt-4o-mini'])
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('shows the API error and refetches when Retry succeeds', async () => {
    const user = userEvent.setup()
    const onApply = vi.fn()
    mockFetchChannelModels
      .mockRejectedValueOnce(
        new ApiError({
          status: 502,
          type: 'https://newtg.dev/problems/upstream_error',
          title: 'Upstream error',
          detail: 'Upstream rejected the API key.',
          kind: 'server',
        }),
      )
      .mockResolvedValueOnce({ models: ['gpt-4o'] })

    renderWithProviders(
      <FetchModelsDialog
        open
        onOpenChange={vi.fn()}
        request={REQUEST}
        existingModels={[]}
        onApply={onApply}
      />,
    )

    expect(
      await screen.findByText('Upstream rejected the API key.'),
    ).toBeTruthy()
    expect(screen.queryByText('gpt-4o')).toBeNull()
    expect(applyButton().disabled).toBe(true)

    await user.click(screen.getByRole('button', { name: 'Retry' }))

    await screen.findByText('gpt-4o')
    expect(mockFetchChannelModels).toHaveBeenCalledTimes(2)
    expect(screen.queryByText('Upstream rejected the API key.')).toBeNull()
    expect(applyButton().disabled).toBe(false)
  })

  it('filters the list, selects only visible models, and clears the selection', async () => {
    const user = userEvent.setup()
    mockFetchChannelModels.mockResolvedValue({
      models: ['gpt-4o', 'gpt-4o-mini', 'o3'],
    })

    renderWithProviders(
      <FetchModelsDialog
        open
        onOpenChange={vi.fn()}
        request={REQUEST}
        existingModels={[]}
        onApply={vi.fn()}
      />,
    )

    await screen.findByText('o3')
    const search = screen.getByPlaceholderText('Filter models')

    await user.type(search, 'gpt-4')
    expect(screen.getByText('gpt-4o')).toBeTruthy()
    expect(screen.getByText('gpt-4o-mini')).toBeTruthy()
    expect(screen.queryByText('o3')).toBeNull()

    // Clear drops the selection; nothing selected disables Apply.
    await user.click(screen.getByRole('button', { name: 'Clear' }))
    expect(checkedStates()).toEqual([false, false])
    expect(applyButton().disabled).toBe(true)

    // Select all only adds what the filter leaves visible (o3 is hidden).
    await user.click(screen.getByRole('button', { name: 'Select all' }))
    expect(checkedStates()).toEqual([true, true])
    expect(screen.getByText('2 selected')).toBeTruthy()
    expect(applyButton().disabled).toBe(false)

    // Revealing the hidden model confirms it was not selected implicitly.
    await user.clear(search)
    expect(checkboxFor('o3').checked).toBe(false)
    expect(checkedStates()).toEqual([true, true, false])

    await user.click(screen.getByRole('button', { name: 'Clear' }))
    expect(checkedStates()).toEqual([false, false, false])
    expect(applyButton().disabled).toBe(true)
  })
})
