import { createFileRoute, redirect } from '@tanstack/react-router'

import { fetchMe } from '@/lib/session'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { TaskPluginsPage } from '@/features/admin/plugins/task-plugins-page'
import {
  hasSettingsManage,
  normalizeTaskPluginsTab,
  type TaskPluginsTabId,
} from '@/features/admin/plugins/tabs'

export interface TaskPluginsSearch {
  tab: TaskPluginsTabId
}

/**
 * 任务插件页（UI §6.10）。`?tab=installed|upload|marketplace|sandbox|pricing|
 * runtime`，默认 installed。
 *
 * 权限：后端 R-M6D-16 把读写统一到 settings:manage。缺少该权限时入口不
 * 渲染，直链在 beforeLoad 落 `/403`；服务端仍逐请求强制。
 */
export const Route = createFileRoute('/_console/admin/system/task-plugins/')({
  validateSearch: (search: Record<string, unknown>): TaskPluginsSearch => ({
    tab: normalizeTaskPluginsTab(search.tab),
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
  component: TaskPluginsRoute,
})

function TaskPluginsRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <TaskPluginsPage
      tab={search.tab}
      onTabChange={(tab) => {
        void navigate({ search: { tab }, replace: true })
      }}
    />
  )
}
