import { SubscriptionSection } from '@/features/account/subscription/subscription-section'
import { TopupPage } from '@/features/account/topup/topup-page'
import { useI18n } from '@/shared/lib/i18n-context'

import type { WalletTransactionsSearch } from './search'
import { TransactionsSection } from './transactions-section'

export interface WalletPageProps {
  search: WalletTransactionsSearch
  onSearchChange: (next: WalletTransactionsSearch) => void
}

/**
 * `/wallet` 的页面级 Tab（R-M3-01）：默认 `topup`，`?tab=transactions` 进入
 * 流水。设计原文把流水放在默认 Tab；控制端裁决先保持现有充值 UX，登记为
 * 「默认页偏离设计」。
 *
 * 两个 Tab 的内容互斥挂载：切到流水时不会触发充值/订阅的额外查询，
 * 反之亦然。充值 Tab 保留原有 TopupPage + SubscriptionSection 堆叠。
 */
export function WalletPage({ search, onSearchChange }: WalletPageProps) {
  const { t } = useI18n()
  const tab = search.tab === 'transactions' ? 'transactions' : 'topup'

  return (
    <div className="flex flex-col gap-4">
      <div role="tablist" aria-label={t('account.wallet.tab_transactions')} className="flex items-center gap-1 border-b border-border-hairline">
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'topup'}
          className={`h-10 rounded-t-lg px-4 text-sm font-medium ${
            tab === 'topup'
              ? 'border-b-2 border-brand text-brand'
              : 'text-ink-2 hover:text-ink-1'
          }`}
          onClick={() => {
            onSearchChange({ ...search, tab: undefined, p: 1 })
          }}
        >
          {t('account.wallet.tab_topup')}
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'transactions'}
          className={`h-10 rounded-t-lg px-4 text-sm font-medium ${
            tab === 'transactions'
              ? 'border-b-2 border-brand text-brand'
              : 'text-ink-2 hover:text-ink-1'
          }`}
          onClick={() => {
            onSearchChange({ ...search, tab: 'transactions', p: 1 })
          }}
        >
          {t('account.wallet.tab_transactions')}
        </button>
      </div>

      {tab === 'transactions' ? (
        <TransactionsSection search={search} onSearchChange={onSearchChange} />
      ) : (
        <>
          <TopupPage />
          <SubscriptionSection />
        </>
      )}
    </div>
  )
}
