import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PerfModels } from '../api'
import { ModelPerfZone } from './model-perf-zone'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

type ModelRow = PerfModels['rows'][number]

function sparkPoints(): (number | null)[] {
  return Array.from({ length: 24 }, (_, index) =>
    index % 5 === 0 ? null : 400 + index * 10,
  )
}

function modelRow(overrides: Partial<ModelRow> = {}): ModelRow {
  return {
    model: 'gpt-4o',
    channels: 4,
    ttft_p50_ms: 420,
    ttft_p95_ms: 910,
    ttft_p50_over_30s: false,
    ttft_p95_over_30s: false,
    latency_p95_ms: 3200,
    latency_p95_over_30s: false,
    tps: 48.2,
    samples: 1284,
    success_rate: 0.994,
    uptime: 0.9998,
    probe: null,
    status: 'normal',
    spark: sparkPoints(),
    ...overrides,
  }
}

function modelsFixture(rows: ModelRow[]): PerfModels {
  return {
    generated_at: '2026-10-28T10:00:00Z',
    sources: [
      { name: 'perfmetrics', status: 'ok' },
      { name: 'probes', status: 'ok' },
    ],
    rows,
    summary: {
      models: rows.length,
      degraded: rows.filter((row) => row.status === 'degraded').length,
      incident: rows.filter((row) => row.status === 'incident').length,
      no_sample: rows.filter((row) => row.status === 'no_sample').length,
    },
  }
}

const DEGRADED = modelRow({
  model: 'gemini-2.5-pro',
  channels: 3,
  ttft_p50_ms: 1240,
  ttft_p95_ms: 30000,
  ttft_p95_over_30s: true,
  latency_p95_ms: 8100,
  success_rate: 0.978,
  uptime: 0.994,
  tps: 22.4,
  samples: 611,
  status: 'degraded',
})

const NO_SAMPLE = modelRow({
  model: 'moonshot-kimi-k2',
  channels: 1,
  ttft_p50_ms: null,
  ttft_p95_ms: null,
  latency_p95_ms: null,
  tps: null,
  samples: 0,
  success_rate: null,
  uptime: null,
  probe: null,
  status: 'no_sample',
  spark: [],
})

/** 展开后的下钻请求需要 msw 应答（onUnhandledRequest:'error' 会兜底预取）。 */
function seriesBody() {
  return {
    buckets: [
      {
        start: '2026-10-28T09:00:00Z',
        model: 'gpt-4o',
        requests: 12,
        ttft_p50_ms: 420,
        ttft_p95_ms: 910,
        ttft_p50_over_30s: false,
        ttft_p95_over_30s: false,
      },
    ],
    pipeline: { enabled: true, stale: false },
  }
}

async function renderZone(options: {
  rows?: ModelRow[]
  onRunNow?: () => void
  sourceDown?: boolean
} = {}) {
  return renderWithProviders(
    <ModelPerfZone
      models={modelsFixture(options.rows ?? [modelRow(), DEGRADED, NO_SAMPLE])}
      sourceDown={options.sourceDown ?? false}
      onRunNow={options.onRunNow}
    />,
    ['admin', 'common'],
  )
}

function rowFor(model: string): HTMLElement {
  const row = screen
    .getAllByTestId('model-row')
    .find((element) => element.getAttribute('data-model') === model)
  if (row === undefined) throw new Error(`missing model row ${model}`)
  return row
}

describe('ModelPerfZone', () => {
  it('renders the ten table headers in the frozen order', async () => {
    await renderZone()

    const headers = screen
      .getAllByRole('columnheader')
      .map((header) => header.textContent)
    expect(headers).toEqual([
      'Model',
      'TTFT p50',
      'TTFT p95',
      'End-to-end p95',
      'Success rate',
      'Uptime',
      'TPS',
      'Samples',
      'Trend',
      'Status',
    ])
  })

  it('renders the readings, channel subtitle and source footnotes', async () => {
    await renderZone()

    const row = rowFor('gpt-4o')
    expect(row.textContent).toContain('420ms')
    expect(row.textContent).toContain('910ms')
    expect(row.textContent).toContain('3.2s')
    // 成功率 1 位小数，可用率 2 位小数：0.9998 若退回 1 位会变成 100.0%。
    expect(row.textContent).toContain('99.4%')
    expect(row.textContent).toContain('99.98%')
    expect(row.textContent).toContain('48.2')
    expect(row.textContent).toContain('1,284')
    expect(within(row).getByText('4 channels')).toBeTruthy()

    const status = within(row).getByTestId('status-badge')
    expect(status.textContent).toBe('Normal')
    expect(status.getAttribute('data-variant')).toBe('success')

    const footer = screen.getByTestId('models-footer')
    expect(footer.textContent).toContain(
      'Percentiles and TPS come from live relay traffic',
    )
    expect(footer.textContent).toContain(
      'Availability and status come from active probes',
    )
  })

  it('formats over-30s buckets as >30s and tones degraded trends as warning', async () => {
    await renderZone()

    const row = rowFor('gemini-2.5-pro')
    expect(row.textContent).toContain('1.2s')
    expect(row.textContent).toContain('>30s')
    expect(row.textContent).toContain('8.1s')
    // 成功率保持 1 位小数；可用率 2 位（0.994 → 99.40%）。
    expect(row.textContent).toContain('97.8%')
    expect(row.textContent).toContain('99.40%')
    expect(within(row).getByTestId('status-badge').getAttribute('data-variant')).toBe(
      'warning',
    )
    expect(
      within(row).getByTestId('model-spark').getAttribute('class'),
    ).toContain('text-warning')
  })

  it('renders a no_sample row as dashes with no sparkline and no channel subtitle', async () => {
    await renderZone()

    const row = rowFor('moonshot-kimi-k2')
    expect(within(row).getByText('No sample yet')).toBeTruthy()
    // 七个数字列（p50 / p95 / 端到端 / 成功率 / 可用率 / TPS / 样本）全部 —。
    expect(within(row).getAllByText('—')).toHaveLength(7)
    expect(within(row).queryByTestId('model-spark')).toBeNull()
    expect(within(row).queryByText('1 channels')).toBeNull()

    const status = within(row).getByTestId('status-badge')
    expect(status.textContent).toBe('Awaiting sample')
    expect(status.getAttribute('data-variant')).toBe('neutral')
  })

  it('omits the channel sub-line when the channel count is unknown', async () => {
    await renderZone({ rows: [modelRow({ channels: null })] })

    const row = rowFor('gpt-4o')
    expect(row.textContent).not.toContain('channels')
    // 数字列照常渲染，只有副标题缺席。
    expect(row.textContent).toContain('420ms')
  })

  it('does not request the drill-down series until a row is expanded', async () => {
    const requests: URL[] = []
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, ({ request }) => {
        requests.push(new URL(request.url))
        return HttpResponse.json(seriesBody())
      }),
    )

    await renderZone()
    // 渲染三行后仍没有下钻请求：折叠态不预取。
    expect(requests).toHaveLength(0)

    fireEvent.click(rowFor('gpt-4o'))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.searchParams.get('range')).toBe('24h')
    expect(requests[0]?.searchParams.get('model')).toBe('gpt-4o')

    await screen.findByTestId('model-detail')
    expect(rowFor('gpt-4o').getAttribute('aria-expanded')).toBe('true')
    expect(rowFor('gemini-2.5-pro').getAttribute('aria-expanded')).toBe('false')

    // 触发器与展开行通过 aria-controls / id 关联。
    const control = rowFor('gpt-4o').getAttribute('aria-controls')
    expect(control).toBeTruthy()
    expect(screen.getByTestId('model-detail-row').id).toBe(control)
  })

  it('keeps at most one row open and closes it on a second activation', async () => {
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, () =>
        HttpResponse.json(seriesBody()),
      ),
    )

    await renderZone()

    fireEvent.click(rowFor('gpt-4o'))
    await screen.findByTestId('model-detail')

    fireEvent.click(rowFor('gemini-2.5-pro'))
    await waitFor(() =>
      expect(screen.getAllByTestId('model-detail-row')).toHaveLength(1),
    )
    expect(rowFor('gpt-4o').getAttribute('aria-expanded')).toBe('false')
    expect(rowFor('gemini-2.5-pro').getAttribute('aria-expanded')).toBe('true')

    fireEvent.click(rowFor('gemini-2.5-pro'))
    await waitFor(() =>
      expect(screen.queryByTestId('model-detail-row')).toBeNull(),
    )
    expect(rowFor('gemini-2.5-pro').getAttribute('aria-expanded')).toBe('false')
  })

  it('toggles a row with Enter and Space', async () => {
    server.use(
      http.get(`${ORIGIN}/api/model-perf`, () =>
        HttpResponse.json(seriesBody()),
      ),
    )

    await renderZone()
    const row = rowFor('gpt-4o')

    fireEvent.keyDown(row, { key: 'Enter' })
    await screen.findByTestId('model-detail')
    expect(rowFor('gpt-4o').getAttribute('aria-expanded')).toBe('true')

    fireEvent.keyDown(row, { key: ' ' })
    await waitFor(() =>
      expect(screen.queryByTestId('model-detail-row')).toBeNull(),
    )
    expect(rowFor('gpt-4o').getAttribute('aria-expanded')).toBe('false')
  })

  it('keeps the model table intact while the monitoring source is down', async () => {
    await renderZone({ sourceDown: true })

    expect(screen.getAllByTestId('model-row')).toHaveLength(3)
    expect(rowFor('gpt-4o').textContent).toContain('420ms')
  })

  it('renders the empty state without a button when no run action is wired', async () => {
    await renderZone({ rows: [] })

    const empty = screen.getByTestId('models-empty')
    expect(within(empty).getByText('No samples yet')).toBeTruthy()
    expect(
      within(empty).getByText('Data appears after the next probe round.'),
    ).toBeTruthy()
    expect(within(empty).queryByRole('button')).toBeNull()
  })

  it('offers a run-now button in the empty state when the page passes an action', async () => {
    const onRunNow = vi.fn()
    await renderZone({ rows: [], onRunNow })

    fireEvent.click(
      within(screen.getByTestId('models-empty')).getByRole('button', {
        name: 'Run a round now',
      }),
    )
    expect(onRunNow).toHaveBeenCalledTimes(1)
  })
})
