import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { DocsPage } from '@/features/public/docs-page'
import { PublicLayout } from '@/features/public/public-layout'

export const Route = createFileRoute('/docs')({
  loader: () => loadRouteDictionary('public'),
  component: DocsRoute,
})

// 页面组件必须留在模块内部（非 export）：TanStack Router 不会拆分导出的
// 路由组件，PublicLayout 的 home.css 会跟着路由表回到首屏包。纯函数与测试
// 直接使用 `features/public/docs-page`，路由文件不再导出它们。
function DocsRoute() {
  return (
    <PublicLayout activePath="/docs">
      <DocsPage />
    </PublicLayout>
  )
}
