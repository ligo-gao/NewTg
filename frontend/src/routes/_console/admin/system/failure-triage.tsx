import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { FailureTriagePage } from '@/features/admin/failure-triage/failure-triage-page'
import { fetchMe } from '@/lib/session'

/**
 * 失败排障页（UI 失败排障，Task 6a 接线）。
 *
 * 只读页，权限 `logs:read`（不复用 `settings:manage`）。admin 角色由父级
 * `_console/admin.tsx` 统一保证，这里只补权限位：缺少权限时入口不渲染，
 * 直链在 beforeLoad 落 `/403`；服务端仍逐请求强制鉴权。
 *
 * 页面 UI 由 Task 6b 提供，当前 `FailureTriagePage` 是占位。
 */
export const Route = createFileRoute('/_console/admin/system/failure-triage')({
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    const canRead =
      me?.permissions?.some(
        (permission) => permission === '*' || permission === 'logs:read',
      ) ?? false
    if (!canRead) {
      throw redirect({
        to: '/403',
        search: { redirect: `${location.pathname}${location.searchStr}` },
      })
    }
  },
  loader: () => loadRouteDictionary('admin'),
  component: FailureTriagePage,
})
