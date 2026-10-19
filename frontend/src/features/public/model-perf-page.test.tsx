import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { fireEvent, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ModelPerfPage } from './model-perf-page'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

interface MetricsFixture {
  summary: {
    avg_latency_ms: number | null
    success_rate: number | null
    tps: number | null
    failed_24h: number | null
    sample_size: number
  }
  per_group: {
    group: string
    avg_ttft_ms: number | null
    avg_latency_ms: number | null
    success_rate: number | null
  }[]
}

const POPULATED: MetricsFixture = {
  summary: {
    avg_latency_ms: 820,
    success_rate: 0.997,
    tps: 42.5,
    failed_24h: 12,
    sample_size: 1543,
  },
  per_group: [
    { group: 'default', avg_ttft_ms: 210, avg_latency_ms: 800, success_rate: 0.998 },
    { group: 'vip', avg_ttft_ms: null, avg_latency_ms: 780, success_rate: 0.999 },
  ],
}

const EMPTY: MetricsFixture = {
  summary: {
    avg_latency_ms: null,
    success_rate: null,
    tps: null,
    failed_24h: null,
    sample_size: 0,
  },
  per_group: [],
}

function metricsHandler(fixture: MetricsFixture, record?: (url: URL) => void) {
  return http.get(`${ORIGIN}/api/pricing/:modelId/metrics`, ({ request }) => {
    record?.(new URL(request.url))
    return HttpResponse.json(fixture)
  })
}

describe('ModelPerfPage (F-09-25)', () => {
  it('renders the aggregate statistics and the per-group table from the metrics contract', async () => {
    server.use(metricsHandler(POPULATED))

    const { findByText, getByRole, getByText } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )

    expect(getByRole('heading', { level: 1 }).textContent).toContain('Performance')
    expect(await findByText('820 ms')).toBeTruthy()
    expect(getByText('99.7%')).toBeTruthy()
    expect(getByText('42.5')).toBeTruthy()
    expect(getByText('12')).toBeTruthy()

    const table = getByRole('table', { name: 'Performance by group' })
    expect(table.textContent).toContain('default')
    expect(table.textContent).toContain('vip')
    expect(table.textContent).toContain('210 ms')
    // 非流式分组的 TTFT 缺失显示 — 而不是 0。
    expect(table.textContent).toContain('—')
    expect(getByText('Approximate')).toBeTruthy()
  })

  it('requests the documented metrics endpoint with the selected range', async () => {
    const urls: string[] = []
    server.use(metricsHandler(POPULATED, (url) => urls.push(url.pathname + url.search)))

    const { findByText, getByRole } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )
    await findByText('820 ms')
    expect(urls[0]).toBe('/api/pricing/gpt-4o/metrics?range=24h')

    fireEvent.click(getByRole('radio', { name: 'Last 7d' }))
    await waitFor(() => {
      expect(urls.at(-1)).toBe('/api/pricing/gpt-4o/metrics?range=7d')
    })
  })

  it('renders em dashes and the empty copy when there is no usage data', async () => {
    server.use(metricsHandler(EMPTY))

    const { findAllByText, findByText } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )

    expect(await findByText('No usage data yet.')).toBeTruthy()
    const dashes = await findAllByText('—')
    expect(dashes.length).toBeGreaterThanOrEqual(4)
  })

  it('shows the low-sample warning below 20 requests', async () => {
    server.use(
      metricsHandler({ ...POPULATED, summary: { ...POPULATED.summary, sample_size: 5 } }),
    )

    const { findByText } = await renderWithProviders(<ModelPerfPage modelId="gpt-4o" />, [
      'public',
    ])

    expect(await findByText('Low sample size')).toBeTruthy()
  })

  it('hides the TTFT column when no group reports a TTFT (non-streaming model)', async () => {
    server.use(
      metricsHandler({
        ...POPULATED,
        per_group: [
          { group: 'default', avg_ttft_ms: null, avg_latency_ms: 800, success_rate: 0.998 },
        ],
      }),
    )

    const { findByText, queryByText, getByRole } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )

    await findByText('820 ms')
    expect(queryByText('Avg TTFT')).toBeNull()
    expect(getByRole('table', { name: 'Performance by group' }).textContent).not.toContain(
      'Avg TTFT',
    )
  })

  it('renders an inline error with retry when the metrics endpoint fails', async () => {
    let fail = true
    server.use(
      http.get(`${ORIGIN}/api/pricing/:modelId/metrics`, () =>
        fail
          ? HttpResponse.json(
              { type: 'about:blank', title: 'Service Unavailable' },
              { status: 503 },
            )
          : HttpResponse.json(POPULATED),
      ),
    )

    const { findByText, getByRole, queryByText } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )

    expect(await findByText('Performance data unavailable')).toBeTruthy()
    fail = false
    fireEvent.click(getByRole('button', { name: 'Retry' }))
    expect(await findByText('820 ms')).toBeTruthy()
    expect(queryByText('Performance data unavailable')).toBeNull()
  })

  it('treats an out-of-range metrics fixture as unavailable instead of rendering it', async () => {
    server.use(
      metricsHandler({
        ...POPULATED,
        summary: { ...POPULATED.summary, success_rate: 1.5 },
      }),
    )

    const { findByText, queryByText } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )

    expect(await findByText('Performance data unavailable')).toBeTruthy()
    // 非法比率绝不能进入统计块。
    expect(queryByText('150%')).toBeNull()
  })

  it('does not render rankings or the live feed (M7 boundary, R-03)', async () => {
    server.use(metricsHandler(POPULATED))

    const { findByText, queryByText } = await renderWithProviders(
      <ModelPerfPage modelId="gpt-4o" />,
      ['public'],
    )

    await findByText('820 ms')
    expect(queryByText(/Rankings/i)).toBeNull()
    expect(queryByText(/Live call feed/i)).toBeNull()
  })
})
