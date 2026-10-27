import { fireEvent, screen, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
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

function useSeries(
  body: {
    buckets: ModelPerfBucket[]
    pipeline: { enabled: boolean; stale: boolean; reason?: string }
  },
  requests?: URL[],
): void {
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

describe('ModelDetail', () => {
  it('renders the legended p50/p95 chart with an epoch-millisecond x axis', async () => {
    useSeries({
      buckets: [bucket(T0), bucket(T1)],
      pipeline: { enabled: true, stale: false },
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

    // ≥2 序列必有图例；两个标签在图例与数据表表头各出现一次。
    expect(screen.getAllByText('TTFT p50').length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText('TTFT p95').length).toBeGreaterThanOrEqual(1)

    const image = within(chart).getByRole('img')
    const ariaLabel = image.getAttribute('aria-label') ?? ''
    expect(ariaLabel).toContain('gpt-4o')
    expect(ariaLabel).toContain('2 series')
    expect(ariaLabel).toContain('ms')

    expect(chart.querySelector('[data-series-key="ttft_p50"]')).not.toBeNull()
    expect(chart.querySelector('[data-series-key="ttft_p95"]')).not.toBeNull()

    // x 轴必须按 epoch 毫秒格式化。若把桶序号当 x，这里会是 1970 年的刻度。
    const expectedTick = new Intl.DateTimeFormat('en', {
      hour: '2-digit',
      minute: '2-digit',
    }).format(T0)
    expect(within(image).getByText(expectedTick)).toBeTruthy()
    expect(chart.textContent).not.toContain('1970')
  })

  it('breaks over-30s points and shows a dash in the data table', async () => {
    useSeries({
      buckets: [
        bucket(T0),
        bucket(T1, { ttft_p95_ms: 30000, ttft_p95_over_30s: true }),
      ],
      pipeline: { enabled: true, stale: false },
    })

    await renderDetail()
    await screen.findByTestId('line-chart')

    fireEvent.click(screen.getByText('View data table'))
    const details = screen.getByText('View data table').closest('details')
    if (details === null) throw new Error('missing data-table details')

    const rows = within(details as HTMLElement).getAllByRole('row')
    // 1 行表头 + 2 个桶。
    expect(rows).toHaveLength(3)
    expect(rows[0]?.textContent).toContain('Samples')
    // 第二个桶的 p95 越过 30s：无限值 → 表格显示 —，样本列仍保留 requests。
    expect(rows[2]?.textContent).toContain('—')
    expect(rows[2]?.textContent).toContain('12')
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
      { buckets: [], pipeline: { enabled: true, stale: false } },
    ],
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

  it('requests the 24h series with the model encoded in the query string', async () => {
    const requests: URL[] = []
    useSeries(
      { buckets: [bucket(T0)], pipeline: { enabled: true, stale: false } },
      requests,
    )

    await renderDetail('org/model x')
    await screen.findByTestId('line-chart')

    expect(requests).toHaveLength(1)
    expect(requests[0]?.searchParams.get('range')).toBe('24h')
    expect(requests[0]?.searchParams.get('model')).toBe('org/model x')
  })
})
