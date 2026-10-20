import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { AnnouncementsPage } from '@/features/admin/announcements/announcements-page'
import {
  parseAnnouncementsSearch,
  type AnnouncementsSearch,
} from '@/features/admin/announcements/search'
import { fetchMe } from '@/lib/session'

/**
 * 公告管理独立列表页（modules-08 UI §6.1.1，F-08-04）。
 *
 * 不进 Settings Shell：站点设置页只提供入口按钮。读与写共用
 * `settings:manage`（与站点设置同源，不新增公告专用键）；缺少权限时
 * 直链在 beforeLoad 落 `/403`，服务端仍逐请求强制鉴权。
 */
export const Route = createFileRoute('/_console/admin/settings/announcements')({
  validateSearch: (search: Record<string, unknown>): AnnouncementsSearch =>
    parseAnnouncementsSearch(search),
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    const canManage =
      me?.permissions?.some(
        (permission) => permission === '*' || permission === 'settings:manage',
      ) ?? false
    if (!canManage) {
      throw redirect({
        to: '/403',
        search: { redirect: `${location.pathname}${location.searchStr}` },
      })
    }
  },
  loader: () => loadRouteDictionary('admin'),
  component: AnnouncementsRoute,
})

function AnnouncementsRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <AnnouncementsPage
      search={search}
      onSearchChange={(patch) => {
        void navigate({
          search: (previous: AnnouncementsSearch) => ({ ...previous, ...patch }),
          replace: true,
        })
      }}
    />
  )
}
