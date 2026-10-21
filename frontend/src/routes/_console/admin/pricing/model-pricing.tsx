import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { ModelPricingListPage } from '@/features/admin/pricing/ModelPricingListPage'

/**
 * 模型定价列表（F-06-13）。
 *
 * `?plugin=<key>` 是任务插件页「定价」Tab（F-08-37）的深链参数：只作为
 * 前端过滤条件消费，不进入任何后端查询（R-50）。非法/空值按「无过滤」
 * 处理并从地址栏剔除，保持分享链接可读。
 */
export const Route = createFileRoute('/_console/admin/pricing/model-pricing')({
  validateSearch: (search: Record<string, unknown>): { plugin?: string } => {
    const raw = search.plugin
    const plugin = typeof raw === 'string' ? raw.trim().slice(0, 30) : ''
    return plugin === '' ? {} : { plugin }
  },
  loader: () => loadRouteDictionary('admin'),
  component: ModelPricingRoute,
})

function ModelPricingRoute() {
  const { plugin } = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <ModelPricingListPage
      plugin={plugin}
      onClearPlugin={() => {
        void navigate({ search: {}, replace: true })
      }}
    />
  )
}
