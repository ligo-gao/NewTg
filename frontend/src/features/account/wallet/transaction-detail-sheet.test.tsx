import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { TransactionDetailSheet } from './transaction-detail-sheet'
import type { WalletTransactionDetail } from './types'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const BASE: WalletTransactionDetail = {
  id: 7,
  type: 'consume_pre',
  amount: -12000,
  balance_after: 988000,
  status: 'settled',
  reference_type: 'usage_log',
  reference_id: '9f3a8d2e1b2c4d5e8f90112233445566',
  created_at: '2026-10-20T10:00:00Z',
  note: null,
  usage: {
    log_id: 88901,
    request_id: '9f3a8d2e1b2c4d5e8f90112233445566',
    model: 'claude-sonnet-4.5',
    prompt_tokens: 1204,
    completion_tokens: 388,
    cost: 9240,
    pre_deducted: 12000,
    refunded: 2760,
  },
  order: null,
  redemption: null,
  adjustment: null,
  segments: { held: 12000, charged: 9240, refunded: 2760 },
  corrects_transaction_id: null,
}

function install(detail: WalletTransactionDetail) {
  server.use(
    http.get(`${ORIGIN}/api/wallet/transactions/7`, () => HttpResponse.json(detail)),
  )
}

async function openSheet(props: Partial<Parameters<typeof TransactionDetailSheet>[0]> = {}) {
  const onOpenChange = vi.fn()
  const rendered = await renderWithProviders(
    <TransactionDetailSheet
      transactionId={7}
      onOpenChange={onOpenChange}
      {...props}
    />,
    ['common', 'account'],
  )
  return { ...rendered, onOpenChange }
}

describe('TransactionDetailSheet (F-03-03)', () => {
  it('renders usage context and the three-segment breakdown', async () => {
    install(BASE)
    await openSheet()

    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('claude-sonnet-4.5')).toBeTruthy()
    expect(within(dialog).getByText('1,204 in / 388 out')).toBeTruthy()
    expect(
      within(dialog).getByText('Held 12,000 → used 9,240 → refunded 2,760'),
    ).toBeTruthy()
    expect(within(dialog).getByText('−12,000')).toBeTruthy()
  })

  it('never renders a machine fingerprint note', async () => {
    install({
      ...BASE,
      type: 'admin_adjustment',
      reference_type: 'admin_op',
      reference_id: 'idem-key',
      note: null,
      usage: null,
      segments: null,
      adjustment: { mode: 'add' },
    })
    await openSheet()
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('Adjustment mode: Add')).toBeTruthy()
    expect(dialog.textContent).not.toContain('idem:')
    expect(dialog.textContent).not.toContain('admin adjustment:')
  })

  it('renders order, redemption and reversal contexts', async () => {
    install({
      ...BASE,
      id: 7,
      type: 'reversal',
      amount: 2000,
      status: 'confirmed',
      reference_type: 'order',
      reference_id: '42',
      usage: null,
      segments: null,
      order: {
        id: 42,
        status: 'succeeded',
        amount: 5000,
        currency: 'CNY',
        provider: 'stripe',
        biz_kind: 'subscribe',
      },
      redemption: { id: 3, batch_name: 'campaign-a', credits: 1000 },
      corrects_transaction_id: 9,
    })
    const onOpenCorrects = vi.fn()
    await openSheet({ onOpenCorrects })

    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('#42')).toBeTruthy()
    expect(within(dialog).getByText('View order')).toBeTruthy()
    expect(within(dialog).getByText('campaign-a')).toBeTruthy()
    expect(
      within(dialog).getByText(
        'This is a reversal record that corrects an earlier transaction.',
      ),
    ).toBeTruthy()

    const user = userEvent.setup()
    await user.click(
      within(dialog).getByRole('button', {
        name: 'View corrected transaction #9',
      }),
    )
    expect(onOpenCorrects).toHaveBeenCalledWith(9)
  })

  it('renders the not-found empty state for a 404', async () => {
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions/7`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Not Found', status: 404 },
          { status: 404 },
        ),
      ),
    )
    await openSheet()
    expect(await screen.findByText('Transaction not found')).toBeTruthy()
  })

  it('copies the transaction id', async () => {
    install(BASE)
    // jsdom 自带 navigator.clipboard；直接替换其方法，组件拿到的就是同一对象。
    const writeText = vi
      .spyOn(navigator.clipboard, 'writeText')
      .mockResolvedValue(undefined)
    await openSheet()
    const dialog = await screen.findByRole('dialog')
    // 等详情落地后再点：footer 的复制按钮在数据到达前是 disabled。
    await within(dialog).findByText('claude-sonnet-4.5')
    const user = userEvent.setup()
    const copyButton = await within(dialog).findByRole('button', {
      name: 'Copy transaction ID',
    })
    await user.click(copyButton)
    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith('7')
    })
  })
})
