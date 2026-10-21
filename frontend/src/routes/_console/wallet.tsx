import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import {
  validateWalletSearch,
  type WalletTransactionsSearch,
} from '@/features/account/wallet/search'
import { WalletPage } from '@/features/account/wallet/wallet-page'

export const Route = createFileRoute('/_console/wallet')({
  // R-M3-01：`?tab=transactions` 是流水页的唯一 URL 形状；缺省/非法值
  // 归一为 `topup`，默认视图不携带多余参数。
  validateSearch: validateWalletSearch,
  loader: () => loadRouteDictionary('account'),
  component: WalletRoute,
})

function WalletRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <WalletPage
      search={search as WalletTransactionsSearch}
      onSearchChange={(next) => {
        // Replace 让筛选输入不堆满浏览器历史，同时保持链接可刷新/分享。
        void navigate({ search: next, replace: true })
      }}
    />
  )
}
