import { createFileRoute, redirect, useNavigate } from '@tanstack/react-router'
import { useEffect } from 'react'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PublicLayout } from '@/features/public/public-layout'
import { pricingAccessRedirect } from '@/features/public/pricing/guard'
import {
  needsPricingUrlRepair,
  validatePricingSearch,
  type PricingSearch,
} from '@/features/public/pricing/params'
import { PricingPage } from '@/features/public/pricing/pricing-page'

/**
 * 首屏 URL 是否需要规范化（F-09-22 口径 2）。memory-router 测试里
 * `window.location` 不属于该路由，直接跳过，避免误写地址栏。
 */
function needsCanonicalisation(search: PricingSearch): boolean {
  if (typeof window === 'undefined') return false
  const pathname = window.location.pathname.replace(/\/+$/, '')
  if (!pathname.endsWith('/pricing')) return false
  return needsPricingUrlRepair(window.location.search, search)
}

/**
 * 定价公示页。`validateSearch` 同时承担 URL 参数降级（F-09-22）：页面
 * 拿到的永远是合法的 `PricingSearch`。
 */
export const Route = createFileRoute('/pricing/')({
  validateSearch: validatePricingSearch,
  loader: () => loadRouteDictionary('public'),
  beforeLoad: async ({ context, location }) => {
    const target = await pricingAccessRedirect(
      context.queryClient,
      location.pathname,
      location.searchStr,
    )
    if (target !== null) throw redirect({ href: target })
  },
  component: PricingRoute,
})

function PricingRoute() {
  const search = Route.useSearch()
  const navigate = useNavigate()

  const change = (
    next: PricingSearch,
    options?: { replace?: boolean },
  ) => {
    void navigate({
      to: '/pricing',
      search: next,
      // 参数修正与打字搜索不污染历史；翻页显式传 false（F-09-21）。
      replace: options?.replace ?? true,
    })
  }

  // 非法枚举/数值已在 validateSearch 里降级；这里把同一结果写回地址栏，
  // 用 replace 不新增历史（F-09-22 口径 2）。
  useEffect(() => {
    if (needsCanonicalisation(search)) change(search, { replace: true })
    // 只在首次挂载时检查一次；后续导航由本组件的 change 写入合法值。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <PublicLayout activePath="/pricing">
      <PricingPage search={search} onSearchChange={change} />
    </PublicLayout>
  )
}
