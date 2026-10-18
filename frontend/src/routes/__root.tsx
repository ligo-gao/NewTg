import { createRootRouteWithContext, Outlet } from '@tanstack/react-router'

import { ErrorFallback } from '@/app/error-boundary'
import { NotFoundPage } from '@/app/not-found-page'
import type { RouterContext } from '@/app/router'

function RootLayout() {
  return (
    <>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:left-3 focus:top-3 focus:z-50 focus:rounded-lg focus:bg-surface-card focus:px-3 focus:py-2 focus:text-sm focus:text-ink-1 focus:shadow-[var(--focus-ring)]"
      >
        Skip to content
      </a>
      <Outlet />
    </>
  )
}

/**
 * Root layer of the web-00 §9.3 guard chain. Its only own gate is the
 * initialization check (`system_initialized=false` → `/setup`), which is
 * deferred until the public-status contract carries the field; the session
 * and role gates live on `_console` / `_console/admin` because public routes
 * must not block on `/api/me` (§9.2).
 *
 * `createRootRouteWithContext` is what makes `context.queryClient` visible to
 * every `beforeLoad`; the router supplies the shared client in `app/router`.
 */
export const Route = createRootRouteWithContext<RouterContext>()({
  component: RootLayout,
  errorComponent: ({ error, reset }) => (
    <ErrorFallback error={error} reset={reset} />
  ),
  // Same body as the `/404` route: render the shared page instead of keeping a
  // second inline copy that could drift from `_errors/404.tsx`.
  notFoundComponent: NotFoundPage,
})
