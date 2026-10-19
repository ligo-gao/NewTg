import { createFileRoute, redirect, useNavigate } from '@tanstack/react-router'
import { useEffect } from 'react'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PublicLayout } from '@/features/public/public-layout'
import { pricingAccessRedirect } from '@/features/public/pricing/guard'
import {
  validatePricingSearch,
  type PricingSearch,
} from '@/features/public/pricing/params'
import { PricingPage } from '@/features/public/pricing/pricing-page'

const SORTS = new Set(['name', 'price_asc', 'price_desc'])

/**
 * 首屏 URL 里是否存在会被静默降级的参数（F-09-22 要求修正地址栏）。
 * facet 取值是否存在要等接口返回，由页面在响应后修正；这里只处理
 * 纯语法层（枚举/数值/长度）。
 */
function needsCanonicalisation(): boolean {
  if (typeof window === 'undefined') return false
  const params = new URLSearchParams(window.location.search)
  const sort = params.get('sort')
  if (sort !== null && !SORTS.has(sort)) return true
  const view = params.get('view')
  if (view !== null && view !== 'card' && view !== 'table') return true
  const quota = params.get('quotaType')
  if (quota !== null && quota !== 'all' && quota !== 'token' && quota !== 'request') {
    return true
  }
  const unit = params.get('tokenUnit')
  if (unit !== null && unit !== 'M' && unit !== 'K') return true
  const page = params.get('p')
  if (page !== null && (!/^\d+$/.test(page) || Number(page) < 1)) return true
  const size = params.get('page_size')
  if (size !== null && !/^\d+$/.test(size)) return true
  const search = params.get('search')
  if (search !== null && search.length > 128) return true
  return false
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
    if (needsCanonicalisation()) change(search, { replace: true })
    // 只在首次挂载时检查一次；后续导航由本组件的 change 写入合法值。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <PublicLayout activePath="/pricing">
      <PricingPage search={search} onSearchChange={change} />
    </PublicLayout>
  )
}
