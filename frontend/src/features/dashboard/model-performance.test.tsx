import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ModelPerformance } from './model-performance'

const ORIGIN = 'http://localhost:3000'

function summary(overrides: Record<string, unknown> = {}) {
  return {
    ttft_p50_ms: 450,
    ttft_p95_ms: 900,
    latency_p50_ms: 950,
    latency_p95_ms: 1300,
    success_rate: 0.993,
    refused: 12,
    uptime: 0.9995,
    incidents: 0,
    tps: 42.5,
    sample_size: 40,
    ttft_p95_over_30s: false,
    latency_p95_over_30s: false,
    ...overrides,
  }
}

function modelRow(overrides: Record<string, unknown> = {}) {
  return {
    model: 'gpt-4o',
    ttft_p50_ms: 400,
    ttft_p95_ms: 800,
    latency_p95_ms: 1200,
    success_rate: 0.99,
    uptime: 0.999,
    incidents: 0,
    sample_size: 40,
    badge: 'fast',
    ...overrides,
  }
}

function perf(overrides: Record<string, unknown> = {}) {
  return {
    window: {
      range: '24h',
      start: '2026-10-18T12:00:00Z',
      end: '2026-10-19T12:00:00Z',
      granularity: '1m',
    },
    buckets: [
      {
        start: '2026-10-19T10:00:00Z',
        model: 'gpt-4o',
        requests: 25,
        ttft_p50_ms: 400,
        ttft_p95_ms: 800,
        latency_p50_ms: 900,
        latency_p95_ms: 1200,
        success_rate: 0.99,
        uptime: 0.999,
        tps: 40,
      },
      {
        start: '2026-10-19T10:00:00Z',
        model: 'claude-sonnet',
        requests: 15,
        ttft_p50_ms: 600,
        ttft_p95_ms: 1200,
        latency_p50_ms: 1100,
        latency_p95_ms: 1600,
        success_rate: 0.98,
        uptime: 0.997,
        tps: 35,
      },
      {
        start: '2026-10-19T10:01:00Z',
        model: 'gpt-4o',
        requests: 20,
        ttft_p50_ms: 420,
        ttft_p95_ms: 850,
        latency_p50_ms: 930,
        latency_p95_ms: 1250,
        success_rate: 0.99,
        uptime: 0.999,
        tps: 41,
      },
    ],
    summary: summary(),
    incidents: [],
    models: [modelRow(), modelRow({ model: 'claude-sonnet', badge: 'healthy' })],
    pipeline: {
      enabled: true,
      updated_at: '2026-10-19T12:05:00Z',
      stale: false,
    },
    comparison: null,
    ...overrides,
  }
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function servePerf(
  body: () => Record<string, unknown>,
  requests: URL[] = [],
): URL[] {
  server.use(
    http.get(`${ORIGIN}/api/model-perf`, ({ request }) => {
      requests.push(new URL(request.url))
      return HttpResponse.json(body())
    }),
  )
  return requests
}

describe('ModelPerformance (F-05-08)', () => {
  it('replaces the board with an explicit gate when collection is disabled', async () => {
    servePerf(() =>
      perf({
        buckets: [],
        summary: {},
        models: [],
        pipeline: { enabled: false, updated_at: null, stale: true },
      }),
    )
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    expect(
      await screen.findByText(
        'Performance metrics collection is disabled. Contact your administrator.',
      ),
    ).toBeTruthy()
    // 不降级为 usage_logs 数字：闸门态下没有任何 KPI 卡。
    expect(screen.queryByTestId('perf-card-ttft')).toBeNull()
    expect(screen.queryByTestId('perf-chart-ttft')).toBeNull()
  })

  it('renders the six KPI cards, charts and model table from the pipeline', async () => {
    servePerf(() => perf())
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    expect(
      (await screen.findByTestId('perf-card-ttft-value')).textContent,
    ).toBe('900ms')
    expect(screen.getByTestId('perf-card-latency-value').textContent).toBe(
      '1.3s',
    )
    expect(screen.getByTestId('perf-card-success-value').textContent).toBe(
      '99.3%',
    )
    expect(screen.getByTestId('perf-card-success-subline').textContent).toContain(
      'Refused 12',
    )
    expect(screen.getByTestId('perf-card-uptime-value').textContent).toBe(
      '99.95%',
    )
    expect(screen.getByTestId('perf-card-incidents-value').textContent).toBe(
      '0',
    )
    expect(screen.getByTestId('perf-card-tps-value').textContent).toBe('42.5')

    expect(screen.getByTestId('perf-chart-ttft')).toBeTruthy()
    expect(screen.getByTestId('perf-chart-latency')).toBeTruthy()
    expect(screen.getByTestId('perf-chart-rates')).toBeTruthy()
    expect(screen.getByTestId('perf-models')).toBeTruthy()
    expect(screen.getAllByTestId('model-perf-badge')).toHaveLength(2)
    expect(screen.queryByTestId('perf-low-sample')).toBeNull()
  })

  it('shows an em dash for missing samples and a low-sample badge', async () => {
    servePerf(() =>
      perf({
        summary: summary({
          ttft_p50_ms: null,
          ttft_p95_ms: null,
          latency_p50_ms: null,
          latency_p95_ms: null,
          success_rate: null,
          uptime: null,
          incidents: null,
          tps: null,
          sample_size: 10,
        }),
      }),
    )
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    expect(
      (await screen.findByTestId('perf-card-ttft-value')).textContent,
    ).toBe('—')
    expect(screen.getByTestId('perf-card-latency-value').textContent).toBe('—')
    expect(screen.getByTestId('perf-card-success-value').textContent).toBe('—')
    expect(screen.getByTestId('perf-card-uptime-value').textContent).toBe('—')
    // 无流量时事故数不是 0（0 是「有流量且无事故」）。
    expect(screen.getByTestId('perf-card-incidents-value').textContent).toBe('—')
    expect(screen.getByTestId('perf-card-tps-value').textContent).toBe('—')
    expect(screen.getByTestId('perf-low-sample').textContent).toContain(
      'Low sample size',
    )
  })

  it('keeps the channel breakdown admin-only and requests the dimension', async () => {
    const requests = servePerf(() =>
      perf({
        dimensions: [
          {
            dimension: 'channel',
            key: '12',
            requests: 40,
            success_rate: 0.99,
            latency_p95_ms: 1200,
            ttft_p95_ms: 900,
            uptime: 0.999,
          },
        ],
      }),
    )
    const user = userEvent.setup()
    const { unmount } = await renderWithProviders(
      <ModelPerformance canViewDimension />,
      ['common'],
    )

    await screen.findByTestId('perf-card-ttft-value')
    await user.click(screen.getByTestId('perf-dimension-channel'))

    await waitFor(() => {
      expect(requests.at(-1)?.searchParams.get('dimension')).toBe('channel')
    })
    expect(await screen.findByTestId('perf-dimensions')).toBeTruthy()
    expect(screen.getByText('12')).toBeTruthy()
    unmount()

    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )
    await screen.findByTestId('perf-card-ttft-value')
    expect(
      screen.queryByRole('radiogroup', { name: 'Breakdown' }),
    ).toBeNull()
  })

  it('hides the dimension block on 403 without blanking the whole board', async () => {
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.has('dimension')) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'Forbidden' },
            { status: 403 },
          )
        }
        return HttpResponse.json(perf())
      }),
    )
    const user = userEvent.setup()
    await renderWithProviders(
      <ModelPerformance canViewDimension />,
      ['common'],
    )

    await screen.findByTestId('perf-card-ttft-value')
    await user.click(screen.getByTestId('perf-dimension-channel'))

    expect(
      await screen.findByText('Administrator permission required'),
    ).toBeTruthy()
    // 主体仍然可用：KPI 与图表没有被维度请求的 403 一起打掉。
    expect(screen.getByTestId('perf-card-ttft-value').textContent).toBe('900ms')
    expect(screen.getByTestId('perf-chart-ttft')).toBeTruthy()
    expect(screen.queryByTestId('perf-error')).toBeNull()
  })

  it('switches 24h/7d and re-queries the window', async () => {
    const requests = servePerf(() => perf())
    const user = userEvent.setup()
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    await screen.findByTestId('perf-card-ttft-value')
    await user.click(screen.getByTestId('perf-range-7d'))

    await waitFor(() => {
      expect(requests.at(-1)?.searchParams.get('range')).toBe('7d')
    })
  })

  it('lists derived incidents with start, duration and reason', async () => {
    servePerf(() =>
      perf({
        incidents: [
          {
            start: '2026-10-19T10:12:00Z',
            end: '2026-10-19T10:31:00Z',
            duration_seconds: 1140,
            severity: 'major',
            reason: 'availability_below_threshold',
          },
        ],
      }),
    )
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    const incident = await screen.findByTestId('perf-incident')
    expect(incident.textContent).toContain('19m')
    expect(incident.textContent).toContain('Availability below threshold')
  })

  it('surfaces the metric discrepancy instead of silently choosing one rate', async () => {
    servePerf(() =>
      perf({
        comparison: {
          pipeline_success_rate: 0.993,
          usage_logs_success_rate: 0.95,
          delta: 0.043,
        },
      }),
    )
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    const notice = await screen.findByTestId('perf-comparison')
    expect(notice.textContent).toContain('Metric discrepancy')
    expect(notice.textContent).toContain('99.3%')
    expect(notice.textContent).toContain('95.0%')
  })

  it('shows a retryable inline error when the metrics endpoint fails', async () => {
    let attempts = 0
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, () => {
        attempts += 1
        return HttpResponse.json(
          { type: 'about:blank', title: 'Internal server error' },
          { status: 500 },
        )
      }),
    )
    const user = userEvent.setup()
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    expect(
      await screen.findByText('Performance data unavailable.'),
    ).toBeTruthy()
    await user.click(screen.getByTestId('perf-retry'))
    await waitFor(() => {
      expect(attempts).toBe(2)
    })
  })

  it('renders the model comparison table with health badges', async () => {
    servePerf(() => perf())
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    const table = await screen.findByTestId('perf-models')
    expect(within(table).getByText('gpt-4o')).toBeTruthy()
    expect(within(table).getByText('claude-sonnet')).toBeTruthy()
    expect(within(table).getByText('Fast')).toBeTruthy()
    expect(within(table).getByText('Healthy')).toBeTruthy()
  })

  it('plots rates on a 0–100 percentage scale and the table matches the KPI', async () => {
    servePerf(() => perf())
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    const chart = await screen.findByTestId('perf-chart-rates')
    // 10:00 success = (0.99×25 + 0.98×15)/40 = 98.6%，uptime = 99.8%；
    // 10:01 = 99% / 99.9%。轴与数据表都必须是百分数，而不是 0.99。
    const values = within(chart)
      .getAllByTestId('perf-point-value')
      .map((cell) => cell.textContent)
    expect(values).toEqual(['98.6', '99', '99.83', '99.9'])
    // 固定值域 [0,100]：轴顶端恒为 100。
    expect(within(chart).getByText('100')).toBeTruthy()
    expect(within(chart).queryByText('0.99')).toBeNull()
  })

  it('renders >30s for +Inf buckets and model rows instead of 30.0s', async () => {
    servePerf(() =>
      perf({
        buckets: [
          {
            start: '2026-10-19T10:00:00Z',
            model: 'gpt-4o',
            requests: 40,
            ttft_p50_ms: 30000,
            ttft_p95_ms: 30000,
            latency_p50_ms: 1500,
            latency_p95_ms: 30000,
            success_rate: 0.99,
            uptime: 0.999,
            tps: 40,
            ttft_p50_over_30s: true,
            ttft_p95_over_30s: true,
            latency_p95_over_30s: true,
          },
        ],
        summary: summary({
          ttft_p95_ms: 30000,
          latency_p95_ms: 30000,
          ttft_p95_over_30s: true,
          latency_p95_over_30s: true,
        }),
        models: [
          modelRow({
            ttft_p50_ms: 30000,
            ttft_p95_ms: 30000,
            latency_p95_ms: 30000,
            ttft_p50_over_30s: true,
            ttft_p95_over_30s: true,
            latency_p95_over_30s: true,
          }),
        ],
      }),
    )
    await renderWithProviders(
      <ModelPerformance canViewDimension={false} />,
      ['common'],
    )

    expect(
      (await screen.findByTestId('perf-card-ttft-value')).textContent,
    ).toBe('>30s')
    expect(screen.getByTestId('perf-card-latency-value').textContent).toBe(
      '>30s',
    )
    const ttftChart = screen.getByTestId('perf-chart-ttft')
    const latencyChart = screen.getByTestId('perf-chart-latency')
    expect(within(ttftChart).getAllByText('>30s').length).toBeGreaterThan(0)
    expect(within(latencyChart).getAllByText('>30s').length).toBeGreaterThan(0)
    expect(within(screen.getByTestId('perf-models')).getAllByText('>30s').length)
      .toBeGreaterThan(0)
    // LOW-4：模型行的 TTFT P50 也消费 *_over_30s，不能只修 P95。
    expect(
      screen.getByTestId('perf-model-row').textContent,
    ).toContain('>30s / >30s')
    expect(screen.queryByText('30.0s')).toBeNull()
  })
})
