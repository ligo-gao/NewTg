import { fireEvent, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PublicModelDetail, PublicModelMetrics } from './api'
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
  capabilities: ['vision'],
  context_length: 128000,
  max_output: 32000,
  mode: 'per_token',
  prompt_price: '500',
  completion_price: '1500',
  cache_read_price: '50',
  cache_write_price: '150',
  is_free: false,
  group_price: null,
  pricing_schedule: null,
  endpoints: ['chat.completions'],
  tags: ['long-context'],
  description: '',
  modalities: ['text', 'image', 'hologram'],
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

const METRICS: PublicModelMetrics = {
  range: '7d',
  window: { start: '2026-10-16T00:00:00Z', end: '2026-10-23T00:00:00Z' },
  summary: {
    avg_latency_ms: 820,
    avg_ttft_ms: 210,
    success_rate: 0.99,
    tps: 42.5,
    failed_24h: 1,
    sample_size: 1543,
  },
  per_group: [],
  latest_test: {
    ttft_ms_p50: 350,
    ttft_ms_p95: 700,
    latency_ms_p50: 1200,
    latency_ms_p95: 2200,
    tps: 20,
    success_rate: 0.98,
    sample_size: 200,
    tested_at: '2026-10-23T00:00:00Z',
  },
}

function handlers(detail: PublicModelDetail = DETAIL, metrics: PublicModelMetrics = METRICS) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    http.get(`${ORIGIN}/api/pricing/:modelId/metrics`, () => HttpResponse.json(metrics)),
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
    // 模态与端点枚举走本地化可读名；未知模态原样显示（不回退成 i18n 键名）。
    expect(getByText('Text · Image · hologram')).toBeTruthy()
    expect(getByText('Vision')).toBeTruthy()
    expect(getByText('Chat completions')).toBeTruthy()
    // 参数表列标签完整（name/required/type/default/description）。
    expect(getByText('Default')).toBeTruthy()
    expect(getByText('Description')).toBeTruthy()
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

  it('renders the real-call performance summary', async () => {
    handlers()
    const { findByRole, findByText } = await renderWithProviders(
      <PricingDetailPage modelId="openai/gpt-5" />,
      ['public'],
    )

    const heading = await findByRole('heading', { name: 'Performance' })
    const section = heading.closest('section')
    if (!(section instanceof HTMLElement)) {
      throw new Error('missing performance section')
    }
    expect(await within(section).findByText('210 ms')).toBeTruthy()
    expect(within(section).getByText('820 ms')).toBeTruthy()
    expect(within(section).getByText('42.5')).toBeTruthy()
    expect(within(section).getByText('99%')).toBeTruthy()
    expect(await findByText('Avg TTFT')).toBeTruthy()
  })

  it('falls back to the latest probe when the usage summary is empty', async () => {
    handlers(DETAIL, {
      ...METRICS,
      summary: {
        avg_latency_ms: null,
        avg_ttft_ms: null,
        success_rate: null,
        tps: null,
        failed_24h: null,
        sample_size: 0,
      },
      latest_test: {
        ...METRICS.latest_test!,
        ttft_ms_p50: 350,
        latency_ms_p50: 1200,
        tps: 20,
        success_rate: 0.98,
      },
    })
    const { findByRole } = await renderWithProviders(
      <PricingDetailPage modelId="openai/gpt-5" />,
      ['public'],
    )

    const heading = await findByRole('heading', { name: 'Performance' })
    const section = heading.closest('section')
    if (!(section instanceof HTMLElement)) {
      throw new Error('missing performance section')
    }
    expect(await within(section).findByText('350 ms')).toBeTruthy()
    expect(within(section).getByText('1,200 ms')).toBeTruthy()
    expect(within(section).getByText('20')).toBeTruthy()
    expect(within(section).getByText('98%')).toBeTruthy()
  })

  it('keeps dashes and offers an inline retry when the metrics request fails', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(STATUS)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
      http.get(`${ORIGIN}/api/pricing/:modelId/metrics`, () =>
        HttpResponse.json({ title: 'boom' }, { status: 500 }),
      ),
      http.get(`${ORIGIN}/api/pricing/:modelId`, () => HttpResponse.json(DETAIL)),
    )
    const { findByRole, getByRole } = await renderWithProviders(
      <PricingDetailPage modelId="openai/gpt-5" />,
      ['public'],
    )

    const heading = await findByRole('heading', { name: 'Performance' })
    const section = heading.closest('section')
    if (!(section instanceof HTMLElement)) {
      throw new Error('missing performance section')
    }
    expect(await within(section).findByRole('alert')).toBeTruthy()
    expect(within(section).getAllByText('—')).toHaveLength(4)
    expect(within(section).getByRole('button', { name: 'Retry' })).toBeTruthy()
    expect(getByRole('heading', { name: 'GPT-5' })).toBeTruthy()
  })
})
