import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PerfModels, PerfOverview } from '../api'
import { VitalsStrip } from './vitals-strip'

type Vital = PerfOverview['vitals'][number]
type ModelSummary = PerfModels['summary']

function vital(overrides: Partial<Vital>): Vital {
  return {
    key: 'gateway.rps',
    value: 21.44,
    unit: 'req/s',
    status: 'ok',
    details: [],
    series: [],
    ...overrides,
  }
}

const VITALS: Vital[] = [
  vital({
    key: 'gateway.rps',
    value: 21.44,
    details: [
      { key: 'p95', value: 1.84, unit: 's' },
      { key: 'errors', value: 0.0002, unit: 'ratio' },
    ],
    series: [
      [1_000, 20],
      [1_001, 21.44],
    ],
  }),
  vital({
    key: 'db.pool',
    value: 11,
    status: 'ok',
    details: [
      { key: 'tps', value: 342, unit: 'count' },
      { key: 'backends', value: 11, unit: 'count' },
      { key: 'max_backends', value: 25, unit: 'count' },
    ],
  }),
  vital({
    key: 'redis.hit_rate',
    value: 0.962,
    status: 'ok',
    details: [
      { key: 'ops', value: 1204, unit: 'count' },
      { key: 'clients', value: 186, unit: 'count' },
    ],
  }),
  vital({
    key: 'host.disk',
    value: 0.71,
    status: 'warn',
    details: [
      { key: 'cpu', value: 0.38, unit: 'ratio' },
      { key: 'mem', value: 0.62, unit: 'ratio' },
      { key: 'disk', value: 0.71, unit: 'ratio' },
    ],
  }),
]

const SUMMARY: ModelSummary = {
  models: 20,
  degraded: 14,
  incident: 1,
  no_sample: 1,
}

function cell(container: HTMLElement, key: string): HTMLElement {
  const element = container.querySelector<HTMLElement>(`[data-vital="${key}"]`)
  if (element === null) throw new Error(`missing vital cell ${key}`)
  return element
}

async function renderStrip(options: {
  vitals?: Vital[]
  summary?: ModelSummary | undefined
  sourceDown?: boolean
  modelProbe?: { name: string; at: string | null }
} = {}) {
  const result = await renderWithProviders(
    <VitalsStrip
      vitals={options.vitals ?? VITALS}
      modelSummary={'summary' in options ? options.summary : SUMMARY}
      sourceDown={options.sourceDown ?? false}
      modelProbe={options.modelProbe}
    />,
    ['admin'],
  )
  return result
}

describe('VitalsStrip', () => {
  it('renders four Prometheus cells plus the derived model cell', async () => {
    const { container } = await renderStrip({
      modelProbe: { name: 'claude-haiku-3-5', at: '2026-10-20T18:20:00' },
    })

    expect(screen.getAllByTestId('vital')).toHaveLength(5)

    const gateway = cell(container, 'gateway.rps')
    expect(within(gateway).getByTestId('vital-figure').textContent).toBe('21.4')
    expect(within(gateway).getByText('req/s')).toBeTruthy()
    expect(
      within(gateway).getByText('Ingress P95 1.84s · 5xx 0.0%'),
    ).toBeTruthy()
    expect(within(gateway).getByText('OK')).toBeTruthy()

    const postgres = cell(container, 'db.pool')
    expect(within(postgres).getByTestId('vital-figure').textContent).toBe('11')
    expect(within(postgres).getByText('/ 25 connections')).toBeTruthy()
    expect(
      within(postgres).getByText('342 tx/s · 11 / 25 connections'),
    ).toBeTruthy()

    const redis = cell(container, 'redis.hit_rate')
    expect(within(redis).getByTestId('vital-figure').textContent).toBe('96.2')
    expect(within(redis).getByText('% hit')).toBeTruthy()
    expect(within(redis).getByText('1204 ops/s · 186 clients')).toBeTruthy()

    const host = cell(container, 'host.disk')
    expect(within(host).getByTestId('vital-figure').textContent).toBe('71.0')
    expect(within(host).getByText('% disk')).toBeTruthy()
    expect(within(host).getByText('Watch')).toBeTruthy()
    expect(
      within(host).getByText('CPU 38.0% · memory 62.0% · disk 71.0%'),
    ).toBeTruthy()

    // 第五格：figure 是 incident 数，单位带 degraded 总数，副标题带最近探针。
    const models = cell(container, 'models')
    expect(within(models).getByTestId('vital-figure').textContent).toBe('1')
    expect(within(models).getByText('/ 14 degraded')).toBeTruthy()
    expect(within(models).getByText('Incident')).toBeTruthy()
    expect(
      within(models).getByText('claude-haiku-3-5 · last probe 18:20'),
    ).toBeTruthy()

    // 第五格没有序列，不画假曲线。
    expect(screen.getAllByTestId('vital-spark')).toHaveLength(4)
    expect(within(models).queryByTestId('vital-spark')).toBeNull()
  })

  it('replaces the four Prometheus readings when the source is down but keeps the model cell', async () => {
    const { container } = await renderStrip({ sourceDown: true })

    for (const key of ['gateway.rps', 'db.pool', 'redis.hit_rate', 'host.disk']) {
      const element = cell(container, key)
      expect(within(element).getByTestId('vital-figure').textContent).toBe('—')
      expect(within(element).getByText('No data')).toBeTruthy()
      expect(within(element).queryByTestId('vital-spark')).toBeNull()
    }
    expect(screen.queryAllByTestId('vital-spark')).toHaveLength(0)

    // 模型数据不受监控断线影响。
    const models = cell(container, 'models')
    expect(within(models).getByTestId('vital-figure').textContent).toBe('1')
    expect(within(models).getByText('Incident')).toBeTruthy()
  })

  it.each([
    [{ models: 20, degraded: 2, incident: 0, no_sample: 0 }, 'Watch'],
    [{ models: 20, degraded: 0, incident: 0, no_sample: 3 }, 'OK'],
    [{ models: 0, degraded: 0, incident: 0, no_sample: 0 }, 'No data'],
    [{ models: 20, degraded: 14, incident: 2, no_sample: 0 }, 'Incident'],
  ] as const)('derives the model cell status from %o', async (summary, label) => {
    const { container } = await renderStrip({ summary: { ...summary } })

    expect(within(cell(container, 'models')).getByText(label)).toBeTruthy()
  })

  it('shows an empty model cell when the summary is unavailable', async () => {
    const { container } = await renderStrip({ summary: undefined })

    const models = cell(container, 'models')
    expect(within(models).getByTestId('vital-figure').textContent).toBe('—')
    expect(within(models).getByText('No data')).toBeTruthy()
  })

  it('omits a subtitle when any detail it needs is missing', async () => {
    const { container } = await renderStrip({
      vitals: [
        vital({
          key: 'gateway.rps',
          value: 21.44,
          // errors 缺失：整行副标题不渲染，不显示半截。
          details: [{ key: 'p95', value: 1.84, unit: 's' }],
        }),
      ],
    })

    expect(container.textContent).not.toContain('Ingress P95')
  })

  it('skips unknown vitals and keeps the model cell last', async () => {
    const { container } = await renderStrip({
      vitals: [
        ...VITALS,
        vital({ key: 'exporter.scrape', value: 1 }),
      ],
    })

    const cells = screen.getAllByTestId('vital')
    expect(cells).toHaveLength(5)
    expect(cells[4]?.getAttribute('data-vital')).toBe('models')
    expect(container.querySelector('[data-vital="exporter.scrape"]')).toBeNull()
  })

  it('falls back to — and the status badge when the headline value is null', async () => {
    const { container } = await renderStrip({
      vitals: [vital({ key: 'gateway.rps', value: null, status: 'unknown' })],
    })

    const gateway = cell(container, 'gateway.rps')
    expect(within(gateway).getByTestId('vital-figure').textContent).toBe('—')
    expect(within(gateway).getByText('No data')).toBeTruthy()
  })
})
