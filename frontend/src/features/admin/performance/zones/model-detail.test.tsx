import { fireEvent, screen, within } from '@testing-library/react'
import { HttpResponse, delay, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { ModelPerfBucket } from '../api'
import { ModelDetail } from './model-detail'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

// 本地时间构造：x 轴刻度断言不依赖跑测试的时区。
const T0 = new Date(2026, 9, 28, 10, 0, 0).getTime()
const T1 = new Date(2026, 9, 28, 11, 0, 0).getTime()
const T2 = new Date(2026, 9, 28, 12, 0, 0).getTime()

function timeLabel(ms: number): string {
  return new Intl.DateTimeFormat('en', {
    hour: '2-digit',
    minute: '2-digit',
  }).format(ms)
}

interface SeriesBody {
  buckets: ModelPerfBucket[]
  pipeline?: { enabled: boolean; stale: boolean; reason?: string }
}

function bucket(
  startMs: number,
  overrides: Partial<ModelPerfBucket> = {},
): ModelPerfBucket {
  return {
    start: new Date(startMs).toISOString(),
    model: 'gpt-4o',
    requests: 12,
    ttft_p50_ms: 420,
    ttft_p95_ms: 910,
    ttft_p50_over_30s: false,
    ttft_p95_over_30s: false,
    ...overrides,
  }
}

const HEALTHY_PIPELINE = { enabled: true, stale: false }

function useSeries(body: SeriesBody, requests?: URL[]): void {
  server.use(
    http.get(`${ORIGIN}/api/model-perf`, ({ request }) => {
      requests?.push(new URL(request.url))
      return HttpResponse.json(body)
    }),
  )
}

async function renderDetail(model = 'gpt-4o') {
  return renderWithProviders(<ModelDetail model={model} />, ['admin', 'common'])
}

function dataTableRows(): HTMLElement[] {
  const details = screen.getByText('View data table').closest('details')
  if (details === null) throw new Error('missing data-table details')
  return within(details as HTMLElement).getAllByRole('row')
}

describe('ModelDetail', () => {
  it('renders the legended p50/p95 chart with an epoch-millisecond x axis', async () => {
    useSeries({
      buckets: [bucket(T0), bucket(T1)],
      pipeline: HEALTHY_PIPELINE,
    })

    await renderDetail()
    const chart = await screen.findByTestId('line-chart')

    expect(
      screen.getByRole('heading', {
        name: 'gpt-4o · first-token latency trend',
      }),
    ).toBeTruthy()
    expect(
      screen.getByText(
        'Last 24 hours · live relay traffic, aggregated per minute · milliseconds',
      ),
    ).toBeTruthy()

    // 每个图例标签恰好出现一次；数据表表头用的是 `p50 (ms)` / `p95 (ms)`。
    expect(screen.getAllByText('TTFT p50')).toHaveLength(1)
    expect(screen.getAllByText('TTFT p95')).toHaveLength(1)

    const image = within(chart).getByRole('img')
    const ariaLabel = image.getAttribute('aria-label') ?? ''
    expect(ariaLabel).toContain('gpt-4o')
    expect(ariaLabel).toContain('2 series')
    expect(ariaLabel).toContain('ms')

    expect(chart.querySelector('[data-series-key="ttft_p50"]')).not.toBeNull()
    expect(chart.querySelector('[data-series-key="ttft_p95"]')).not.toBeNull()

    // x 轴必须按 epoch 毫秒格式化。若把桶序号当 x，这里会是 1970 年的刻度。
    expect(within(image).getByText(timeLabel(T0))).toBeTruthy()
    expect(chart.textContent).not.toContain('1970')
  })

  it('breaks the line at over-30s points and reads >30s in the data table', async () => {
    useSeries({
      buckets: [
        bucket(T0),
        bucket(T1, { ttft_p95_ms: 30000, ttft_p95_over_30s: true }),
        bucket(T2),
      ],
      pipeline: HEALTHY_PIPELINE,
    })

    await renderDetail()
    const chart = await screen.findByTestId('line-chart')

    // 中间的 p95 是 null：折线分成两段（两个 M 子路径），而不是跨洞连线。
    const p95Path = chart.querySelector('[data-series-key="ttft_p95"]')
    const p50Path = chart.querySelector('[data-series-key="ttft_p50"]')
    expect((p95Path?.getAttribute('d')?.match(/M/g) ?? []).length).toBe(2)
    expect((p50Path?.getAttribute('d')?.match(/M/g) ?? []).length).toBe(1)

    fireEvent.click(screen.getByText('View data table'))
    const rows = dataTableRows()
    // 1 行表头 + 3 个桶。
    expect(rows).toHaveLength(4)
    expect(rows[0]?.textContent).toContain('Samples')
    // 第二个桶的 p95 越过 30s：折线断开，但读数是已知下界 `>30s` 而非 `—`。
    expect(rows[2]?.textContent).toContain('>30s')
    expect(rows[2]?.textContent).not.toContain('—')
    expect(rows[2]?.textContent).toContain('12')
    // 两侧的有限点照常渲染。
    expect(rows[1]?.textContent).toContain('910')
    expect(rows[3]?.textContent).toContain('910')
  })

  it('renders a busy skeleton while the series is loading', async () => {
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, async () => {
        await delay(50)
        return HttpResponse.json({
          buckets: [bucket(T0)],
          pipeline: HEALTHY_PIPELINE,
        })
      }),
    )

    await renderDetail()

    const loading = screen.getByTestId('model-detail-loading')
    expect(loading.getAttribute('role')).toBe('status')
    expect(loading.getAttribute('aria-busy')).toBe('true')

    await screen.findByTestId('line-chart')
  })

  it('shows a retryable error state instead of the empty gate when the request fails', async () => {
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, () => {
        calls += 1
        if (calls === 1) return new HttpResponse(null, { status: 500 })
        return HttpResponse.json({
          buckets: [bucket(T0)],
          pipeline: HEALTHY_PIPELINE,
        })
      }),
    )

    await renderDetail()

    const alert = await screen.findByTestId('model-detail-error')
    expect(alert.getAttribute('role')).toBe('alert')
    expect(
      within(alert).getByText('Performance data unavailable.'),
    ).toBeTruthy()
    expect(screen.queryByTestId('model-detail-empty')).toBeNull()

    fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }))
    await screen.findByTestId('line-chart')
    expect(calls).toBe(2)
    expect(screen.queryByTestId('model-detail-error')).toBeNull()
  })

  it.each([
    [
      'the pipeline is disabled',
      {
        buckets: [bucket(T0)],
        pipeline: { enabled: false, stale: true, reason: 'collection_disabled' },
      },
    ],
    [
      'the model has no buckets',
      { buckets: [], pipeline: HEALTHY_PIPELINE },
    ],
    ['the response omits the pipeline', { buckets: [bucket(T0)] }],
  ])('renders the empty gate when %s', async (_label, body) => {
    useSeries(body)

    await renderDetail()

    const empty = await screen.findByTestId('model-detail-empty')
    expect(within(empty).getByText('No samples yet')).toBeTruthy()
    expect(
      within(empty).getByText('Data appears after the next probe round.'),
    ).toBeTruthy()
    expect(screen.queryByTestId('line-chart')).toBeNull()
  })

  it('renders out-of-order buckets in time order', async () => {
    useSeries({
      buckets: [bucket(T1), bucket(T0)],
      pipeline: HEALTHY_PIPELINE,
    })

    await renderDetail()
    await screen.findByTestId('line-chart')

    const rows = dataTableRows()
    expect(rows[1]?.textContent).toContain(timeLabel(T0))
    expect(rows[2]?.textContent).toContain(timeLabel(T1))
  })

  it('drops a bucket with an invalid start without crashing', async () => {
    useSeries({
      buckets: [bucket(T0, { start: 'not-a-date' }), bucket(T1)],
      pipeline: HEALTHY_PIPELINE,
    })

    await renderDetail()
    const chart = await screen.findByTestId('line-chart')

    const rows = dataTableRows()
    // 只有合法的桶进入 x 轴与数据表。
    expect(rows).toHaveLength(2)
    expect(rows[1]?.textContent).toContain(timeLabel(T1))
    expect(chart.querySelector('[data-series-key="ttft_p95"]')).not.toBeNull()
  })

  it('requests the 24h series with the model encoded in the query string', async () => {
    const requests: URL[] = []
    useSeries(
      { buckets: [bucket(T0)], pipeline: HEALTHY_PIPELINE },
      requests,
    )

    await renderDetail('org/model&x=1')
    await screen.findByTestId('line-chart')

    expect(requests).toHaveLength(1)
    expect(requests[0]?.searchParams.get('range')).toBe('24h')
    expect(requests[0]?.searchParams.get('model')).toBe('org/model&x=1')
  })
})
