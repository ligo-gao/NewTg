import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'

import { fetchChannelModels } from './api'
import { ModelProfileWizard } from './model-profile-wizard'
import { renderWithProviders } from './test-utils'
import type { FetchChannelModelEntry } from './types'

vi.mock('./api', () => ({
  fetchChannelModels: vi.fn(),
}))
vi.mock('@/api/client', () => ({
  apiClient: { GET: vi.fn(), POST: vi.fn(), PUT: vi.fn() },
}))

const mockFetch = vi.mocked(fetchChannelModels)
const mockGet = vi.mocked(apiClient.GET)

const REQUEST = {
  type: 'gemini',
  base_url: 'https://generativelanguage.googleapis.com',
  credentials: { api_key: 'AIza-test' },
}

function entry(
  overrides: Partial<FetchChannelModelEntry> = {},
): FetchChannelModelEntry {
  return {
    id: 'gpt-4o',
    display_name: '',
    description: '',
    context_length: null,
    max_output: null,
    released_at: '',
    endpoints: [],
    vendor_suggestion: '',
    facts_source: '',
    ...overrides,
  }
}

function renderWizard() {
  return renderWithProviders(
    <ModelProfileWizard
      request={REQUEST}
      channelModels={[]}
      onBack={vi.fn()}
      onApplyModels={vi.fn()}
    />,
  )
}

beforeEach(() => {
  mockFetch.mockReset()
  mockGet.mockReset()
  mockGet.mockResolvedValue({ data: { items: [], total: 0 } } as never)
})

describe('ModelProfileWizard', () => {
  it('probes the upstream and shows one row per advertised model', async () => {
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro', 'text-embedding-004'],
      entries: [
        entry({
          id: 'gemini-2.5-pro',
          display_name: 'Gemini 2.5 Pro',
          context_length: 1048576,
        }),
        entry({ id: 'text-embedding-004', endpoints: [] }),
      ],
    })

    renderWizard()

    expect(await screen.findByText('gemini-2.5-pro')).toBeTruthy()
    expect(screen.getByText('Gemini 2.5 Pro')).toBeTruthy()
    // Values the upstream supplied are marked "Auto"; the row that only has a
    // model id is marked "Missing".
    expect(screen.getAllByText('Auto').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Missing').length).toBeGreaterThan(0)
    expect(screen.getByText('text-embedding-004')).toBeTruthy()
  })

  it('shows an empty state when the upstream advertises nothing', async () => {
    mockFetch.mockResolvedValue({ models: [], entries: [] })

    renderWizard()

    expect(
      await screen.findByText('The upstream returned no models.'),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Back to channel form' }),
    ).toBeTruthy()
  })

  it('surfaces a probe failure with a retry action', async () => {
    mockFetch.mockRejectedValue(
      new ApiError({
        status: 502,
        type: 'https://newtg.dev/problems/upstream_error',
        title: 'Upstream error',
        detail: 'The upstream returned 502',
        kind: 'server',
      }),
    )

    renderWizard()

    expect(
      await screen.findByText('The upstream returned 502'),
    ).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Back to channel form' }),
    ).toBeTruthy()
  })

  it('returns the selected models to the channel form when the toggle is on', async () => {
    const user = userEvent.setup()
    const onBack = vi.fn()
    const onApplyModels = vi.fn()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro', 'text-embedding-004'],
      entries: [
        entry({ id: 'gemini-2.5-pro' }),
        entry({ id: 'text-embedding-004' }),
      ],
    })

    renderWithProviders(
      <ModelProfileWizard
        request={REQUEST}
        channelModels={[]}
        onBack={onBack}
        onApplyModels={onApplyModels}
      />,
    )

    await screen.findByText('gemini-2.5-pro')
    await user.click(screen.getByRole('button', { name: 'Back to channel form' }))

    expect(onApplyModels).toHaveBeenCalledWith([
      'gemini-2.5-pro',
      'text-embedding-004',
    ])
    expect(onBack).toHaveBeenCalledTimes(1)
  })

  it('locks the next step when the model catalog fails to load', async () => {
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    mockGet.mockImplementation(((path: string) =>
      path === '/api/models'
        ? Promise.reject(new Error('catalog down'))
        : Promise.resolve({ data: { items: [], total: 0 } })) as never)

    renderWizard()

    expect(
      await screen.findByText(
        'Could not load the model catalog. Reload it before continuing.',
      ),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Reload catalog' }),
    ).toBeTruthy()
    expect(
      (screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
  })

  it('unlocks the next step once a failed catalog retry succeeds', async () => {
    const user = userEvent.setup()
    let catalogCalls = 0
    mockGet.mockImplementation(((path: string) => {
      if (path === '/api/models') {
        catalogCalls += 1
        return catalogCalls === 1
          ? Promise.reject(new Error('catalog down'))
          : Promise.resolve({ data: { items: [], total: 0 } })
      }
      return Promise.resolve({ data: { items: [], total: 0 } })
    }) as never)
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })

    renderWizard()

    await user.click(
      await screen.findByRole('button', { name: 'Reload catalog' }),
    )

    await waitFor(() => {
      expect(
        (screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement)
          .disabled,
      ).toBe(false)
    })
  })

  it('keeps models the upstream did not advertise when writing back', async () => {
    const user = userEvent.setup()
    const onApplyModels = vi.fn()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro', 'text-embedding-004'],
      entries: [
        entry({ id: 'gemini-2.5-pro' }),
        entry({ id: 'text-embedding-004' }),
      ],
    })

    renderWithProviders(
      <ModelProfileWizard
        request={REQUEST}
        channelModels={['legacy-model']}
        onBack={vi.fn()}
        onApplyModels={onApplyModels}
      />,
    )

    await screen.findByText('gemini-2.5-pro')
    await user.click(
      screen.getByRole('checkbox', { name: 'text-embedding-004' }),
    )
    await user.click(
      screen.getByRole('button', { name: 'Back to channel form' }),
    )

    expect(onApplyModels).toHaveBeenCalledWith([
      'legacy-model',
      'gemini-2.5-pro',
    ])
  })

  it('leaves the channel model list untouched when the fill toggle is off', async () => {
    const user = userEvent.setup()
    const onApplyModels = vi.fn()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })

    renderWithProviders(
      <ModelProfileWizard
        request={REQUEST}
        channelModels={[]}
        onBack={vi.fn()}
        onApplyModels={onApplyModels}
      />,
    )

    await screen.findByText('gemini-2.5-pro')
    await user.click(
      screen.getByRole('checkbox', {
        name: "Also fill the channel's model list",
      }),
    )
    await user.click(
      screen.getByRole('button', { name: 'Back to channel form' }),
    )

    expect(onApplyModels).not.toHaveBeenCalled()
  })
})
