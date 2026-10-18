import type { QueryClient } from '@tanstack/react-query'
import { createRouter } from '@tanstack/react-router'

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
