import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { ModelCombobox } from './ModelCombobox'

type PricingCandidate = components['schemas']['PricingCandidate']

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const CANDIDATES: PricingCandidate[] = [
  {
    model: 'deepseek-flash',
    display_name: 'DeepSeek Flash',
    vendor: 'DeepSeek',
    context_length: 128000,
    sources: ['channel'],
    priced: true,
    mode: 'per_token',
  },
  {
    model: 'qwen3.7-plus',
    display_name: 'Qwen 3.7 Plus',
    vendor: 'Alibaba',
    context_length: 1_000_000,
    sources: ['catalog', 'pricing'],
    priced: false,
    mode: 'per_token',
  },
  {
    model: 'gpt-image-2',
    display_name: null,
    vendor: 'OpenAI',
    context_length: null,
    sources: ['catalog'],
    priced: false,
    mode: 'per_request',
  },
]

function installCandidates(items: PricingCandidate[] = CANDIDATES): void {
  server.use(
    http.get(`${ORIGIN}/api/model-pricing/candidates`, () =>
      HttpResponse.json({ items }),
    ),
  )
}

function Harness({
  onSelectCandidate,
}: {
  onSelectCandidate?: (candidate: PricingCandidate) => void
}) {
  const [value, setValue] = useState('')
  return (
    <ModelCombobox
      value={value}
      onChange={setValue}
      onSelectCandidate={onSelectCandidate}
    />
  )
}

describe('ModelCombobox', () => {
  it('lists candidates with their priced state and metadata', async () => {
    installCandidates()
    const { findByText, getAllByText, getByText } = await renderWithProviders(
      <Harness />,
      ['common', 'admin'],
    )

    expect(await findByText('deepseek-flash')).toBeTruthy()
    expect(getByText('qwen3.7-plus')).toBeTruthy()
    expect(getByText('gpt-image-2')).toBeTruthy()
    expect(getAllByText('Priced')).toHaveLength(1)
    expect(getAllByText('Unpriced')).toHaveLength(2)
    expect(getByText('DeepSeek · 128,000')).toBeTruthy()
    expect(getByText('OpenAI · Context unknown')).toBeTruthy()
  })

  it('filters candidates by keyword and selects one', async () => {
    installCandidates()
    const onSelectCandidate = vi.fn()
    const { findByText, getByLabelText, getByText, queryByText } =
      await renderWithProviders(<Harness onSelectCandidate={onSelectCandidate} />, [
        'common',
        'admin',
      ])
    const user = userEvent.setup()

    expect(await findByText('deepseek-flash')).toBeTruthy()
    await user.type(getByLabelText('Model'), 'qwen')

    expect(getByText('qwen3.7-plus')).toBeTruthy()
    expect(queryByText('deepseek-flash')).toBeNull()
    expect(getByText('Manual model: qwen')).toBeTruthy()

    await user.click(getByText('qwen3.7-plus'))
    expect(onSelectCandidate).toHaveBeenCalledWith(
      expect.objectContaining({ model: 'qwen3.7-plus', priced: false }),
    )
    expect((getByLabelText('Model') as HTMLInputElement).value).toBe('qwen3.7-plus')
  })

  it('reports a load failure without blocking manual input', async () => {
    server.use(
      http.get(`${ORIGIN}/api/model-pricing/candidates`, () =>
        HttpResponse.json({ type: 'about:blank', title: 'Server error' }, { status: 500 }),
      ),
    )
    const { findByText, getByLabelText } = await renderWithProviders(<Harness />, [
      'common',
      'admin',
    ])
    const user = userEvent.setup()

    expect(await findByText('Could not load models.')).toBeTruthy()
    await user.type(getByLabelText('Model'), 'custom-model')
    expect((getByLabelText('Model') as HTMLInputElement).value).toBe('custom-model')
  })
})
