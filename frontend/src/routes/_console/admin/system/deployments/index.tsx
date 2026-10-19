import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { ModelDeploymentsPage } from '@/features/admin/deployments/deployments-page'
import {
  hasSettingsManage,
  normalizeDeploymentsTab,
  type DeploymentsTabId,
} from '@/features/admin/deployments/tabs'
import { fetchMe } from '@/lib/session'

export interface DeploymentsSearch {
  tab: DeploymentsTabId
}

/**
 * 模型部署页（UI §6.13）。`?tab=deployments|resources|settings`，默认
 * deployments。
 *
 * 权限：全部能力复用 `settings:manage`（§3.20.6）。缺少权限时**直链**落
 * `/403`，与入口不渲染保持一致；服务端仍对每个请求做 RBAC 强制。
 */
export const Route = createFileRoute('/_console/admin/system/deployments/')({
  validateSearch: (search: Record<string, unknown>): DeploymentsSearch => ({
    tab: normalizeDeploymentsTab(search.tab),
  }),
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (!hasSettingsManage(me?.permissions)) {
      throw redirect({
        to: '/403',
        search: { redirect: `${location.pathname}${location.searchStr}` },
      })
    }
  },
  loader: () => loadRouteDictionary('admin'),
  component: DeploymentsRoute,
})

function DeploymentsRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <ModelDeploymentsPage
      tab={search.tab}
      onTabChange={(tab) => {
        void navigate({ search: { tab }, replace: true })
      }}
    />
  )
}
