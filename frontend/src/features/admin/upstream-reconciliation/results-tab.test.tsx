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

import type { ReconcileItem, ReconcileRun } from './api'
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

interface ApiOptions {
  items?: ReconcileItem[]
  listTotal?: number
  severityCounts?: Partial<Record<string, number>>
  itemsError?: boolean
  runs?: ReconcileRun[]
  onList?: (url: URL) => void
  onAck?: (body: unknown) => void
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
      ({ request }) => {
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

        listCalls.push(url)
        options.onList?.(url)
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

        const rows = (options.items ?? []).map((item) =>
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
    // 差异率与上游 token、金额列都是 NULL，必须显示长破折号。
    expect(within(row).getAllByText('—').length).toBeGreaterThan(0)
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
      '#9 · 2026-10-22 – 2026-10-24',
      '#8 · 2026-10-19 – 2026-10-21',
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
})
