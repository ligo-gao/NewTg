import { useState } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { I18nProvider, loadDictionary } from '@/app/providers/i18n'

import type { WalletTransactionsSearch } from './search'
import { WalletPage } from './wallet-page'

// 只验证页面级 Tab 的挂载与 URL 回写；概览/充值/订阅/流水各自的查询契约由
// 各自测试覆盖，这里用桩组件把四者隔离。
vi.mock('@/features/account/topup/topup-page', () => ({
  TopupPage: ({ initialSelection }: { initialSelection?: unknown }) => (
    <div>
      <div>topup stub</div>
      <div data-testid="topup-selection">
        {initialSelection === undefined
          ? 'none'
          : JSON.stringify(initialSelection)}
      </div>
    </div>
  ),
}))
vi.mock('@/features/account/subscription/subscription-section', () => ({
  SubscriptionSection: () => <div>subscriptions stub</div>,
}))
vi.mock('./transactions-section', () => ({
  TransactionsSection: () => <div>transactions stub</div>,
}))
vi.mock('./wallet-overview', () => ({
  WalletOverview: ({
    onSelectTab,
    onTopup,
  }: {
    onSelectTab: (tab: 'topup' | 'subscription' | 'transactions') => void
    onTopup: (selection?: { cents: number | null; custom: boolean }) => void
  }) => (
    <div>
      <div>overview stub</div>
      <button type="button" onClick={() => onSelectTab('transactions')}>
        overview to transactions
      </button>
      <button
        type="button"
        onClick={() => onTopup({ cents: 20000, custom: false })}
      >
        overview topup preset
      </button>
      <button type="button" onClick={() => onTopup({ cents: null, custom: true })}>
        overview topup custom
      </button>
    </div>
  ),
}))

function Harness({ initial }: { initial: WalletTransactionsSearch }) {
  const [search, setSearch] = useState(initial)
  return <WalletPage search={search} onSearchChange={setSearch} />
}

async function renderPage(initial: WalletTransactionsSearch) {
  await loadDictionary('en', 'account')
  return render(
    <I18nProvider modules={['common', 'account']}>
      <Harness initial={initial} />
    </I18nProvider>,
  )
}

describe('WalletPage tabs (R-M3-01)', () => {
  it('defaults to the overview tab', async () => {
    await renderPage({ p: 1, page_size: 20 })
    expect(
      screen.getByRole('tab', { name: 'Overview', selected: true }),
    ).toBeTruthy()
    expect(screen.getByText('overview stub')).toBeTruthy()
    expect(screen.queryByText('topup stub')).toBeNull()
    expect(screen.queryByText('subscriptions stub')).toBeNull()
    expect(screen.queryByText('transactions stub')).toBeNull()
  })

  it('opens the transactions tab from ?tab=transactions', async () => {
    await renderPage({ tab: 'transactions', p: 1, page_size: 20 })
    expect(
      screen.getByRole('tab', { name: 'Transactions', selected: true }),
    ).toBeTruthy()
    expect(screen.getByText('transactions stub')).toBeTruthy()
    expect(screen.queryByText('overview stub')).toBeNull()
  })

  it('switches between all four tabs', async () => {
    await renderPage({ p: 3, page_size: 50 })
    const user = userEvent.setup()

    await user.click(screen.getByRole('tab', { name: 'Top-up' }))
    expect(screen.getByText('topup stub')).toBeTruthy()
    expect(screen.queryByText('subscriptions stub')).toBeNull()

    await user.click(screen.getByRole('tab', { name: 'Subscription' }))
    expect(screen.getByText('subscriptions stub')).toBeTruthy()
    expect(screen.queryByText('topup stub')).toBeNull()

    await user.click(screen.getByRole('tab', { name: 'Transactions' }))
    expect(screen.getByText('transactions stub')).toBeTruthy()

    await user.click(screen.getByRole('tab', { name: 'Overview' }))
    expect(screen.getByText('overview stub')).toBeTruthy()
  })

  it('opens the transactions tab from the overview callback', async () => {
    await renderPage({ p: 1, page_size: 20 })
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'overview to transactions' }),
    )

    expect(screen.getByText('transactions stub')).toBeTruthy()
    expect(
      screen.getByRole('tab', { name: 'Transactions', selected: true }),
    ).toBeTruthy()
  })

  it('opens top-up with the default seeds from the page header', async () => {
    await renderPage({ p: 1, page_size: 20 })
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Top up' }))

    expect(screen.getByText('topup stub')).toBeTruthy()
    expect(screen.getByTestId('topup-selection').textContent).toBe('none')
  })

  it('opens the top-up tab with the quick amount picked in the overview', async () => {
    await renderPage({ p: 1, page_size: 20 })
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'overview topup preset' }))

    expect(
      screen.getByRole('tab', { name: 'Top-up', selected: true }),
    ).toBeTruthy()
    expect(screen.getByTestId('topup-selection').textContent).toBe(
      '{"cents":20000,"custom":false}',
    )
  })

  it('opens the top-up tab in custom mode from the overview', async () => {
    await renderPage({ p: 1, page_size: 20 })
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'overview topup custom' }))

    expect(
      screen.getByRole('tab', { name: 'Top-up', selected: true }),
    ).toBeTruthy()
    expect(screen.getByTestId('topup-selection').textContent).toBe(
      '{"cents":null,"custom":true}',
    )
  })

  it('wires the selected tab to the mounted panel', async () => {
    await renderPage({ p: 1, page_size: 20 })

    const overview = screen.getByRole('tab', { name: 'Overview' })
    const topup = screen.getByRole('tab', { name: 'Top-up' })
    const panel = screen.getByRole('tabpanel')

    expect(overview.id).toBe('wallet-tab-overview')
    expect(overview.getAttribute('aria-controls')).toBe('wallet-panel-overview')
    expect(overview.getAttribute('tabindex')).toBe('0')
    // The inactive panel is not mounted, so its tab must not point at it.
    expect(topup.getAttribute('aria-controls')).toBeNull()
    expect(topup.getAttribute('tabindex')).toBe('-1')
    expect(panel.id).toBe('wallet-panel-overview')
    expect(panel.getAttribute('aria-labelledby')).toBe('wallet-tab-overview')
  })

  it('activates the next tab with ArrowRight', async () => {
    await renderPage({ p: 1, page_size: 20 })
    const user = userEvent.setup()

    const overview = screen.getByRole('tab', { name: 'Overview' })
    overview.focus()
    expect(document.activeElement).toBe(overview)

    await user.keyboard('{ArrowRight}')

    const topup = screen.getByRole('tab', { name: 'Top-up', selected: true })
    expect(topup).toBeTruthy()
    expect(document.activeElement).toBe(topup)
    expect(screen.getByText('topup stub')).toBeTruthy()
  })
})
