import { createFileRoute } from '@tanstack/react-router'
import { useEffect } from 'react'

import { loadRouteDictionary } from '@/app/providers/i18n'
import {
  validateWalletSearch,
  walletTabOf,
  type WalletTransactionsSearch,
} from '@/features/account/wallet/search'
import { WalletPage } from '@/features/account/wallet/wallet-page'

export const Route = createFileRoute('/_console/wallet')({
  // R-M3-01：默认 Tab 是 `overview`，干净链接不携带 tab；只有显式选择的
  // `topup` / `subscription` / `transactions` 留在 URL。`validateSearch`
  // 负责丢弃非法值，挂载后本路由再把结果写回地址栏（见 WalletRoute）。
  validateSearch: validateWalletSearch,
  loader: () => loadRouteDictionary('account'),
  component: WalletRoute,
})

/**
 * tab 存在但不是三个可序列化的值（含显式 `?tab=overview` 与未知值）。
 *
 * TanStack Router 会把原始 search 合并进 `useSearch`（strict 未开启），
 * 所以手改的非法 tab 会绕过 `validateSearch` 留在地址栏；这类值需要被
 * 归一化后 replace 写回。
 */
function isNonCanonicalTab(search: WalletTransactionsSearch): boolean {
  return search.tab !== undefined && walletTabOf(search) === 'overview'
}

function WalletRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // 与 pricing 路由的口径一致：把 `validateSearch` 的规范化结果 replace
  // 回地址栏，URL 与视图保持一致且不新增历史。写回后 tab 已缺失，
  // 条件不再成立，因此不会循环。
  useEffect(() => {
    if (isNonCanonicalTab(search)) {
      void navigate({ search: validateWalletSearch(search), replace: true })
    }
  }, [search, navigate])

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
