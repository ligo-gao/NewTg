import { createFileRoute, redirect, useNavigate } from '@tanstack/react-router'
import { useEffect } from 'react'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PublicLayout } from '@/features/public/public-layout'
import { rankingsAccessRedirect } from '@/features/public/rankings/guard'
import {
  needsRankingsUrlRepair,
  validateRankingsSearch,
  type RankingsSearch,
} from '@/features/public/rankings/params'
import { RankingsPage } from '@/features/public/rankings/page'

function needsCanonicalisation(): boolean {
  if (typeof window === 'undefined') return false
  const pathname = window.location.pathname.replace(/\/+$/, '')
  if (!pathname.endsWith('/rankings')) return false
  return needsRankingsUrlRepair(window.location.search)
}

/**
 * `/rankings`（F-09-29）。`validateSearch` 把非法 period 静默回落 week，
 * 首次进入/非法值时用 replace 写回地址栏，保证分享链接始终合法。
 */
export const Route = createFileRoute('/rankings/')({
  validateSearch: validateRankingsSearch,
  loader: () => loadRouteDictionary('public'),
  beforeLoad: async ({ context, location }) => {
    const target = await rankingsAccessRedirect(
      context.queryClient,
      location.pathname,
      location.searchStr,
    )
    if (target !== null) throw redirect({ href: target })
  },
  component: RankingsRoute,
})

function RankingsRoute() {
  const search = Route.useSearch()
  const navigate = useNavigate()

  const changePeriod = (period: RankingsSearch['period']) => {
    void navigate({ to: '/rankings', search: { period }, replace: false })
  }

  useEffect(() => {
    if (needsCanonicalisation()) {
      void navigate({
        to: '/rankings',
        search: { period: search.period },
        replace: true,
      })
    }
    // 只在首次挂载时规范化；后续切换由 changePeriod 写入合法值。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <PublicLayout activePath="/rankings">
      <RankingsPage period={search.period} onPeriodChange={changePeriod} />
    </PublicLayout>
  )
}
