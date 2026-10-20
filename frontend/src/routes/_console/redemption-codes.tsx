import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { RedemptionCodesPage } from '@/features/admin/redemption-codes/RedemptionCodesPage'
import {
  hasRedemptionManage,
  normalizeRedemptionSearch,
} from '@/features/admin/redemption-codes/redemption-search'
import { fetchMe } from '@/lib/session'

/**
 * 兑换码管理（F-04-15/16，`/redemption-codes`）。
 *
 * web-00 §4 路由表把这一页冻结为**不带 `/admin` 前缀**的管理专属 URL
 * （modules-04 的既有约定），因此它直接挂在 `_console` 下，必须自己施加
 * 两层守卫：会话层由父级 `_console` 完成，这里补 admin 角色与
 * `redemption:manage` 权限。缺少任一条件都落 `/403`，与服务端逐请求
 * RBAC 的拒绝结果一致（前端守卫只是 UX）。
 */
export const Route = createFileRoute('/_console/redemption-codes')({
  validateSearch: normalizeRedemptionSearch,
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (me === null) {
      throw redirect({ to: '/sign-in', search: { redirect: location.href } })
    }
    if (me.role !== 'admin' || !hasRedemptionManage(me.permissions)) {
      throw redirect({
        to: '/403',
        search: { redirect: `${location.pathname}${location.searchStr}` },
      })
    }
  },
  loader: () => loadRouteDictionary('admin'),
  component: RedemptionCodesRoute,
})

function RedemptionCodesRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <RedemptionCodesPage
      search={search}
      onSearchChange={(next, options) => {
        // 筛选/翻页走 replace：它们不是「新页面」，反复查询不应堆满历史栈，
        // 否则浏览器后退要按很多次才离开这一页。
        void navigate({
          search: next,
          replace: options?.replace ?? true,
        })
      }}
    />
  )
}
