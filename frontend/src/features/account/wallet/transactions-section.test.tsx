import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { WalletTransactionsSearch } from './search'
import { TransactionsSection } from './transactions-section'
import type { WalletTransaction, WalletTransactionPage } from './types'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.useRealTimers()
})
afterAll(() => server.close())

function transaction(overrides: Partial<WalletTransaction>): WalletTransaction {
  return {
    id: 1,
    type: 'topup',
    amount: 1000,
    balance_after: 1000,
    status: 'confirmed',
    reference_type: 'order',
    reference_id: '1',
    created_at: '2026-10-20T10:00:00Z',
    ...overrides,
  }
}

function page(items: WalletTransaction[], total = items.length, pageNo = 1, size = 20): WalletTransactionPage {
  return { items, total, page: pageNo, page_size: size }
}

const DEFAULT_SEARCH: WalletTransactionsSearch = { tab: 'transactions', p: 1, page_size: 20 }

function Harness({
  initial = DEFAULT_SEARCH,
  onChange,
}: {
  initial?: WalletTransactionsSearch
  onChange?: (next: WalletTransactionsSearch) => void
}) {
  const [search, setSearch] = useState(initial)
  return (
    <TransactionsSection
      search={search}
      onSearchChange={(next) => {
        onChange?.(next)
        setSearch(next)
      }}
    />
  )
}

describe('TransactionsSection list (F-03-02)', () => {
  it('renders the seven columns, tags, badges and signed amounts', async () => {
    const items = [
      transaction({ id: 2, type: 'topup', amount: 1000, balance_after: 2000 }),
      transaction({
        id: 1,
        type: 'consume_pre',
        amount: -300,
        balance_after: 1700,
        status: 'hold',
        reference_type: 'usage_log',
        reference_id: '9f3a8d2e1b2c4d5e8f90112233445566',
      }),
    ]
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () =>
        HttpResponse.json(page(items, 2)),
      ),
    )
    await renderWithProviders(<Harness />, ['common', 'account'])

    // 金额方向独立于颜色：+ 收入、− (U+2212) 支出。
    expect(await screen.findByText('+1,000')).toBeTruthy()
    expect(screen.getByText('−300')).toBeTruthy()

    for (const header of [
      'Time',
      'Type',
      'Direction',
      'Amount',
      'Balance after',
      'Status',
      'Actions',
    ]) {
      expect(screen.getByRole('columnheader', { name: header })).toBeTruthy()
    }

    const tags = screen.getAllByTestId('tag').map((tag) => tag.getAttribute('data-variant'))
    expect(tags).toEqual(['success', 'warning'])
    const badges = screen.getAllByTestId('status-badge')
    expect(badges).toHaveLength(2)
    // hold 行带脉冲（reduced-motion 时由 motion-reduce 类静止）。
    expect(badges[1]?.className).toContain('animate-pulse')

    // 11 类 + 全部；5 状态 + 全部。
    const typeSelect = screen.getByLabelText('Type') as HTMLSelectElement
    expect(within(typeSelect).getAllByRole('option')).toHaveLength(12)
    const statusSelect = screen.getByLabelText('Status') as HTMLSelectElement
    expect(within(statusSelect).getAllByRole('option')).toHaveLength(6)
  })

  it('writes select filters back to the URL search and the request', async () => {
    const onChange = vi.fn()
    const seen: string[] = []
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, ({ request }) => {
        seen.push(new URL(request.url).search)
        return HttpResponse.json(page([]))
      }),
    )
    await renderWithProviders(<Harness onChange={onChange} />, ['common', 'account'])
    const user = userEvent.setup()

    await screen.findByText('No transactions yet')
    await user.selectOptions(screen.getByLabelText('Type'), 'consume_pre')
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ type: 'consume_pre', p: 1 }),
    )
    await waitFor(() => {
      expect(seen.some((search) => search.includes('type=consume_pre'))).toBe(true)
    })

    await user.selectOptions(screen.getByLabelText('Direction'), 'out')
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ type: 'consume_pre', dir: 'out', p: 1 }),
    )
  })

  it('commits amount on blur and reference on Enter', async () => {
    const onChange = vi.fn()
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => HttpResponse.json(page([]))),
    )
    await renderWithProviders(<Harness onChange={onChange} />, ['common', 'account'])

    const min = screen.getByLabelText('Min amount')
    fireEvent.change(min, { target: { value: '100' } })
    fireEvent.blur(min)
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ min: 100, p: 1 }),
    )

    const ref = screen.getByLabelText('Reference')
    fireEvent.change(ref, { target: { value: '424242' } })
    fireEvent.keyDown(ref, { key: 'Enter' })
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ ref: '424242', p: 1 }),
    )
  })

  it('blocks the request for a >90 day range and shows the inline error', async () => {
    let requests = 0
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => {
        requests += 1
        return HttpResponse.json(page([]))
      }),
    )
    await renderWithProviders(
      <TransactionsSection
        search={{
          ...DEFAULT_SEARCH,
          from: '2026-01-01T00:00',
          to: '2026-10-01T00:00',
        }}
        onSearchChange={() => undefined}
      />,
      ['common', 'account'],
    )
    expect(
      await screen.findByText('Time range cannot exceed 90 days'),
    ).toBeTruthy()
    expect(requests).toBe(0)
  })

  it('paginates with p/page_size and disables beyond the last page', async () => {
    const onChange = vi.fn()
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, ({ request }) => {
        const p = Number(new URL(request.url).searchParams.get('p') ?? '1')
        return HttpResponse.json(page([transaction({ id: p })], 45, p))
      }),
    )
    await renderWithProviders(<Harness onChange={onChange} />, ['common', 'account'])

    expect(await screen.findByText('Page 1 / 3')).toBeTruthy()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Next page' }))
    expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ p: 2 }))
    expect(await screen.findByText('Page 2 / 3')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Next page' }))
    expect(await screen.findByText('Page 3 / 3')).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Next page' }).hasAttribute('disabled'),
    ).toBe(true)

    await user.selectOptions(screen.getByLabelText('Per page'), '50')
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ page_size: 50, p: 1 }),
    )
  })

  it('distinguishes the empty and filtered-empty states', async () => {
    const onChange = vi.fn()
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => HttpResponse.json(page([]))),
    )
    const first = await renderWithProviders(<Harness onChange={onChange} />, [
      'common',
      'account',
    ])
    expect(await screen.findByText('No transactions yet')).toBeTruthy()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Go to top-up' }))
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ tab: undefined }),
    )
    first.unmount()

    await renderWithProviders(<Harness initial={{ ...DEFAULT_SEARCH, type: 'bonus' }} onChange={onChange} />, [
      'common',
      'account',
    ])
    expect(
      await screen.findByText('No transactions match your filters'),
    ).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(onChange).toHaveBeenLastCalledWith({
      tab: 'transactions',
      p: 1,
      page_size: 20,
    })
  })

  it('shows an inline error with retry', async () => {
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'boom', status: 500 },
            { status: 500 },
          )
        }
        return HttpResponse.json(page([transaction({})]))
      }),
    )
    await renderWithProviders(<Harness />, ['common', 'account'])
    expect(await screen.findByText('Could not load transactions')).toBeTruthy()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('+1,000')).toBeTruthy()
  })

  it('renders the 429 banner and disables controls', async () => {
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Too many requests', status: 429 },
          { status: 429, headers: { 'Retry-After': '5' } },
        ),
      ),
    )
    await renderWithProviders(<Harness />, ['common', 'account'])
    expect(
      await screen.findByText('Too many requests, retry in 5s'),
    ).toBeTruthy()
    expect(
      (screen.getByLabelText('Type') as HTMLSelectElement).disabled,
    ).toBe(true)
    expect(
      screen.getByRole('button', { name: 'Refresh' }).hasAttribute('disabled'),
    ).toBe(true)
  })

  it('shows the offline notice when navigator reports offline', async () => {
    Object.defineProperty(window.navigator, 'onLine', {
      configurable: true,
      value: false,
    })
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => HttpResponse.json(page([]))),
    )
    try {
      await renderWithProviders(<Harness />, ['common', 'account'])
      expect(
        await screen.findByText(
          'You are offline. Showing the last loaded page.',
        ),
      ).toBeTruthy()
    } finally {
      Object.defineProperty(window.navigator, 'onLine', {
        configurable: true,
        value: true,
      })
    }
  })
})

describe('TransactionsSection hold polling (R-M3-14)', () => {
  it('does not poll when no row is on hold', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let requests = 0
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => {
        requests += 1
        return HttpResponse.json(page([transaction({ status: 'confirmed' })]))
      }),
    )
    const rendered = await renderWithProviders(<Harness />, ['common', 'account'])
    await screen.findByText('+1,000')
    expect(requests).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(requests).toBe(1)
    rendered.unmount()
  })

  it('polls every 30s while a hold row exists and stops after two failures', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let requests = 0
    let failing = false
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, () => {
        requests += 1
        if (failing) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'boom', status: 500 },
            { status: 500 },
          )
        }
        return HttpResponse.json(
          page([
            transaction({
              type: 'consume_pre',
              status: 'hold',
              amount: -300,
              reference_type: 'usage_log',
              reference_id: '9f3a8d2e1b2c4d5e8f90112233445566',
            }),
          ]),
        )
      }),
    )
    const rendered = await renderWithProviders(<Harness />, ['common', 'account'])
    await waitFor(() => {
      expect(screen.getByTestId('status-badge').textContent).toBe('In progress')
    })
    expect(requests).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    await waitFor(() => {
      expect(requests).toBe(2)
    })

    failing = true
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    await waitFor(() => {
      expect(requests).toBe(3)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    await waitFor(() => {
      expect(requests).toBe(4)
    })
    // 让第二次失败的 React 状态（fetchFailureCount）提交，refetchInterval
    // 才会被清除；再跨过一个完整周期验证轮询确实停止。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000)
    })
    expect(requests).toBe(4)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(requests).toBe(4)
    rendered.unmount()
  })
})
