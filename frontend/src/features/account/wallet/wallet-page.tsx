import { useState } from 'react'

import { SubscriptionSection } from '@/features/account/subscription/subscription-section'
import { TopupPage, type TopupSelection } from '@/features/account/topup/topup-page'
import { PageHeader } from '@/features/shell/page-header'
import { useI18n } from '@/shared/lib/i18n-context'
import { handleTabListKeyDown } from '@/shared/lib/roving-focus'
import { Button } from '@/shared/ui/button'

import type { WalletTransactionsSearch } from './search'
import { WALLET_TABS, walletTabOf, type WalletTabId } from './search'
import { TransactionsSection } from './transactions-section'
import { WalletOverview } from './wallet-overview'

export interface WalletPageProps {
  search: WalletTransactionsSearch
  onSearchChange: (next: WalletTransactionsSearch) => void
}

const TAB_BASE =
  '-mb-px h-9 rounded-t-lg px-3.5 text-[13px] font-medium transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'
const TAB_SELECTED = 'border-b-2 border-brand font-semibold text-brand'
const TAB_IDLE = 'text-ink-2 hover:text-ink-1'

/**
 * `/wallet` 的页面级 Tab（R-M3-01）。
 *
 * 默认 `overview`：余额、消费趋势与最近流水。`topup` / `subscription` /
 * `transactions` 各自保留原有页面，四者互斥挂载——切走时不会触发对方的
 * 额外查询。URL 只序列化非默认 Tab，默认视图保持干净链接。
 */
export function WalletPage({ search, onSearchChange }: WalletPageProps) {
  const { t } = useI18n()
  const tab: WalletTabId = walletTabOf(search)
  // The overview is unmounted while the top-up tab is open, so the selection
  // it hands over must live here. `null` means "let the top-up tab seed from
  // the server config" (the plain header/tab entry points).
  const [topupSelection, setTopupSelection] = useState<TopupSelection | null>(
    null,
  )

  function select(next: WalletTabId): void {
    setTopupSelection(null)
    onSearchChange({
      ...search,
      tab: next === 'overview' ? undefined : next,
      p: 1,
    })
  }

  /** Opens the top-up tab, carrying the amount the user picked in the overview. */
  function startTopup(selection?: TopupSelection): void {
    setTopupSelection(selection ?? null)
    onSearchChange({ ...search, tab: 'topup', p: 1 })
  }

  return (
    <div className="mx-auto flex w-full max-w-[1520px] flex-col">
      <PageHeader
        crumbs={[
          { label: t('common.console.label') },
          { label: t('account.wallet.overview.title') },
        ]}
        title={t('account.wallet.overview.title')}
        description={t('account.wallet.overview.subtitle')}
        actions={
          <>
            <Button
              variant="secondary"
              className="h-8 px-3 text-xs"
              onClick={() => select('topup')}
            >
              {t('account.wallet.overview.action_redeem')}
            </Button>
            <a
              href="/orders"
              className="inline-flex h-8 items-center justify-center rounded-lg border border-border-default bg-surface-card px-3 text-xs font-medium text-ink-1 transition-colors hover:bg-surface-hover"
            >
              {t('account.wallet.overview.action_orders')}
            </a>
            <Button className="h-8 px-3 text-xs" onClick={() => startTopup()}>
              {t('account.wallet.overview.action_topup')}
            </Button>
          </>
        }
      />

      <div
        role="tablist"
        aria-label={t('account.wallet.tabs_label')}
        onKeyDown={handleTabListKeyDown}
        className="mt-4 flex gap-0.5 border-b border-border-hairline"
      >
        {WALLET_TABS.map((item) => {
          const selected = tab === item
          return (
            <button
              key={item}
              type="button"
              role="tab"
              id={`wallet-tab-${item}`}
              aria-selected={selected}
              // Only the selected panel is mounted, so inactive tabs must not
              // point at ids that do not exist in the DOM.
              aria-controls={selected ? `wallet-panel-${item}` : undefined}
              tabIndex={selected ? 0 : -1}
              onClick={() => {
                if (item === 'topup') {
                  startTopup()
                } else {
                  select(item)
                }
              }}
              className={[TAB_BASE, selected ? TAB_SELECTED : TAB_IDLE].join(
                ' ',
              )}
            >
              {t(`account.wallet.tab_${item}`)}
            </button>
          )
        })}
      </div>

      <div
        id={`wallet-panel-${tab}`}
        role="tabpanel"
        aria-labelledby={`wallet-tab-${tab}`}
        className="mt-4"
      >
        {tab === 'overview' ? (
          <WalletOverview onSelectTab={select} onTopup={startTopup} />
        ) : null}
        {tab === 'topup' ? (
          <TopupPage initialSelection={topupSelection ?? undefined} />
        ) : null}
        {tab === 'subscription' ? <SubscriptionSection /> : null}
        {tab === 'transactions' ? (
          <TransactionsSection
            search={search}
            onSearchChange={onSearchChange}
          />
        ) : null}
      </div>
    </div>
  )
}
