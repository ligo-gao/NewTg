import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
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

import type { ReconcileItem, ReconcileRun, ReconcileRunResult } from './api'
import { ResultsTab } from './results-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

const RUNS: ReconcileRun[] = [
  {
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
  },
  {
    id: 8,
    account_id: 1,
    window_start: '2026-10-19',
    window_end: '2026-10-21',
    triggered_by: 'manual',
    status: 'partial',
    token_tolerance_bp: 50,
    amount_tolerance_minor: 100,
    quota_per_unit: 100,
    timezone: 'Asia/Shanghai',
    started_at: '2026-10-21T02:00:00Z',
    finished_at: '2026-10-21T02:00:05Z',
    error: '',
  },
]

function makeRun(
  id: number,
  startedAt: string,
  overrides: Partial<ReconcileRun> = {},
): ReconcileRun {
  return {
    id,
    account_id: 1,
    window_start: '2026-10-01',
    window_end: '2026-10-02',
    triggered_by: 'worker',
    status: 'succeeded',
    token_tolerance_bp: 50,
    amount_tolerance_minor: 100,
    quota_per_unit: 100,
    timezone: 'Asia/Shanghai',
    started_at: startedAt,
    finished_at: startedAt,
    error: '',
    ...overrides,
  }
}

const RUN_100 = makeRun(100, '2026-10-03T02:00:00Z')
const RUN_200 = makeRun(200, '2026-10-04T02:00:00Z')

/** 请求在此 Promise 完成前不返回：用于观察占位 / 加载中的表格形态。 */
function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve: () => void = () => undefined
  const promise = new Promise<void>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

function makeItem(overrides: Partial<ReconcileItem> = {}): ReconcileItem {
  return {
    id: 501,
    run_id: 9,
    account_id: 1,
    usage_date: '2026-10-24',
    upstream_model: 'deepseek-chat',
    local_requests: 5,
    local_estimated_requests: 0,
    local_refunded_requests: 0,
    local_cache_hit_tokens: 100,
    local_cache_miss_tokens: 25,
    local_output_tokens: 300,
    local_wallet_credits: 380,
    local_subscription_credits: 0,
    local_cost_credits: 380,
    local_unknown_requests: 0,
    local_multi_attempt_requests: 0,
    snapshot_cache_hit_tokens: 90,
    snapshot_cache_miss_tokens: 20,
    snapshot_cache_write_tokens: 5,
    snapshot_output_tokens: 300,
    snapshot_requests: 4,
    snapshot_amount_minor: 1234,
    delta_cache_hit_tokens: 10,
    delta_cache_miss_tokens: 5,
    delta_output_tokens: 0,
    delta_token_bp: 250,
    snapshot_partial: false,
    revenue_minor: 380,
    upstream_cost_minor: 1234,
    margin_minor: -854,
    margin_bp: -22474,
    severity: 'warn',
    status: 'open',
    note: '',
    created_at: '2026-10-24T02:00:00Z',
    updated_at: '2026-10-24T02:00:00Z',
    ...overrides,
  }
}

interface TriggerCall {
  body: unknown
  idempotencyKey: string | null
}

interface ApiOptions {
  items?: ReconcileItem[]
  /** 按运行返回不同明细；缺省回退到 `items`。 */
  itemsByRun?: Record<string, ReconcileItem[]>
  /** 明细请求在该 Promise 完成前不返回；按 URL 与运行 id 决定是否拦截。 */
  listGate?: (url: URL, runId: string) => Promise<void> | undefined
  listTotal?: number
  severityCounts?: Partial<Record<string, number>>
  itemsError?: boolean
  runs?: ReconcileRun[]
  onList?: (url: URL) => void
  onAck?: (body: unknown) => void
  triggerResult?: ReconcileRunResult
  triggerError?: { status: number; title: string; detail: string }
  onTrigger?: (call: TriggerCall) => void
}

function installApi(options: ApiOptions = {}) {
  const state = { acked: false }
  const listCalls: URL[] = []

  server.use(
    http.get(`${ORIGIN}/api/upstream-reconciliations/runs`, () =>
      HttpResponse.json({
        items: options.runs ?? RUNS,
        total: (options.runs ?? RUNS).length,
        page: 1,
        page_size: 20,
      }),
    ),
    http.get(
      `${ORIGIN}/api/upstream-reconciliations/runs/:id/items`,
      async ({ request, params }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('page_size') === '1') {
          const severity = url.searchParams.get('severity') ?? ''
          return HttpResponse.json({
            items: [],
            total: options.severityCounts?.[severity] ?? 0,
            page: 1,
            page_size: 1,
          })
        }

        const runId = String(params.id)
        listCalls.push(url)
        options.onList?.(url)
        const gate = options.listGate?.(url, runId)
        if (gate !== undefined) await gate
        if (options.itemsError === true) {
          return HttpResponse.json(
            {
              type: 'about:blank',
              title: 'Internal error',
              detail: 'reconcile store unavailable',
            },
            {
              status: 500,
              headers: { 'Content-Type': 'application/problem+json' },
            },
          )
        }

        const rows = (options.itemsByRun?.[runId] ?? options.items ?? []).map(
          (item) =>
            state.acked ? { ...item, status: 'acknowledged' as const } : item,
        )
        return HttpResponse.json({
          items: rows,
          total: options.listTotal ?? rows.length,
          page: Number(url.searchParams.get('p') ?? '1'),
          page_size: 20,
        })
      },
    ),
    http.post(
      `${ORIGIN}/api/upstream-reconciliations/items/:id/ack`,
      async ({ request }) => {
        options.onAck?.(await request.json())
        state.acked = true
        return HttpResponse.json({
          id: 501,
          status: 'acknowledged',
          note: '',
          updated_at: '2026-10-24T03:00:00Z',
        })
      },
    ),
    http.post(
      `${ORIGIN}/api/upstream-reconciliations/run`,
      async ({ request }) => {
        options.onTrigger?.({
          body: await request.json(),
          idempotencyKey: request.headers.get('Idempotency-Key'),
        })
        if (options.triggerError !== undefined) {
          return HttpResponse.json(
            {
              type: 'about:blank',
              title: options.triggerError.title,
              detail: options.triggerError.detail,
            },
            {
              status: options.triggerError.status,
              headers: { 'Content-Type': 'application/problem+json' },
            },
          )
        }
        return HttpResponse.json(
          options.triggerResult ?? {
            run_ids: [],
            items: 0,
            alerts: 0,
            status: 'succeeded',
          },
        )
      },
    ),
  )

  return { listCalls, state }
}

function renderTab(
  runId: number | null,
  options: {
    canManage?: boolean
    onSelectRun?: (id: number | null) => void
  } = {},
) {
  return renderWithProviders(
    <ResultsTab
      runId={runId}
      onSelectRun={options.onSelectRun ?? (() => undefined)}
      canManage={options.canManage ?? true}
    />,
    ['common', 'admin'],
  )
}

/** 页面在真实应用里持有 `runId`；测试里用受控包装复现运行切换。 */
function ControlledResultsTab({ initialRunId }: { initialRunId: number }) {
  const [runId, setRunId] = useState<number | null>(initialRunId)
  return <ResultsTab runId={runId} onSelectRun={setRunId} canManage />
}

/** 按表头文字取单元格，避免把列位置写死在断言里。 */
function tableCell(row: HTMLElement, columnLabel: string): string | null | undefined {
  const table = row.closest('table')
  if (table === null) throw new Error('row is not inside a table')
  const headers = within(table).getAllByRole('columnheader')
  const index = headers.findIndex(
    (header) => header.textContent === columnLabel,
  )
  if (index < 0) throw new Error(`no column labelled ${columnLabel}`)
  return within(row).getAllByRole('cell')[index]?.textContent
}

describe('reconciliation results tab', () => {
  it('groups severity filters and pages server-side', async () => {
    const { listCalls } = installApi({
      items: [makeItem()],
      listTotal: 45,
      severityCounts: {
        ok: 40,
        warn: 3,
        alert: 1,
        partial_snapshot: 0,
        missing_snapshot: 0,
        missing_local: 0,
        local_expired: 1,
      },
    })
    renderTab(9)
    const user = userEvent.setup()

    await screen.findByTestId('item-row')

    // 每页 20 条的初始请求（无严重度过滤）。
    expect(listCalls[0]?.searchParams.get('p')).toBe('1')
    expect(listCalls[0]?.searchParams.get('page_size')).toBe('20')
    expect(listCalls[0]?.searchParams.has('severity')).toBe(false)

    await user.click(screen.getByRole('button', { name: 'Alert (1)' }))

    await waitFor(() => {
      expect(
        listCalls.some(
          (url) =>
            url.searchParams.get('severity') === 'alert' &&
            url.searchParams.get('p') === '1' &&
            url.searchParams.get('page_size') === '20',
        ),
      ).toBe(true)
    })

    const next = screen.getByRole('button', { name: 'Next' })
    await waitFor(() => expect((next as HTMLButtonElement).disabled).toBe(false))
    await user.click(next)

    await waitFor(() => {
      expect(
        listCalls.some(
          (url) =>
            url.searchParams.get('severity') === 'alert' &&
            url.searchParams.get('p') === '2' &&
            url.searchParams.get('page_size') === '20',
        ),
      ).toBe(true)
    })
    expect(screen.getByText('Page 2 / 3')).toBeTruthy()
  })

  it('returns to the first page when the severity filter changes', async () => {
    const { listCalls } = installApi({
      items: [makeItem()],
      listTotal: 45,
      severityCounts: { warn: 1 },
    })
    renderTab(9)
    const user = userEvent.setup()

    await screen.findByTestId('item-row')
    const next = screen.getByRole('button', { name: 'Next' })
    await waitFor(() => expect((next as HTMLButtonElement).disabled).toBe(false))
    await user.click(next)
    await waitFor(() => {
      expect(
        listCalls.some(
          (url) =>
            !url.searchParams.has('severity') &&
            url.searchParams.get('p') === '2',
        ),
      ).toBe(true)
    })

    await user.click(screen.getByRole('button', { name: 'Warning (1)' }))

    // 新筛选必须从第一页开始，不能继承旧筛选的页码。
    await waitFor(() => {
      expect(
        listCalls.some(
          (url) =>
            url.searchParams.get('severity') === 'warn' &&
            url.searchParams.get('p') === '1',
        ),
      ).toBe(true)
    })
  })

  it('does not carry the previous run rows into a new run', async () => {
    const gate = deferred()
    installApi({
      runs: [RUN_100, RUN_200],
      itemsByRun: {
        '100': [
          makeItem({ id: 101, run_id: 100, upstream_model: 'run-100-model' }),
        ],
        '200': [
          makeItem({ id: 201, run_id: 200, upstream_model: 'run-200-model' }),
        ],
      },
      listGate: (_url, runId) => (runId === '200' ? gate.promise : undefined),
    })
    renderWithProviders(<ControlledResultsTab initialRunId={100} />, [
      'common',
      'admin',
    ])
    const user = userEvent.setup()

    expect(await screen.findByText('run-100-model')).toBeTruthy()
    await screen.findByRole('option', { name: /#200/ })
    await user.selectOptions(
      screen.getByLabelText('Reconciliation run'),
      '200',
    )

    // 新运行的数据还没到：旧运行的行不得继续渲染，也不得被点开。
    await waitFor(() => expect(screen.queryByText('run-100-model')).toBeNull())
    expect(screen.queryByTestId('item-row')).toBeNull()
    expect(
      screen.queryByRole('button', { name: /^Open details for run-100/ }),
    ).toBeNull()

    gate.resolve()

    expect(await screen.findByText('run-200-model')).toBeTruthy()
    expect(screen.queryByText('run-100-model')).toBeNull()
  })

  it('closes the sheet while a same-run query shows placeholder data', async () => {
    const gate = deferred()
    const rows = [makeItem()]
    installApi({
      items: rows,
      listTotal: 45,
      severityCounts: { warn: 1 },
      listGate: (url) =>
        url.searchParams.get('severity') === 'warn' ? gate.promise : undefined,
    })
    renderTab(9)
    const user = userEvent.setup()

    await screen.findByTestId('item-row')
    await user.click(
      screen.getByRole('button', {
        name: 'Open details for deepseek-chat on 2026-10-24',
      }),
    )
    expect(await screen.findByRole('dialog')).toBeTruthy()
    // 先写一段草稿：若抽屉在刷新后自己弹回来，这段说明必然已被重置清空。
    await user.type(screen.getByLabelText('Note'), 'draft before filter')

    // 抽屉打开时背景被设为 aria-hidden；筛选按钮仍在 DOM 中。
    await user.click(
      screen.getByRole('button', { name: 'Warning (1)', hidden: true }),
    )

    // 占位行不属于当前请求：抽屉不能继续展示占位数据里的明细。
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    // 刷新数据里多一行标记：它出现即证明新数据已经落到表格，占位窗口随之结束。
    rows.push(makeItem({ id: 502, upstream_model: 'refreshed-row' }))
    gate.resolve()

    await screen.findByTestId('item-row')
    expect(await screen.findByText('refreshed-row')).toBeTruthy()
    // 刷新结果到达后抽屉必须保持关闭，而不是带着被重置清空的草稿重新打开。
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('keeps the same run’s rows on screen while a new filter loads', async () => {
    const gate = deferred()
    const rows = [makeItem({ upstream_model: 'placeholder-kept' })]
    installApi({
      items: rows,
      severityCounts: { warn: 1 },
      listGate: (url) =>
        url.searchParams.get('severity') === 'warn' ? gate.promise : undefined,
    })
    await renderTab(9)
    const user = userEvent.setup()

    await screen.findByTestId('item-row')
    await user.click(screen.getByRole('button', { name: 'Warning (1)' }))

    // 同一运行内换筛选：新请求完成前沿用上一页数据，行不能消失。
    expect(screen.getByText('placeholder-kept')).toBeTruthy()
    expect(screen.getAllByTestId('item-row').length).toBeGreaterThan(0)

    // 放行后返回一份新数据（多一行标记），用以证明刷新结果真的到了。
    rows.push(makeItem({ id: 502, upstream_model: 'refreshed-row' }))
    gate.resolve()
    expect(await screen.findByText('refreshed-row')).toBeTruthy()
    expect(screen.getByText('placeholder-kept')).toBeTruthy()
  })

  it('opens the sheet with the six comparison rows', async () => {
    installApi({ items: [makeItem()], listTotal: 1 })
    renderTab(9)
    const user = userEvent.setup()

    await screen.findByTestId('item-row')
    await user.click(
      screen.getByRole('button', {
        name: 'Open details for deepseek-chat on 2026-10-24',
      }),
    )

    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByRole('heading', {
        name: 'deepseek-chat · 2026-10-24',
      }),
    ).toBeTruthy()
    expect(within(dialog).getAllByTestId(/^comparison-row-/)).toHaveLength(6)
    const hit = within(dialog).getByTestId('comparison-row-cache_hit')
    const cells = within(hit).getAllByRole('cell')
    expect(cells[1]?.textContent).toBe('100')
    expect(cells[2]?.textContent).toBe('90')
    expect(cells[3]?.textContent).toBe('10')
  })

  it('renders NULL deltas as — and never NaN', async () => {
    installApi({
      items: [
        makeItem({
          snapshot_cache_hit_tokens: null,
          snapshot_cache_miss_tokens: null,
          snapshot_cache_write_tokens: null,
          snapshot_output_tokens: null,
          snapshot_requests: null,
          snapshot_amount_minor: null,
          delta_cache_hit_tokens: null,
          delta_cache_miss_tokens: null,
          delta_output_tokens: null,
          delta_token_bp: null,
          revenue_minor: null,
          upstream_cost_minor: null,
          margin_minor: null,
          margin_bp: null,
          severity: 'partial_snapshot',
        }),
      ],
      listTotal: 1,
    })
    renderTab(9)
    const user = userEvent.setup()

    const row = await screen.findByTestId('item-row')
    // 每个 NULL 维度自己的单元格都必须是长破折号（而不是「某处有一个 —」）。
    expect(tableCell(row, 'Token delta (bp)')).toBe('—')
    expect(tableCell(row, 'Upstream tokens')).toBe('—')
    expect(tableCell(row, 'Revenue')).toBe('—')
    expect(tableCell(row, 'Upstream cost')).toBe('—')
    expect(tableCell(row, 'Margin')).toBe('—')
    // 非 NULL 的本地合计仍按数字渲染，不能被 NULL 规则牵连。
    expect(tableCell(row, 'Local tokens')).toBe('425')
    expect(row.textContent).not.toContain('NaN')

    await user.click(
      screen.getByRole('button', {
        name: 'Open details for deepseek-chat on 2026-10-24',
      }),
    )
    const dialog = await screen.findByRole('dialog')
    const hit = within(dialog).getByTestId('comparison-row-cache_hit')
    const cells = within(hit).getAllByRole('cell')
    expect(cells[2]?.textContent).toBe('—')
    expect(cells[3]?.textContent).toBe('—')
    expect(dialog.textContent).not.toContain('NaN')
  })

  it('renders genuine zero values as 0, not —', async () => {
    installApi({
      items: [
        makeItem({
          snapshot_cache_hit_tokens: 0,
          snapshot_cache_miss_tokens: 0,
          snapshot_output_tokens: 0,
          delta_token_bp: 0,
          revenue_minor: 0,
          upstream_cost_minor: 0,
          margin_minor: 0,
        }),
      ],
      listTotal: 1,
    })
    renderTab(9)

    const row = await screen.findByTestId('item-row')
    expect(tableCell(row, 'Token delta (bp)')).toBe('0')
    expect(tableCell(row, 'Upstream tokens')).toBe('0')
    expect(tableCell(row, 'Revenue')).toBe('¥0.00')
    expect(tableCell(row, 'Upstream cost')).toBe('¥0.00')
    expect(tableCell(row, 'Margin')).toBe('¥0.00')
  })

  it('acks an item and refreshes the list', async () => {
    let ackBody: unknown = null
    const { listCalls } = installApi({
      items: [makeItem()],
      listTotal: 1,
      onAck: (body) => {
        ackBody = body
      },
    })
    renderTab(9)
    const user = userEvent.setup()

    await screen.findByTestId('item-row')
    await user.click(
      screen.getByRole('button', {
        name: 'Open details for deepseek-chat on 2026-10-24',
      }),
    )
    await screen.findByRole('dialog')

    await user.type(screen.getByLabelText('Note'), 'checked with provider')
    await user.click(
      screen.getByRole('button', { name: 'Save acknowledgement' }),
    )

    await waitFor(() => expect(ackBody).not.toBeNull())
    expect(ackBody).toEqual({
      status: 'acknowledged',
      note: 'checked with provider',
    })

    // 确认后按前缀失效：列表重新请求，行上的状态徽标变为已确认。
    await waitFor(() => expect(listCalls.length).toBeGreaterThanOrEqual(2))
    await waitFor(() => {
      const row = screen.getByTestId('item-row')
      expect(within(row).getByText('Acknowledged')).toBeTruthy()
    })
  })

  it('shows an empty state for a run with zero items', async () => {
    installApi({ items: [], listTotal: 0 })
    renderTab(9)

    expect(
      await screen.findByText('This run has no reconciliation items.'),
    ).toBeTruthy()
    expect(screen.queryByTestId('item-row')).toBeNull()
  })

  it('renders the RFC 7807 detail when the items request fails', async () => {
    installApi({ itemsError: true })
    renderTab(9)

    expect(
      await screen.findByText('reconcile store unavailable'),
    ).toBeTruthy()
  })

  it('selects the newest run once the run list loads', async () => {
    installApi()
    const onSelectRun = vi.fn()
    renderTab(null, { onSelectRun })

    await waitFor(() => expect(onSelectRun).toHaveBeenCalledWith(9))
    // 服务端已按 started_at 降序返回：下拉保持该顺序，不重排。
    const labels = screen
      .getAllByRole('option')
      .map((option) => option.textContent)
    expect(labels).toEqual([
      'Select a run',
      '#9 · 2026-10-22 – 2026-10-24 · Succeeded',
      '#8 · 2026-10-19 – 2026-10-21 · Partial',
    ])
  })

  it('renders an unknown severity as a neutral badge with its raw value', async () => {
    installApi({
      items: [
        makeItem({
          severity: 'future_severity' as ReconcileItem['severity'],
        }),
      ],
      listTotal: 1,
    })
    renderTab(9)

    const raw = await screen.findByText('future_severity')
    const badge = raw.closest('[data-testid="status-badge"]')
    expect(badge?.getAttribute('data-variant')).toBe('neutral')
  })

  it('triggers a run for the selected account with a fresh idempotency key', async () => {
    const runs = [RUN_100]
    const calls: TriggerCall[] = []
    const newRun = makeRun(300, '2026-10-05T02:00:00Z')
    installApi({
      runs,
      items: [],
      triggerResult: {
        run_ids: [300],
        items: 4,
        alerts: 1,
        status: 'succeeded',
      },
      onTrigger: (call) => {
        calls.push(call)
        // 第二次点击不重复插入同一条运行：下拉的 key 必须唯一。
        if (!runs.some((run) => run.id === newRun.id)) runs.unshift(newRun)
      },
    })
    await renderWithProviders(<ControlledResultsTab initialRunId={100} />, [
      'common',
      'admin',
    ])
    const user = userEvent.setup()

    await screen.findByRole('option', { name: /#100/ })
    await user.click(screen.getByRole('button', { name: 'Reconcile again' }))

    await waitFor(() => expect(calls).toHaveLength(1))
    // 选中运行的账号随请求体发出；幂等键非空。
    expect(calls[0]?.body).toEqual({ account_id: 1 })
    expect(calls[0]?.idempotencyKey).toBeTruthy()

    // 成功后会重取运行列表并选中最新一条，同时给出响应摘要。
    const select = screen.getByLabelText('Reconciliation run')
    await waitFor(() =>
      expect((select as HTMLSelectElement).value).toBe('300'),
    )
    expect(
      await screen.findByText(
        'Started 1 run(s) · 4 item(s) · 1 alert(s) · Succeeded',
      ),
    ).toBeTruthy()

    // 等待第一次请求彻底收尾（按钮恢复可点），再发起第二次点击。
    const triggerButton = await waitFor(() =>
      screen.getByRole('button', { name: 'Reconcile again' }),
    )
    await user.click(triggerButton)
    await waitFor(() => expect(calls).toHaveLength(2))
    // 连续两次点击是两个意图：键必须不同，否则第二次会被当作重放。
    expect(calls[1]?.idempotencyKey).toBeTruthy()
    expect(calls[1]?.idempotencyKey).not.toBe(calls[0]?.idempotencyKey)
  })
  it('triggers all accounts when no run is selected and explains an empty result', async () => {
    const calls: TriggerCall[] = []
    installApi({
      runs: [],
      triggerResult: { run_ids: [], items: 0, alerts: 0, status: 'succeeded' },
      onTrigger: (call) => {
        calls.push(call)
      },
    })
    renderTab(null)
    const user = userEvent.setup()

    const triggerButton = await screen.findByRole('button', {
      name: 'Reconcile again',
    })
    await waitFor(() =>
      expect((triggerButton as HTMLButtonElement).disabled).toBe(false),
    )
    await user.click(triggerButton)

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]?.body).toEqual({})
    expect(
      await screen.findByText('No enabled upstream account was reconciled.'),
    ).toBeTruthy()
    // 空 run_ids 不是「0 条明细」：不得渲染计数摘要。
    expect(screen.queryByText(/0 item\(s\)/)).toBeNull()
  })

  it('shows the problem detail and keeps the selection when triggering fails', async () => {
    installApi({
      items: [],
      triggerError: {
        status: 422,
        title: 'Unprocessable entity',
        detail: 'no enabled account',
      },
    })
    await renderWithProviders(<ControlledResultsTab initialRunId={9} />, [
      'common',
      'admin',
    ])
    const user = userEvent.setup()

    await screen.findByRole('option', { name: /#8/ })
    await user.click(screen.getByRole('button', { name: 'Reconcile again' }))

    expect(await screen.findByText('no enabled account')).toBeTruthy()
    // 失败不改变当前选中的运行。
    expect(
      (screen.getByLabelText('Reconciliation run') as HTMLSelectElement).value,
    ).toBe('9')
  })

  it('hides the trigger button without settings:manage', async () => {
    installApi({ items: [makeItem()] })
    renderTab(9, { canManage: false })

    await screen.findByTestId('item-row')
    expect(
      screen.queryByRole('button', { name: 'Reconcile again' }),
    ).toBeNull()
  })

  it('renders a failed run’s raw error with a danger alert', async () => {
    installApi({
      runs: [
        makeRun(11, '2026-10-05T02:00:00Z', {
          status: 'failed',
          error: 'insert failed: duplicate key',
          finished_at: '2026-10-05T02:00:10Z',
        }),
      ],
      items: [],
    })
    renderTab(11)

    const message = await screen.findByText('insert failed: duplicate key')
    const alert = message.closest('[role="alert"]')
    expect(alert?.textContent).toContain('insert failed: duplicate key')
    expect(alert?.className).toContain('text-danger')
    expect(
      within(await screen.findByTestId('run-status')).getByText('Failed'),
    ).toBeTruthy()
  })

  it('renders a partial run’s reason with a warning alert', async () => {
    installApi({
      runs: [
        makeRun(11, '2026-10-05T02:00:00Z', {
          status: 'partial',
          error: 'log_stream_lag',
          finished_at: '2026-10-05T02:00:10Z',
        }),
      ],
      items: [],
    })
    renderTab(11)

    const message = await screen.findByText('log_stream_lag')
    const alert = message.closest('[role="alert"]')
    expect(alert?.textContent).toContain('log_stream_lag')
    expect(alert?.className).toContain('text-warning')
    expect(
      within(await screen.findByTestId('run-status')).getByText('Partial'),
    ).toBeTruthy()
  })

  it('discloses a running run and labels options with their statuses', async () => {
    installApi({
      runs: [
        makeRun(12, '2026-10-06T02:00:00Z', {
          status: 'running',
          finished_at: null,
        }),
        makeRun(9, '2026-10-04T02:00:00Z'),
      ],
      items: [],
    })
    renderTab(12)

    expect(
      await screen.findByText(
        'This run is still running; the item list is incomplete and will fill in.',
      ),
    ).toBeTruthy()
    const status = await screen.findByTestId('run-status')
    expect(within(status).getByText('Running')).toBeTruthy()
    // finished_at 为空时给出运行中的说明，而不是破折号。
    expect(within(status).getByText('Still running')).toBeTruthy()

    expect(
      screen.getAllByRole('option').map((option) => option.textContent),
    ).toEqual([
      'Select a run',
      '#12 · 2026-10-01 – 2026-10-02 · Running',
      '#9 · 2026-10-01 – 2026-10-02 · Succeeded',
    ])
  })

  it('renders no error alert for a succeeded run with an empty error', async () => {
    installApi({ runs: [makeRun(9, '2026-10-04T02:00:00Z')], items: [makeItem()] })
    renderTab(9)

    await screen.findByTestId('item-row')
    expect(screen.queryByRole('alert')).toBeNull()

    const finished = new Intl.DateTimeFormat('en', {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(Date.parse('2026-10-04T02:00:00Z'))
    const status = screen.getByTestId('run-status')
    expect(within(status).getByText('Succeeded')).toBeTruthy()
    expect(within(status).getByText(`Finished ${finished}`)).toBeTruthy()
  })
})
