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
})
