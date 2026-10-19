import type { QueryClient } from '@tanstack/react-query'
import { createRouter } from '@tanstack/react-router'

import { ErrorFallback } from '@/app/error-boundary'
import { NotFoundPage } from '@/app/not-found-page'
import { setAuthFailureHandlers } from '@/app/providers/auth-failures'
import { appQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'

/** Context every route guard receives (web-00 §9.3). */
export interface RouterContext {
  queryClient: QueryClient
}

export const router = createRouter({
  routeTree,
  context: { queryClient: appQueryClient },
  defaultPreload: 'intent',
  scrollRestoration: true,
  /**
   * web-00 §8.3 的统一映射：任何未自带 `errorComponent` 的路由抛错都落到
   * 500 页（`ErrorFallback` 从错误对象携带 request_id），未匹配路由落到
   * `/404` 的同一组件。`__root` 显式声明了同样的两个组件；这里兜住未来新增
   * 的路由，避免它们悄悄退回 React Router 的默认英文错误屏。
   */
  defaultErrorComponent: ({ error, reset }) => (
    <ErrorFallback error={error} reset={reset} />
  ),
  defaultNotFoundComponent: NotFoundPage,
})

/**
 * Runtime 401 landing (web-00 §9.6). SPA navigation keeps the origin path in
 * `?redirect=` so the `/401` page's sign-in button can return the user after
 * re-authentication. The forced-password-change case keeps the hard-navigation
 * default from `auth-failures`.
 */
setAuthFailureHandlers({
  unauthorized: (path) => {
    void router.navigate({ to: '/401', search: { redirect: path } })
  },
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
