import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PerformancePage } from '@/features/admin/performance/performance-page'
import { fetchMe } from '@/lib/session'

/**
 * 性能管理页（UI §6.12，F-08-48～F-08-50）。
 *
 * 独立页，不进 Settings Shell。读统计与三个写操作共用 `settings:manage`
 * （modules-07，不新增运维权限键）：缺少权限时入口不渲染，直链在 beforeLoad
 * 落 `/403`；服务端仍逐请求强制鉴权。
 *
 * 这里直接内联判权而不复用插件页的 `hasSettingsManage`，避免路由 chunk 把
 * 整个插件特性模块拖进本页首屏。
 */
export const Route = createFileRoute('/_console/admin/system/performance')({
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
  component: PerformancePage,
})
