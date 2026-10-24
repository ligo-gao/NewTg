import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
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

import type { ReconcileRun, ReconcileSummary } from './api'
import { OverviewTab } from './overview-tab'

/** 按天元素类型从汇总信封派生；`api.ts` 未单独导出它。 */
type ReconcileSummaryDay = ReconcileSummary['days'][number]

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

function makeRun(overrides: Partial<ReconcileRun> = {}): ReconcileRun {
  return {
    id: 9,
    account_id: 1,
    window_start: '2026-10-22',
    window_end: '2026-10-24',
    triggered_by: 'worker',
    status: 'succeeded',
    token_tolerance_bp: 50,
    amount_tolerance_minor: 100,
    quota_per_unit: 100,
    timezone: 'Asia/Shanghai',
    started_at: '2026-10-24T02:00:00Z',
    finished_at: '2026-10-24T02:00:05Z',
    error: '',
    ...overrides,
  }
}

function makeDay(overrides: Partial<ReconcileSummaryDay> = {}): ReconcileSummaryDay {
  return {
    usage_date: '2026-10-24',
    item_count: 1,
    alert_count: 0,
    warn_count: 0,
    delta_token_bp: 0,
    revenue_minor: 0,
    upstream_cost_minor: 0,
    margin_minor: 0,
    ...overrides,
  }
}

interface ApiOptions {
  days?: ReconcileSummaryDay[]
  runs?: ReconcileRun[]
  summaryError?: boolean
  onSummary?: (url: URL) => void
}

function installApi(options: ApiOptions = {}) {
  const summaryCalls: URL[] = []
  const runsCalls: URL[] = []
  const runs = options.runs ?? [makeRun()]

  server.use(
    http.get(`${ORIGIN}/api/upstream-reconciliations/runs`, ({ request }) => {
      runsCalls.push(new URL(request.url))
      return HttpResponse.json({
        items: runs,
        total: runs.length,
        page: 1,
        page_size: 20,
      })
    }),
    http.get(`${ORIGIN}/api/upstream-reconciliations/summary`, ({ request }) => {
      const url = new URL(request.url)
      summaryCalls.push(url)
      options.onSummary?.(url)
      if (options.summaryError === true) {
        return HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Internal error',
            detail: 'summary store unavailable',
          },
          {
            status: 500,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        )
      }
      const summary: ReconcileSummary = {
        run_id: Number(url.searchParams.get('run_id') ?? '9'),
        account_id: 1,
        generated_at: '2026-10-24T02:00:05Z',
        days: options.days ?? [],
      }
      return HttpResponse.json(summary)
    }),
  )

  return { summaryCalls, runsCalls }
}

function renderTab(runId: number | null) {
  return renderWithProviders(<OverviewTab runId={runId} />, ['common', 'admin'])
}

/** 三个 KPI + 一个差异率卡片，值元素由 StatCard 的 `testId-value` 约定命名。 */
const KPI_IDS = [
  'overview-revenue',
  'overview-upstream-cost',
  'overview-margin',
  'overview-delta-bp',
] as const

function kpiValue(id: (typeof KPI_IDS)[number]): string | null {
  return screen.getByTestId(`${id}-value`).textContent
}

describe('reconciliation overview tab', () => {
  it('shows four KPI tiles and a single-series trend', async () => {
    installApi({
      days: [
        makeDay({
          usage_date: '2026-10-22',
          delta_token_bp: 100,
          revenue_minor: 1000,
          upstream_cost_minor: 400,
          margin_minor: 600,
        }),
        makeDay({
          usage_date: '2026-10-23',
          delta_token_bp: -320,
          revenue_minor: 2500,
          upstream_cost_minor: 900,
          margin_minor: 1600,
        }),
        makeDay({
          usage_date: '2026-10-24',
          delta_token_bp: null,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
        }),
      ],
    })
    await renderTab(9)

    for (const id of KPI_IDS) {
      expect(await screen.findByTestId(id)).toBeTruthy()
    }

    expect(screen.getByTestId('overview-revenue').textContent).toContain('Revenue')
    expect(screen.getByTestId('overview-upstream-cost').textContent).toContain(
      'Upstream cost',
    )
    expect(screen.getByTestId('overview-margin').textContent).toContain('Margin')
    expect(screen.getByTestId('overview-delta-bp').textContent).toContain(
      'Token delta',
    )

    const chart = screen.getByTestId('analytics-chart')
    expect(chart.getAttribute('data-kind')).toBe('line')
    // 单一序列：差值率一条线，容差带是参考线而不是第二个序列。
    expect(chart.getAttribute('data-series-count')).toBe('1')
    expect(screen.getByRole('heading', { name: 'Token delta trend' })).toBeTruthy()
    expect(
      document.querySelector('svg polyline[data-series-id="delta_token_bp"]'),
    ).not.toBeNull()
  })

  it('sums the KPI window and names the worst non-null delta day', async () => {
    installApi({
      days: [
        // 故意乱序：聚合前必须按 usage_date 升序，平手时取最早一天。
        makeDay({
          usage_date: '2026-10-23',
          delta_token_bp: -320,
          revenue_minor: 2500,
          upstream_cost_minor: 900,
          margin_minor: 1600,
        }),
        makeDay({
          usage_date: '2026-10-22',
          delta_token_bp: 100,
          revenue_minor: 1000,
          upstream_cost_minor: 400,
          margin_minor: 600,
        }),
        // null 日既不参与求和，也不参与「最差日」比较。
        makeDay({
          usage_date: '2026-10-24',
          delta_token_bp: null,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
        }),
      ],
    })
    await renderTab(9)

    await screen.findByTestId('overview-revenue-value')
    // 精确金额（分 → 元，两位小数）；null 日被跳过，总和不出现 NaN。
    expect(kpiValue('overview-revenue')).toBe('¥35.00')
    expect(kpiValue('overview-upstream-cost')).toBe('¥13.00')
    expect(kpiValue('overview-margin')).toBe('¥22.00')
    // 绝对值最大的一天：-320，而不是 100，也不是 null 日。
    expect(kpiValue('overview-delta-bp')).toBe('-320')
    expect(screen.getByTestId('overview-delta-bp-subline').textContent).toBe(
      'Worst day: 2026-10-23',
    )
    expect(
      within(screen.getByTestId('overview-delta-bp')).getByText('bp'),
    ).toBeTruthy()
  })

  it('renders — for every KPI when the window has no money or delta', async () => {
    installApi({
      days: [
        makeDay({
          usage_date: '2026-10-24',
          delta_token_bp: null,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
        }),
      ],
    })
    await renderTab(9)

    await screen.findByTestId('overview-revenue-value')
    for (const id of KPI_IDS) {
      expect(kpiValue(id)).toBe('—')
    }
    // 没有可比较的天：差异率没有最差日小字；图为空态（图表组件自己的空态）。
    expect(screen.queryByTestId('overview-delta-bp-subline')).toBeNull()
    expect(screen.getByTestId('analytics-chart-empty')).toBeTruthy()
  })

  it('plots no point for a null delta day instead of a zero', async () => {
    installApi({
      days: [
        makeDay({
          usage_date: '2026-10-22',
          delta_token_bp: 100,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
        }),
        makeDay({
          usage_date: '2026-10-23',
          delta_token_bp: null,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
        }),
        makeDay({
          usage_date: '2026-10-24',
          delta_token_bp: 200,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
        }),
      ],
    })
    await renderTab(9)

    const table = await screen.findByRole('table')
    // 数据行 = 非 null 的天数；若把 null 当成 0 就会多出一行。
    expect(table.querySelectorAll('tbody tr')).toHaveLength(2)
    expect(table.textContent).toContain('2026-10-22T00:00:00.000Z')
    expect(table.textContent).toContain('2026-10-24T00:00:00.000Z')
    expect(table.textContent).not.toContain('2026-10-23T00:00:00.000Z')
  })

  it('never renders a second y-axis', async () => {
    installApi({
      days: [
        makeDay({ usage_date: '2026-10-22', delta_token_bp: 100 }),
        makeDay({ usage_date: '2026-10-23', delta_token_bp: -320 }),
      ],
    })
    const { container } = await renderTab(9)

    await screen.findByTestId('analytics-chart')
    const svg = container.querySelector('svg')
    expect(svg).not.toBeNull()
    if (svg === null) throw new Error('chart svg missing')

    // 一根 Y 轴 = 一条竖线（x1 === x2）；第二根 Y 轴会多出一条竖线。
    const verticals = [...svg.querySelectorAll('line')].filter(
      (line) => line.getAttribute('x1') === line.getAttribute('x2'),
    )
    expect(verticals).toHaveLength(1)
    // Y 轴只标注 min/max 两个刻度（刻度文字位于绘图区左侧）；第二根轴会
    // 带来额外刻度，X 轴末端标签虽然也是 end 对齐，但 x 在绘图区内。
    const yTicks = [...svg.querySelectorAll('text')].filter(
      (text) => Number(text.getAttribute('x')) < 44,
    )
    expect(yTicks).toHaveLength(2)
  })

  it('draws the dashed ±tolerance band when the run is in the run list', async () => {
    installApi({
      runs: [makeRun({ id: 9, token_tolerance_bp: 50 })],
      days: [
        makeDay({ usage_date: '2026-10-22', delta_token_bp: 100 }),
        makeDay({ usage_date: '2026-10-23', delta_token_bp: -320 }),
      ],
    })
    const { container } = await renderTab(9)

    await screen.findByTestId('analytics-chart')
    const lines = [
      ...container.querySelectorAll('line[data-reference-line="true"]'),
    ]
    expect(lines).toHaveLength(2)
    for (const line of lines) {
      expect(line.getAttribute('stroke-dasharray')).not.toBeNull()
    }
    // 参考线信息进入无障碍摘要，不只靠颜色/虚线表达。
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'Tolerance ±50 bp',
    )
  })

  it('omits the band when the run is not in the run list', async () => {
    installApi({
      runs: [makeRun({ id: 8, token_tolerance_bp: 50 })],
      days: [makeDay({ usage_date: '2026-10-22', delta_token_bp: 100 })],
    })
    const { container } = await renderTab(9)

    await screen.findByTestId('analytics-chart')
    // 不在首页运行列表里：宁可不画，也不猜一个容忍度。
    expect(container.querySelectorAll('line[data-reference-line="true"]')).toHaveLength(
      0,
    )
  })

  it('renders genuine zeros as 0 and ¥0.00 rather than —', async () => {
    installApi({
      days: [
        makeDay({
          usage_date: '2026-10-22',
          delta_token_bp: 0,
          revenue_minor: 0,
          upstream_cost_minor: 0,
          margin_minor: 0,
        }),
      ],
    })
    await renderTab(9)

    // 0 是真实值；只有 NULL 才渲染 —。四个格式化器必须都保留 0。
    await screen.findByTestId('overview-revenue-value')
    expect(kpiValue('overview-revenue')).toBe('¥0.00')
    expect(kpiValue('overview-upstream-cost')).toBe('¥0.00')
    expect(kpiValue('overview-margin')).toBe('¥0.00')
    expect(kpiValue('overview-delta-bp')).toBe('0')
    expect(screen.getByTestId('overview-delta-bp-subline').textContent).toBe(
      'Worst day: 2026-10-22',
    )
  })

  it('breaks a worst-day tie with the earlier date', async () => {
    installApi({
      days: [
        // 服务端顺序把较晚的一天放在前面；绝对值相同（100）时必须选 10-22。
        makeDay({ usage_date: '2026-10-23', delta_token_bp: 100 }),
        makeDay({ usage_date: '2026-10-22', delta_token_bp: -100 }),
      ],
    })
    await renderTab(9)

    await screen.findByTestId('overview-delta-bp-value')
    expect(kpiValue('overview-delta-bp')).toBe('-100')
    expect(screen.getByTestId('overview-delta-bp-subline').textContent).toBe(
      'Worst day: 2026-10-22',
    )
  })

  it('shows the hovered day value through the chart tooltip', async () => {
    installApi({
      days: [
        makeDay({ usage_date: '2026-10-22', delta_token_bp: 100 }),
        makeDay({ usage_date: '2026-10-23', delta_token_bp: 200 }),
        makeDay({ usage_date: '2026-10-24', delta_token_bp: 300 }),
      ],
    })
    const { container } = await renderTab(9)
    await screen.findByTestId('analytics-chart')
    const svg = screen.getByRole('img')
    // jsdom 不做布局：viewBox 是 320×180，最后一点在 x=312。
    vi.spyOn(svg, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: 320,
      bottom: 180,
      width: 320,
      height: 180,
      toJSON: () => ({}),
    })

    fireEvent.mouseMove(svg, { clientX: 312 })
    expect(screen.getByTestId('analytics-chart-tooltip').textContent).toBe(
      '300 bp',
    )
    expect(container.querySelector('line[data-crosshair="true"]')).not.toBeNull()
  })

  it('asks for a run in the results tab when no run is selected', async () => {
    const { summaryCalls, runsCalls } = installApi()
    await renderTab(null)

    expect(
      screen.getByText('Choose a run in the Results tab to see its overview.'),
    ).toBeTruthy()
    expect(screen.queryByTestId('analytics-chart')).toBeNull()
    // 没有 run 就两个请求都不发：汇总没有 run_id，运行列表也只服务于容差带。
    // 让出一轮事件循环，否则任何挂载即发的请求都还来不及到达 MSW。
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(summaryCalls).toHaveLength(0)
    expect(runsCalls).toHaveLength(0)
  })

  it('sends the selected run id and the documented runs page parameters', async () => {
    // 用一个非默认 run id：把 run_id 写死成 9 的实现会在这里暴露。
    const { summaryCalls, runsCalls } = installApi({
      runs: [makeRun({ id: 42 })],
      days: [makeDay({ usage_date: '2026-10-22', delta_token_bp: 10 })],
    })
    await renderTab(42)

    await screen.findByTestId('analytics-chart')
    expect(summaryCalls[0]?.searchParams.get('run_id')).toBe('42')
    expect(runsCalls[0]?.searchParams.get('p')).toBe('1')
    expect(runsCalls[0]?.searchParams.get('page_size')).toBe('20')
  })

  it('renders an empty state for a run with no days', async () => {
    installApi({ days: [] })
    await renderTab(9)

    expect(
      await screen.findByText(
        'This run has no reconciliation days to chart.',
      ),
    ).toBeTruthy()
    expect(screen.queryByTestId('analytics-chart')).toBeNull()
  })

  it('renders the RFC 7807 detail and retries the summary', async () => {
    const { summaryCalls } = installApi({ summaryError: true })
    await renderTab(9)
    const user = userEvent.setup()

    expect(await screen.findByText('summary store unavailable')).toBeTruthy()
    // 四张卡片的错误重试按钮必须有各自的名称，不能都叫「Retry」。
    const names = screen.getAllByRole('button').map((button) => button.textContent)
    expect(names).toEqual([
      'Retry Revenue',
      'Retry Upstream cost',
      'Retry Margin',
      'Retry Token delta',
    ])
    expect(new Set(names).size).toBe(4)

    // StatCard 的错误态保留重试入口；点击后重新请求汇总。
    const retry = screen.getByTestId('overview-revenue-retry')
    const before = summaryCalls.length
    await user.click(retry)
    await waitFor(() => {
      expect(summaryCalls.length).toBeGreaterThan(before)
    })
  })
})
