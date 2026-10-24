import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
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

import type { ReconcileItem } from './api'
import { ItemSheet } from './item-sheet'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

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

interface AckCall {
  body: unknown
  idempotencyKey: string | null
}

function installAckApi(handler: (call: AckCall) => void = () => undefined) {
  server.use(
    http.post(
      `${ORIGIN}/api/upstream-reconciliations/items/:id/ack`,
      async ({ request }) => {
        handler({
          body: await request.json(),
          idempotencyKey: request.headers.get('Idempotency-Key'),
        })
        return HttpResponse.json({
          id: 501,
          status: 'acknowledged',
          note: '',
          updated_at: '2026-10-24T03:00:00Z',
        })
      },
    ),
  )
}

function renderSheet(
  item: ReconcileItem | null,
  options: { canManage?: boolean; onAckSuccess?: () => void } = {},
) {
  const onOpenChange = vi.fn()
  return renderWithProviders(
    <ItemSheet
      item={item}
      onOpenChange={onOpenChange}
      canManage={options.canManage ?? true}
      onAckSuccess={options.onAckSuccess}
    />,
    ['common', 'admin'],
  )
}

function comparisonCells(rowKey: string): HTMLElement[] {
  const row = screen.getByTestId(`comparison-row-${rowKey}`)
  return within(row)
    .getAllByRole('cell')
    .map((cell) => cell as HTMLElement)
}

/** 同一条明细以新状态重新传入（确认成功后列表被失效重取）。 */
function RefreshHarness() {
  const [status, setStatus] = useState<ReconcileItem['status']>('open')
  return (
    <>
      <button type="button" onClick={() => setStatus('acknowledged')}>
        refresh item
      </button>
      <ItemSheet
        item={makeItem({ status })}
        onOpenChange={() => undefined}
        canManage
      />
    </>
  )
}

/** 抽屉常驻挂载、内容随 `item` 打开：复现「关闭态收到明细」的生产路径。 */
function SeedHarness() {
  const [item, setItem] = useState<ReconcileItem | null>(null)
  return (
    <>
      <button
        type="button"
        onClick={() => setItem(makeItem({ status: 'resolved' }))}
      >
        open item
      </button>
      <ItemSheet item={item} onOpenChange={() => undefined} canManage />
    </>
  )
}

describe('reconciliation item sheet', () => {
  it('renders the six comparison rows with API values', async () => {
    renderSheet(makeItem())
    const dialog = await screen.findByRole('dialog')

    expect(within(dialog).getByRole('heading', { name: 'deepseek-chat · 2026-10-24' })).toBeTruthy()
    expect(within(dialog).getByText('Local')).toBeTruthy()
    expect(within(dialog).getByText('Upstream')).toBeTruthy()
    expect(within(dialog).getByText('Delta')).toBeTruthy()
    expect(within(dialog).getAllByTestId(/^comparison-row-/)).toHaveLength(6)

    const hit = comparisonCells('cache_hit')
    expect(hit[0]?.textContent).toBe('Cache hit tokens')
    expect(hit[1]?.textContent).toBe('100')
    expect(hit[2]?.textContent).toBe('90')
    expect(hit[3]?.textContent).toBe('10')

    const requests = comparisonCells('requests')
    expect(requests[1]?.textContent).toBe('5')
    expect(requests[2]?.textContent).toBe('4')
    expect(requests[3]?.textContent).toBe('1')

    // 本地没有缓存写入与账单金额口径：只显示上游值，其余为长破折号。
    const amount = comparisonCells('amount')
    expect(amount[1]?.textContent).toBe('—')
    expect(amount[2]?.textContent).toBe('¥12.34')
    expect(amount[3]?.textContent).toBe('—')
  })

  it('renders NULL deltas as — and never NaN', async () => {
    renderSheet(
      makeItem({
        local_cache_hit_tokens: 100,
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
      }),
    )
    const dialog = await screen.findByRole('dialog')

    const hit = comparisonCells('cache_hit')
    expect(hit[1]?.textContent).toBe('100')
    expect(hit[2]?.textContent).toBe('—')
    expect(hit[3]?.textContent).toBe('—')

    const miss = comparisonCells('cache_miss')
    expect(miss[3]?.textContent).toBe('—')

    // 公式块同样不为 NULL 造 0。
    expect(dialog.textContent).toContain('= — bp')
    expect(dialog.textContent).not.toContain('NaN')
  })

  it('always sends note, including the empty string, in the ack body', async () => {
    const calls: AckCall[] = []
    installAckApi((call) => {
      calls.push(call)
    })
    const onAckSuccess = vi.fn()
    renderSheet(makeItem(), { onAckSuccess })
    const user = userEvent.setup()

    await screen.findByRole('dialog')
    await user.click(
      screen.getByRole('button', { name: 'Save acknowledgement' }),
    )

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]?.body).toEqual({ status: 'acknowledged', note: '' })
    expect(calls[0]?.idempotencyKey).toBeTruthy()
    await waitFor(() => expect(onAckSuccess).toHaveBeenCalledTimes(1))
  })

  it('keeps a resolved item resolved when the status is untouched', async () => {
    const calls: AckCall[] = []
    installAckApi((call) => {
      calls.push(call)
    })
    renderSheet(makeItem({ status: 'resolved' }))
    const user = userEvent.setup()

    await screen.findByRole('dialog')
    expect(
      (screen.getByLabelText('New status') as HTMLSelectElement).value,
    ).toBe('resolved')

    await user.click(
      screen.getByRole('button', { name: 'Save acknowledgement' }),
    )

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]?.body).toEqual({ status: 'resolved', note: '' })
  })

  it('seeds an open item as acknowledged', async () => {
    renderSheet(makeItem({ status: 'open' }))

    await screen.findByRole('dialog')
    expect(
      (screen.getByLabelText('New status') as HTMLSelectElement).value,
    ).toBe('acknowledged')
  })

  it('seeds the status when a closed sheet receives its item', async () => {
    await renderWithProviders(<SeedHarness />, ['common', 'admin'])

    // 抽屉打开时背景被设为 aria-hidden，按钮不在可访问树里，只能用文本查询。
    fireEvent.click(screen.getByText('open item'))

    expect(
      (screen.getByLabelText('New status') as HTMLSelectElement).value,
    ).toBe('resolved')
  })

  it('submits the selected status and the typed note', async () => {
    const calls: AckCall[] = []
    installAckApi((call) => {
      calls.push(call)
    })
    renderSheet(makeItem())
    const user = userEvent.setup()

    await screen.findByRole('dialog')
    await user.selectOptions(
      screen.getByLabelText('New status'),
      'resolved',
    )
    await user.type(screen.getByLabelText('Note'), 'checked with provider')
    await user.click(
      screen.getByRole('button', { name: 'Save acknowledgement' }),
    )

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]?.body).toEqual({
      status: 'resolved',
      note: 'checked with provider',
    })
  })

  it('keeps the draft and the success alert when the same item refreshes after an ack', async () => {
    installAckApi()
    await renderWithProviders(<RefreshHarness />, ['common', 'admin'])
    const user = userEvent.setup()

    await screen.findByRole('dialog')
    await user.type(screen.getByLabelText('Note'), 'checked with provider')
    await user.click(
      screen.getByRole('button', { name: 'Save acknowledgement' }),
    )
    await screen.findByText('Acknowledgement saved.')

    // 确认成功后列表被失效重取，同一条明细以新状态再次传入。这只是刷新，
    // 不是换了一条明细：刚输入的说明与「已保存」提示必须留在抽屉里。
    // 抽屉打开时背景是 aria-hidden，按钮不在可访问树里，只能用文本查询。
    fireEvent.click(screen.getByText('refresh item'))

    expect(
      (screen.getByLabelText('Note') as HTMLTextAreaElement).value,
    ).toBe('checked with provider')
    expect(screen.getByText('Acknowledgement saved.')).toBeTruthy()
  })

  it('disables the ack controls without settings:manage', async () => {
    installAckApi()
    renderSheet(makeItem(), { canManage: false })

    await screen.findByRole('dialog')
    expect(
      screen.getByText(
        'Read-only: your account lacks the settings:manage permission.',
      ),
    ).toBeTruthy()
    expect(
      (
        screen.getByRole('button', {
          name: 'Save acknowledgement',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true)
    expect(
      (screen.getByLabelText('Note') as HTMLTextAreaElement).disabled,
    ).toBe(true)
  })
})
