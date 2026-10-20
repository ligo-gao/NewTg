import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SubscriptionPlansPage } from '@/features/admin/subscriptions/subscription-plans-page'
import { fetchMe } from '@/lib/session'

/**
 * 订阅套餐管理页（billing-08 §16.2 F-H-01/02）。
 *
 * 独立页，不进 Settings Shell。路由登记为 `/_console/admin/subscriptions`
 * （实际 URL `/admin/subscriptions`）；billing-08 原文写作 `/subscriptions`，
 * 这里按管理页统一挂到 `_console/admin` 角色守卫下（与性能/部署/插件页同
 * 一口径），避免与用户侧订阅入口混淆。
 *
 * 读写都复用 `settings:manage`（后端 handlers_subscriptions 的权限口径）：
 * 缺少权限时入口不渲染，直链在 beforeLoad 落 `/403`；服务端仍逐请求强制。
 */
export const Route = createFileRoute('/_console/admin/subscriptions')({
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
  component: SubscriptionPlansPage,
})
