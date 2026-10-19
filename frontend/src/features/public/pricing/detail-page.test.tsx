import { fireEvent } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PublicModelDetail } from './api'
import { PricingDetailPage } from './detail-page'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const STATUS = {
  system_initialized: true,
  languages: ['en'],
  default_language: 'en',
  site: { server_address: 'https://api.example.com' },
}

const DETAIL: PublicModelDetail = {
  model_id: 'openai/gpt-5',
  display_name: 'GPT-5',
  vendor: 'openai',
  capabilities: [],
  context_length: 128000,
  max_output: 32000,
  mode: 'per_token',
  prompt_price: '500',
  completion_price: '1500',
  cache_read_price: '50',
  cache_write_price: '150',
  is_free: false,
  group_price: null,
  endpoints: ['chat.completions'],
  tags: ['long-context'],
  description: '',
  modalities: ['text', 'image'],
  knowledge_cutoff: '2025-03',
  released_at: '2025-08-07',
  group_prices: [
    {
      group: 'vip',
      ratio: 120,
      prompt: '600',
      completion: '1800',
      cache_read: '60',
      cache_write: '180',
      source: 'ratio',
    },
  ],
  parameters: [
    {
      name: 'temperature',
      required: false,
      type: 'number',
      default: 1,
      description: 'Sampling temperature',
    },
  ],
  rate_limits: [{ group: 'default', user_model_rpm: 60 }],
  auth_style: 'bearer',
}

function handlers(detail: PublicModelDetail = DETAIL) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    http.get(`${ORIGIN}/api/pricing/:modelId`, () => HttpResponse.json(detail)),
  )
}

describe('PricingDetailPage (F-09-24 / F-09-27)', () => {
  it('renders specs, base prices, group prices and parameters', async () => {
    handlers()
    const { findByRole, getByText, getAllByRole, getAllByText } = await renderWithProviders(
      <PricingDetailPage modelId="openai/gpt-5" />,
      ['public'],
    )

    expect(await findByRole('heading', { name: 'GPT-5' })).toBeTruthy()
    expect(getByText('128K')).toBeTruthy()
    expect(getByText('2025-03')).toBeTruthy()
    expect(getByText('vip')).toBeTruthy()
    // 分组表直接展示服务端算好的 600，不做倍率乘法。
    expect(getByText('600')).toBeTruthy()
    expect(getByText('1.20x')).toBeTruthy()
    expect(getAllByRole('table').length).toBeGreaterThanOrEqual(2)
    expect(getByText('temperature')).toBeTruthy()
    expect(getAllByText('60').length).toBeGreaterThanOrEqual(1)
  })

  it('switches code tabs and copies a sample with the site base URL', async () => {
    handlers()
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })

    const { findByText, getByRole, getByText } = await renderWithProviders(
      <PricingDetailPage modelId="openai/gpt-5" />,
      ['public'],
    )

    expect(await findByText(/base_url="https:\/\/api\.example\.com\/v1"/)).toBeTruthy()
    fireEvent.click(getByText('Anthropic'))
    expect(await findByText(/client\.messages\.create/)).toBeTruthy()

    fireEvent.click(getByRole('button', { name: 'Copy code sample' }))
    expect(writeText).toHaveBeenCalledTimes(1)
    const copied = writeText.mock.calls[0]?.[0] as string
    expect(copied).toContain('<YOUR_TOKEN>')
    expect(copied).toContain('client.messages.create')
  })

  it('renders the dedicated 404 branch for a missing model', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
      http.get(`${ORIGIN}/api/pricing/:modelId`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/model-not-found',
            title: 'Model not found',
          },
          { status: 404, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )

    const { findByText, getByRole } = await renderWithProviders(
      <PricingDetailPage modelId="openai/missing" />,
      ['public'],
    )

    expect(await findByText('This model does not exist or is unavailable')).toBeTruthy()
    expect(getByRole('link', { name: 'Back to marketplace' }).getAttribute('href')).toBe(
      '/pricing',
    )
  })
})
