import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { ModelCatalogPage } from '@/features/admin/models/ModelCatalogPage'
import {
  canReadCatalog,
  normalizeModelCatalogSearch,
} from '@/features/admin/models/model-catalog-search'
import { fetchMe } from '@/lib/session'

/**
 * 模型与供应商目录（web-00 §4.3.4：`/admin/models`，F-06-22～26）。
 *
 * 访问要求是 `channels:read`（写面 `channels:manage`，批量/同步/供应商
 * 操作 `channels:manage_sensitive`）。角色层由父级 `_console/admin`
 * 守卫；这里补权限层，缺权限时连同搜索串一起把用户送到 `/403`。
 * 前端守卫只是 UX，服务端仍逐请求强制（web-00 §9.1）。
 */
export const Route = createFileRoute('/_console/admin/models')({
  validateSearch: normalizeModelCatalogSearch,
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (!canReadCatalog(me?.permissions)) {
      throw redirect({
        to: '/403',
        search: { redirect: `${location.pathname}${location.searchStr}` },
      })
    }
  },
  loader: () => loadRouteDictionary('admin'),
  component: ModelCatalogRoute,
})

function ModelCatalogRoute() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <ModelCatalogPage
      search={search}
      onSearchChange={(next, options) => {
        // 筛选/翻页/Tab 切换都走 replace：它们不是「新页面」，反复查询
        // 不应堆满历史栈。
        void navigate({
          search: next,
          replace: options?.replace ?? true,
        })
      }}
    />
  )
}
