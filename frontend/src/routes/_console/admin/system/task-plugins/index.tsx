import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { TaskPluginsPage } from '@/features/admin/plugins/task-plugins-page'
import {
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
 * 权限：读 admin（由 `_console/admin` 守卫），写 settings:manage 在页面内
 * 收窄（R-M6D-02）。非 admin 直链由角色守卫送 `/403`；服务端逐请求强制。
 */
export const Route = createFileRoute('/_console/admin/system/task-plugins/')({
  validateSearch: (search: Record<string, unknown>): TaskPluginsSearch => ({
    tab: normalizeTaskPluginsTab(search.tab),
  }),
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
