import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { UpstreamReconciliationPage } from '@/features/admin/upstream-reconciliation/reconciliation-page'
import {
  hasSettingsManage,
  normalizeReconciliationTab,
  type ReconciliationTabId,
} from '@/features/admin/upstream-reconciliation/tabs'
import { fetchMe } from '@/lib/session'

export interface UpstreamReconciliationSearch {
  tab: ReconciliationTabId
}

/**
 * 上游用量对账页。`?tab=entry|results|overview|accounts`，缺省 overview。
 *
 * 权限：读写复用 `settings:manage`（不新增运维权限键）。缺少权限时入口
 * 不渲染（nav-config），直链在 beforeLoad 落 `/403`；服务端仍逐请求强制。
 */
export const Route = createFileRoute(
  '/_console/admin/upstream-reconciliation',
)({
  validateSearch: (
    search: Record<string, unknown>,
  ): UpstreamReconciliationSearch => ({
    tab: normalizeReconciliationTab(search.tab),
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
  component: UpstreamReconciliationPage,
})
