import { createFileRoute, redirect, useNavigate } from '@tanstack/react-router'
import { useEffect } from 'react'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PublicLayout } from '@/features/public/public-layout'
import { modelsAccessRedirect } from '@/features/public/models/guard'
import {
  needsModelsUrlRepair,
  validateModelsSearch,
  type ModelsSearch,
} from '@/features/public/models/params'
import { ModelsPage } from '@/features/public/models/page'

/**
 * 首屏 URL 是否需要规范化。memory-router 测试里 `window.location` 不属于
 * 该路由，直接跳过，避免误写地址栏。
 */
function needsCanonicalisation(search: ModelsSearch): boolean {
  if (typeof window === 'undefined') return false
  const pathname = window.location.pathname.replace(/\/+$/, '')
  if (!pathname.endsWith('/models')) return false
  return needsModelsUrlRepair(window.location.search, search)
}

/**
 * `/models` 模型库。`validateSearch` 同时承担 URL 参数降级，页面拿到的
 * 永远是合法的 `ModelsSearch`；非法值在挂载时用 replace 写回。
 */
export const Route = createFileRoute('/models/')({
  validateSearch: validateModelsSearch,
  loader: () => loadRouteDictionary('public'),
  beforeLoad: async ({ context, location }) => {
    const target = await modelsAccessRedirect(
      context.queryClient,
      location.pathname,
      location.searchStr,
    )
    if (target !== null) throw redirect({ href: target })
  },
  component: ModelsRoute,
})

function ModelsRoute() {
  const search = Route.useSearch()
  const navigate = useNavigate()

  const change = (next: ModelsSearch, replace = false) => {
    void navigate({ to: '/models', search: next, replace })
  }

  // 非法枚举已在 validateSearch 里降级；这里把同一结果写回地址栏，
  // 用 replace 不新增历史。只在首次挂载时检查一次。
  useEffect(() => {
    if (needsCanonicalisation(search)) change(search, true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <PublicLayout activePath="/models">
      <ModelsPage search={search} onSearchChange={change} />
    </PublicLayout>
  )
}
