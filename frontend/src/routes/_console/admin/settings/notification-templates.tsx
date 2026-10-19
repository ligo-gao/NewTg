import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { NotificationTemplatesPage } from '@/features/admin/notifications/notification-templates-page'
import { parseNotificationTemplatesSearch, type NotificationTemplatesSearch } from '@/features/admin/notifications/search'

/**
 * 通知模板独立列表页（UI §6.9、F-08-24～F-08-27）。
 *
 * 路由只做 URL search 的归一与写回；页面本身不依赖 RouterProvider，便于
 * 单元测试直接渲染。父级 `_console/admin` 守卫已保证只有 admin 能到达这里，
 * 写操作再按 settings:manage 在页面内收窄。
 */
export const Route = createFileRoute('/_console/admin/settings/notification-templates')({
  validateSearch: (search: Record<string, unknown>) =>
    parseNotificationTemplatesSearch(search),
  loader: () => loadRouteDictionary('settings'),
  component: NotificationTemplatesRoute,
})

function NotificationTemplatesRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <NotificationTemplatesPage
      search={search}
      onSearchChange={(patch) => {
        void navigate({
          search: (previous: NotificationTemplatesSearch) => ({ ...previous, ...patch }),
          replace: true,
        })
      }}
    />
  )
}
