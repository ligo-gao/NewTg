import { useState } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { I18nProvider, loadDictionary } from '@/app/providers/i18n'

import type { WalletTransactionsSearch } from './search'
import { WalletPage } from './wallet-page'

// 只验证页面级 Tab 的挂载与 URL 回写；充值/订阅/流水各自的查询契约由
// 各自测试覆盖，这里用桩组件把三者隔离。
vi.mock('@/features/account/topup/topup-page', () => ({
  TopupPage: () => <div>topup stub</div>,
}))
vi.mock('@/features/account/subscription/subscription-section', () => ({
  SubscriptionSection: () => <div>subscriptions stub</div>,
}))
vi.mock('./transactions-section', () => ({
  TransactionsSection: () => <div>transactions stub</div>,
}))

function Harness({ initial }: { initial: WalletTransactionsSearch }) {
  const [search, setSearch] = useState(initial)
  return <WalletPage search={search} onSearchChange={setSearch} />
}

async function renderPage(initial: WalletTransactionsSearch) {
  await loadDictionary('en', 'account')
  return render(
    <I18nProvider modules={['account']}>
      <Harness initial={initial} />
    </I18nProvider>,
  )
}

describe('WalletPage tabs (R-M3-01)', () => {
  it('defaults to the top-up tab', async () => {
    await renderPage({ p: 1, page_size: 20 })
    expect(
      screen.getByRole('tab', { name: 'Top-up', selected: true }),
    ).toBeTruthy()
    expect(screen.getByText('topup stub')).toBeTruthy()
    expect(screen.getByText('subscriptions stub')).toBeTruthy()
    expect(screen.queryByText('transactions stub')).toBeNull()
  })

  it('opens the transactions tab from ?tab=transactions', async () => {
    await renderPage({ tab: 'transactions', p: 1, page_size: 20 })
    expect(
      screen.getByRole('tab', { name: 'Transactions', selected: true }),
    ).toBeTruthy()
    expect(screen.getByText('transactions stub')).toBeTruthy()
    expect(screen.queryByText('topup stub')).toBeNull()
  })

  it('switches tabs and keeps the URL shape canonical', async () => {
    await renderPage({ p: 3, page_size: 50 })
    const user = userEvent.setup()

    await user.click(screen.getByRole('tab', { name: 'Transactions' }))
    expect(screen.getByText('transactions stub')).toBeTruthy()
    expect(
      screen.getByRole('tab', { name: 'Transactions', selected: true }),
    ).toBeTruthy()

    await user.click(screen.getByRole('tab', { name: 'Top-up' }))
    expect(screen.getByText('topup stub')).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Top-up', selected: true })).toBeTruthy()
  })
})
