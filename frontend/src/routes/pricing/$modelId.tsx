import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PublicLayout } from '@/features/public/public-layout'
import { PricingDetailPage } from '@/features/public/pricing/detail-page'
import { pricingAccessRedirect } from '@/features/public/pricing/guard'

/**
 * 模型详情。与列表页是**兄弟路由**（目录式命名），不存在 `/pricing` 父
 * 布局；因此列表页的筛选状态不会成为详情的壳，详情也不会继承列表的
 * 工具栏与侧栏。
 */
export const Route = createFileRoute('/pricing/$modelId')({
  loader: () => loadRouteDictionary('public'),
  beforeLoad: async ({ context, location }) => {
    const target = await pricingAccessRedirect(
      context.queryClient,
      location.pathname,
      location.searchStr,
    )
    if (target !== null) throw redirect({ href: target })
  },
  component: PricingDetailRoute,
})

function PricingDetailRoute() {
  const { modelId } = Route.useParams()
  return (
    <PublicLayout activePath={`/pricing/${modelId}`}>
      <PricingDetailPage modelId={modelId} />
    </PublicLayout>
  )
}
